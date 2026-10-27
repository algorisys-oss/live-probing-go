package ui

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"liveprobe/internal/core"
	"liveprobe/internal/history"
)

// fakeBackend serves a fixed live state and lets tests publish events.
type fakeBackend struct {
	details map[string]core.TraceDetail
	mu      sync.Mutex
	state   LiveState
	traces  []core.TraceSummary
	subs    []chan Event
}

func (f *fakeBackend) LiveState() LiveState { return f.state }
func (f *fakeBackend) RecentTraces(limit int) []core.TraceSummary {
	return f.traces[:min(limit, len(f.traces))]
}
func (f *fakeBackend) TraceDetail(id string) (*core.TraceDetail, error) {
	if d, ok := f.details[id]; ok {
		return &d, nil
	}
	return nil, nil
}
func (f *fakeBackend) ServiceDetail(name string) core.ServiceDetail {
	return core.ServiceDetail{Name: name}
}
func (f *fakeBackend) History() *history.Store { return nil }
func (f *fakeBackend) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 8)
	f.mu.Lock()
	f.subs = append(f.subs, ch)
	f.mu.Unlock()
	return ch, func() {}
}
func (f *fakeBackend) publish(ev Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ch := range f.subs {
		ch <- ev
	}
}
func (f *fakeBackend) subscribers() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs)
}

func fixtureBackend() *fakeBackend {
	topo := core.Topology{
		Nodes: []string{"gateway", "catalog", "postgresql", "order"},
		Edges: []core.Edge{
			{From: "gateway", To: "catalog", Calls: 100, AvgDurationMicros: 14_000},
			{From: "catalog", To: "postgresql", Calls: 400, AvgDurationMicros: 2_500},
			{From: "gateway", To: "order", Calls: 100, Errors: 14, AvgDurationMicros: 23_000},
		},
		Externals: []string{},
	}
	return &fakeBackend{
		state: LiveState{
			Topology: topo, MermaidFlow: core.ToMermaidFlow(topo),
			Red:    []core.RedMetric{{Service: "gateway", Calls: 100, RatePerSec: 6.4, P95Micros: 48_000}, {Service: "order", Calls: 100, Errors: 14, ErrorRate: 0.14, RatePerSec: 6.4, P95Micros: 23_000}},
			Alerts: []core.FiringAlert{{Alert: core.Alert{ID: "service:order:error-rate", Severity: "error"}, Since: 1}},
		},
		traces: []core.TraceSummary{
			{TraceID: "t2", RootOperation: "POST /checkout", Services: []string{"gateway", "order"}, SpanCount: 4, StartTime: 2_000, DurationMicros: 48_000, HasError: true},
			{TraceID: "t1", RootOperation: "GET /products", Services: []string{"gateway", "catalog"}, SpanCount: 3, StartTime: 1_000, DurationMicros: 14_000},
		},
	}
}

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

func mustContain(t *testing.T, body string, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if !strings.Contains(body, p) {
			t.Errorf("missing %q", p)
		}
	}
}

func TestLivePageServerRendered(t *testing.T) {
	code, body := get(t, Handler(fixtureBackend()), "/")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	mustContain(t, body,
		`data-stream="/ui/live?metric=calls&amp;page=live&amp;view=graph"`,
		`data-id="t2"`, `data-href="/trace/t2"`, "POST /checkout", "dot dot-error", // trace rows, newest first
		`red-tile red-tile-error`, "14%", "6.4/s", "48ms", // RED tiles
		`flow-node flow-node-service flow-node-error`, // order is failing → red node
		`flow-node flow-node-datastore`,               // postgresql pill
		`data-href="/service/catalog"`,                // services link; datastores don't
		`id="lp-alert-badge" class="nav-badge nav-badge-error"`, ">1<",
		"<strong id=\"lp-service-count\">4</strong>",
	)
	if strings.Contains(body, `data-href="/service/postgresql"`) {
		t.Error("datastores must not link to a service page")
	}
	if strings.Index(body, `data-id="t2"`) > strings.Index(body, `data-id="t1"`) {
		t.Error("rows should be newest first")
	}
}

func TestLivePageFilterAndMatrix(t *testing.T) {
	h := Handler(fixtureBackend())
	_, body := get(t, h, "/?service=catalog")
	mustContain(t, body, `<option value="catalog" selected>`, "clear ✕", `data-id="t1"`)
	if strings.Contains(body, `data-id="t2"`) {
		t.Error("filtered list should drop traces not touching catalog")
	}
	if strings.Contains(body, ">order</text>") {
		t.Error("filtered graph should only show catalog and its neighbours")
	}
	_, body = get(t, h, "/?view=matrix&metric=latency")
	mustContain(t, body, "depmatrix-svg", "Who calls whom", `seg-btn seg-btn-active" href="/?metric=latency&amp;view=matrix">latency`, ">2.5ms<")
}

func TestEmptyStates(t *testing.T) {
	_, body := get(t, Handler(&fakeBackend{state: LiveState{Topology: core.Topology{}}}), "/")
	mustContain(t, body, "No traces yet…", `id="lp-flow" hidden`)
}

func TestUnknownRouteRedirectsAndStaticServed(t *testing.T) {
	h := Handler(fixtureBackend())
	if code, _ := get(t, h, "/nope"); code != http.StatusFound {
		t.Errorf("unknown route status %d", code)
	}
	if code, body := get(t, h, "/static/live.js"); code != 200 || !strings.Contains(body, "EventSource") {
		t.Errorf("static asset: %d", code)
	}
}

// readEvents parses SSE frames into (event, data) pairs.
func readEvents(t *testing.T, r io.Reader, n int) []map[string]any {
	t.Helper()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	var out []map[string]any
	var event string
	for sc.Scan() && len(out) < n {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			var data map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data); err != nil {
				t.Fatal(err)
			}
			data["_event"] = event
			out = append(out, data)
		}
	}
	return out
}

func TestLiveStreamResetThenUpdates(t *testing.T) {
	b := fixtureBackend()
	srv := httptest.NewServer(Handler(b))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/ui/live?page=live&service=gateway", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("content-type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}

	go func() {
		for b.subscribers() == 0 {
			time.Sleep(time.Millisecond)
		}
		b.publish(Event{Traces: []core.TraceSummary{
			{TraceID: "t3", RootOperation: "GET /x", Services: []string{"gateway"}, StartTime: 3_000},
			{TraceID: "t4", RootOperation: "GET /y", Services: []string{"auth"}, StartTime: 4_000}, // filtered out
		}})
		st := b.state
		b.publish(Event{Live: &st})
	}()

	evs := readEvents(t, res.Body, 3)
	if len(evs) != 3 || evs[0]["_event"] != "reset" || evs[1]["_event"] != "traces" || evs[2]["_event"] != "live" {
		t.Fatalf("events: %v", evs)
	}
	if rows := evs[0]["rows"].([]any); len(rows) != 2 {
		t.Errorf("reset rows: %d", len(rows))
	}
	rows := evs[1]["rows"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["id"] != "t3" || evs[1]["count"] != float64(4) {
		t.Errorf("traces event: %v", evs[1])
	}
	live := evs[2]
	if live["services"] != float64(4) || live["alerts"] != float64(1) || !strings.Contains(live["flow"].(string), "flow-node") ||
		!strings.Contains(live["red"].(string), "red-tile") || live["flowFit"] == "" {
		t.Errorf("live event: %v", live)
	}
}

func TestLiveStreamOtherPagesGetHeaderOnly(t *testing.T) {
	b := fixtureBackend()
	srv := httptest.NewServer(Handler(b))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/ui/live?page=errors", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	reset := readEvents(t, res.Body, 1)[0]
	live := reset["live"].(map[string]any)
	if _, has := reset["rows"]; has || live["flow"] != nil || live["alerts"] != float64(1) {
		t.Errorf("header-only reset: %v", reset)
	}
}

// checkoutDetail: gateway → catalog with an N+1 of 4 sequential SELECTs, and a failing order call.
func checkoutDetail() core.TraceDetail {
	ev := func(id, parent, who, peer, op string, kind core.SpanKind, start, dur int64, status core.SpanStatus, attrs core.Attributes) core.Event {
		return core.Event{TraceID: "tr", SpanID: id, ParentSpanID: parent, Participant: who, Peer: peer, Operation: op, Kind: kind, StartTime: start, Duration: dur, Status: status, Attributes: attrs}
	}
	events := []core.Event{
		ev("g", "", "gateway", "", "POST /checkout", core.KindServer, 0, 48_000, core.StatusOK, core.Attributes{"http_x": "1", "http.y": "2", "B": "3", "a": "4"}),
		ev("gc", "g", "gateway", "catalog", "GET /products", core.KindClient, 1_000, 15_000, core.StatusOK, nil),
		ev("c", "gc", "catalog", "", "GET /products", core.KindServer, 1_500, 14_000, core.StatusOK, nil),
		ev("q0", "c", "catalog", "postgresql", "SELECT", core.KindClient, 2_000, 2_500, core.StatusOK, nil),
		ev("q1", "c", "catalog", "postgresql", "SELECT", core.KindClient, 5_000, 2_500, core.StatusOK, nil),
		ev("q2", "c", "catalog", "postgresql", "SELECT", core.KindClient, 8_000, 2_500, core.StatusOK, nil),
		ev("q3", "c", "catalog", "postgresql", "SELECT", core.KindClient, 11_000, 2_500, core.StatusOK, nil),
		ev("go", "g", "gateway", "order", "POST /orders", core.KindClient, 20_000, 24_000, core.StatusOK, nil),
		ev("o", "go", "order", "", "POST /orders", core.KindServer, 20_500, 23_000, core.StatusError, nil),
	}
	return core.Detail(core.AssembleEvents("tr", events))
}

func TestTracePageWaterfall(t *testing.T) {
	b := fixtureBackend()
	b.details = map[string]core.TraceDetail{"tr": checkoutDetail()}
	code, body := get(t, Handler(b), "/trace/tr")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	mustContain(t, body, "POST /checkout", "badge badge-error", `class="tab tab-active" href="/trace/tr"`,
		`data-selected="o"`, // default selection: the failing span
		`<div class="wf-detail-op" title="POST /orders">`, "wf-bar wf-bar-error",
		"wf-critical-mark", "wf-bar-childtime", `data-hidden="8"`, // root hides its 8 descendants when collapsed
		`data-copy="sequenceDiagram`)
	if strings.Count(body, `data-depth=`) != 9 {
		t.Errorf("rows: %d", strings.Count(body, `data-depth=`))
	}
}

func TestTracePageSequenceGroupsAndExpand(t *testing.T) {
	b := fixtureBackend()
	b.details = map[string]core.TraceDetail{"tr": checkoutDetail()}
	h := Handler(b)
	_, body := get(t, h, "/trace/tr?tab=sequence")
	mustContain(t, body, "▸ SELECT ×4", "Σ10ms · sequential", `data-href="/trace/tr?expand=q0&amp;tab=sequence"`, "seq-participant-label")
	_, body = get(t, h, "/trace/tr?tab=sequence&expand=q0")
	mustContain(t, body, "▾ SELECT (×4)", `data-href="/trace/tr?tab=sequence"`) // the lead row collapses it again
	if strings.Contains(body, "▸ SELECT ×4") {
		t.Error("expanded group should not render the collapsed row")
	}
}

func TestSpanFragmentAndAttributeOrder(t *testing.T) {
	b := fixtureBackend()
	b.details = map[string]core.TraceDetail{"tr": checkoutDetail()}
	h := Handler(b)
	code, body := get(t, h, "/ui/trace/tr/span/g")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	// localeCompare (ICU) order: case-insensitive, and "_" sorts before "."
	order := []string{`title="a"`, `title="B"`, `title="http_x"`, `title="http.y"`}
	for i := 1; i < len(order); i++ {
		if strings.Index(body, order[i-1]) > strings.Index(body, order[i]) {
			t.Errorf("%s should come before %s", order[i-1], order[i])
		}
	}
	if code, _ := get(t, h, "/ui/trace/tr/span/nope"); code != 404 {
		t.Errorf("unknown span: %d", code)
	}
	if code, body := get(t, h, "/trace/missing"); code != 404 || !strings.Contains(body, "Trace not found") {
		t.Errorf("missing trace: %d", code)
	}
}
