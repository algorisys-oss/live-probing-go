package ui

import (
	"cmp"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"liveprobe/internal/core"
	"liveprobe/internal/history"
	"liveprobe/internal/ui/view"
)

// Handlers + data shaping for the history-backed pages (alerts, errors, search, history, day,
// service, compare). Ports of ui/src/pages/*.tsx; templates are in pages.templ.

func headerFrom(st LiveState, traces int, active string) headerState {
	return headerState{Active: active, Traces: traces, Services: len(st.Topology.Nodes), Alerts: len(st.Alerts), AlertSeverity: alertSeverity(st.Alerts)}
}

// --- alerts --------------------------------------------------------------------------------

// firingFor is how long an alert has fired ("42s", "3m 7s", "2h 5m"); live.js keeps it ticking.
func firingFor(sinceMs, nowMs int64) string {
	s := int64(math.Max(0, math.Floor(float64(nowMs-sinceMs)/1000+0.5)))
	if s < 60 {
		return strconv.FormatInt(s, 10) + "s"
	}
	m := s / 60
	if m < 60 {
		return strconv.FormatInt(m, 10) + "m " + strconv.FormatInt(s%60, 10) + "s"
	}
	return strconv.FormatInt(m/60, 10) + "h " + strconv.FormatInt(m%60, 10) + "m"
}

type alertRow struct {
	Severity, Message, Metric, Since, Firing, ServiceHref, Service string
}

func alertRows(alerts []core.FiringAlert, nowMs int64) []alertRow {
	out := []alertRow{}
	for _, a := range alerts {
		r := alertRow{Severity: a.Severity, Message: a.Message, Metric: a.Metric, Since: strconv.FormatInt(a.Since, 10), Firing: firingFor(a.Since, nowMs)}
		// Error-rate alerts name a service (link to its page); latency alerts name an edge.
		if a.Kind == "service" && !view.IsDatastore(a.Target) {
			r.Service, r.ServiceHref = a.Target, serviceHref(a.Target)
		}
		out = append(out, r)
	}
	return out
}

func alertsPage(b Backend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := b.LiveState()
		render(w, r, alertsPageView(headerFrom(st, len(b.RecentTraces(maxListedTraces)), "alerts"), alertRows(st.Alerts, time.Now().UnixMilli())))
	}
}

// --- errors --------------------------------------------------------------------------------

// timeOfDay is a UTC "HH:MM:SS UTC" for epoch micros.
func timeOfDay(micros int64) string {
	ms := int64(math.Floor(float64(micros) / 1000))
	return time.UnixMilli(ms).UTC().Format("15:04:05") + " UTC"
}

func errorsPage(b Backend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := b.LiveState()
		groups, err := b.History().ErrorGroups(100)
		msg := ""
		if err != nil {
			msg = "Failed to load errors"
		}
		render(w, r, errorsPageView(headerFrom(st, len(b.RecentTraces(maxListedTraces)), "errors"), groups, msg))
	}
}

// --- search --------------------------------------------------------------------------------

type searchForm struct {
	Q, Service, Attr, Error, MinMs, MaxMs, MinSpans, Sort string
}

func (f searchForm) hasQuery() bool {
	return f.Q != "" || f.Service != "" || f.Attr != "" || f.Error != "" || f.MinMs != "" || f.MaxMs != "" || f.MinSpans != ""
}

// positive mirrors the API's posInt: a finite number > 0, else unset.
func positive(raw string) float64 {
	n := core.ParseJSNumber(raw)
	if math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 {
		return 0
	}
	return n
}

func searchPage(b Backend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f := searchForm{Q: q.Get("q"), Service: q.Get("service"), Attr: q.Get("attr"), Error: q.Get("error"),
			MinMs: q.Get("minMs"), MaxMs: q.Get("maxMs"), MinSpans: q.Get("minSpans"), Sort: q.Get("sort")}
		if f.Sort != "slowest" {
			f.Sort = "recent"
		}
		st := b.LiveState()
		var results []core.TraceSummary
		errMsg := ""
		if f.hasQuery() {
			o := history.SearchOptions{Q: f.Q, Service: f.Service, Attr: f.Attr, Sort: f.Sort, Limit: 200, // the React page asked for 200
				MinMicros: int64(positive(f.MinMs) * 1000), MaxMicros: int64(positive(f.MaxMs) * 1000), MinSpans: int(math.Ceil(positive(f.MinSpans)))}
			if f.Error != "" {
				v := f.Error == "true"
				o.Error = &v
			}
			var err error
			if results, err = b.History().Search(o); err != nil {
				errMsg = "Search failed"
			}
		}
		h := headerFrom(st, len(b.RecentTraces(maxListedTraces)), "search")
		h.Query = f.Q
		render(w, r, searchPageView(h, f, sortedCopy(st.Topology.Nodes), results, errMsg))
	}
}

// --- history + day -------------------------------------------------------------------------

func historyPage(b Backend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := b.LiveState()
		days, err := b.History().Days()
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		render(w, r, historyPageView(headerFrom(st, len(b.RecentTraces(maxListedTraces)), "history"), days, buildTrends(days), msg))
	}
}

type endpointRow struct {
	Href, Class, Operation, Calls, Errors, ErrClass, Avg, Max string
}

type dayData struct {
	Header     headerState
	Summary    history.DaySummary
	Err        string
	Endpoints  []endpointRow
	Throughput throughputChart
	Endpoint   string // selected endpoint (?endpoint=), "" = none
	Latency    latencyChart
	Histogram  histogramChart
	Heatmap    heatmapChart
	Flame      flamegraph
}

func dayPage(b Backend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		day := r.PathValue("date")
		st := b.LiveState()
		d := dayData{Header: headerFrom(st, len(b.RecentTraces(maxListedTraces)), "history"), Endpoint: r.URL.Query().Get("endpoint")}
		store := b.History()
		sum, err := store.DaySummary(day)
		if err != nil {
			d.Err = err.Error()
			render(w, r, dayPageView(d))
			return
		}
		d.Summary, d.Throughput = sum, buildThroughput(sum.Throughput)
		for _, e := range sum.TopEndpoints {
			row := endpointRow{
				Href: "/day/" + pathEscape(day) + "?endpoint=" + queryEscape(e.Operation) + "#latency", Class: "row-clickable",
				Operation: e.Operation, Calls: grouped(e.Calls), Errors: grouped(e.Errors), ErrClass: "num",
				Avg: view.Micros(e.AvgMicros), Max: view.Micros(e.MaxMicros),
			}
			if e.Operation == d.Endpoint {
				row.Class += " row-selected"
			}
			if e.Errors > 0 {
				row.ErrClass = "num error-text"
			}
			d.Endpoints = append(d.Endpoints, row)
		}
		if d.Endpoint != "" {
			if lat, err := store.EndpointLatency(day, d.Endpoint); err == nil {
				d.Latency = buildLatency(lat)
			} else {
				d.Latency = latencyChart{Empty: true}
			}
			dist, err := store.EndpointDistribution(day, d.Endpoint)
			if err != nil {
				dist = history.LatencyDistribution{}
			}
			d.Histogram, d.Heatmap = buildHistogram(dist.Histogram), buildHeatmap(dist.Heatmap)
			if flame, err := store.EndpointFlamegraph(day, d.Endpoint, 150); err == nil {
				d.Flame = buildFlamegraph(flame)
			} else {
				d.Flame = flamegraph{Empty: true}
			}
		}
		render(w, r, dayPageView(d))
	}
}

// --- service -------------------------------------------------------------------------------

func servicePage(b Backend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := b.LiveState()
		name := r.PathValue("name")
		render(w, r, servicePageView(headerFrom(st, len(b.RecentTraces(maxListedTraces)), ""), name, b.ServiceDetail(name)))
	}
}

// --- compare -------------------------------------------------------------------------------

type diffRow struct {
	Key, A, B, Delta, DeltaClass string
}

// compareDiff totals duration per "participant: operation" in each trace and orders by the
// largest absolute change (ties keep first-seen order: A's spans, then B's).
func compareDiff(a, b *core.TraceDetail) []diffRow {
	type acc struct{ a, b int64 }
	totals := map[string]*acc{}
	var order []string
	add := func(spans []core.SpanRow, isB bool) {
		for _, s := range spans {
			k := s.Participant + ": " + s.Operation
			t := totals[k]
			if t == nil {
				t = &acc{}
				totals[k] = t
				order = append(order, k)
			}
			if isB {
				t.b += s.Duration
			} else {
				t.a += s.Duration
			}
		}
	}
	add(a.Spans, false)
	add(b.Spans, true)
	slices.SortStableFunc(order, func(x, y string) int {
		dx, dy := totals[x].b-totals[x].a, totals[y].b-totals[y].a
		return cmp.Compare(abs64(dy), abs64(dx))
	})
	rows := make([]diffRow, 0, len(order))
	for _, k := range order {
		t := totals[k]
		delta := t.b - t.a
		r := diffRow{Key: k, A: "—", B: "—", Delta: "0", DeltaClass: "num muted"}
		if t.a != 0 {
			r.A = view.Micros(t.a)
		}
		if t.b != 0 {
			r.B = view.Micros(t.b)
		}
		switch {
		case delta > 0:
			r.Delta, r.DeltaClass = "+"+view.Micros(delta), "num err-text"
		case delta < 0:
			r.Delta, r.DeltaClass = "−"+view.Micros(-delta), "num good-text"
		}
		rows = append(rows, r)
	}
	return rows
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

type compareData struct {
	Header     headerState
	A, B       string
	DA, DB     *core.TraceDetail
	Err        string
	Diff       []diffRow
	Recent     []core.TraceSummary
	HasEither  bool
	MissingAny bool
}

func comparePage(b Backend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := b.LiveState()
		recent := b.RecentTraces(maxListedTraces)
		q := r.URL.Query()
		d := compareData{Header: headerFrom(st, len(recent), ""), A: strings.TrimSpace(q.Get("a")), B: strings.TrimSpace(q.Get("b")), Recent: recent}
		if d.A != "" {
			if d.DA, _ = b.TraceDetail(d.A); d.DA == nil {
				d.Err = "Trace A not found."
			}
		}
		if d.B != "" {
			if d.DB, _ = b.TraceDetail(d.B); d.DB == nil {
				d.Err = "Trace B not found."
			}
		}
		d.HasEither = d.DA != nil || d.DB != nil
		if d.DA != nil && d.DB != nil {
			d.Diff = compareDiff(d.DA, d.DB)
		}
		render(w, r, comparePageView(d))
	}
}
