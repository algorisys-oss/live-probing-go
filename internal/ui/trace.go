package ui

import (
	"math"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"

	"liveprobe/internal/core"
	"liveprobe/internal/ui/view"
)

// Geometry for the trace page: the waterfall (ui/src/components/waterfall-view.tsx) and the UML
// sequence diagram (sequence-view.tsx), computed in Go and printed by trace.templ.

// serviceColor is a stable per-service hue (datastores share one colour).
func serviceColor(name string) string {
	if view.IsDatastore(name) {
		return "var(--datastore)"
	}
	return "hsl(" + view.Num(float64(view.HashString(name)%360)) + " 55% 58%)"
}

func pct(x float64) string { return view.Num(x) + "%" }

// --- waterfall -----------------------------------------------------------------------------

const wfLabelW = 280 // keep in sync with .wf-label width in styles.css

type wfTick struct{ Left, Label string }

type wfCoverage struct{ Left, Width string }

type wfRow struct {
	SpanID, Class, Op, Title, Color      string
	Depth, Hidden                        int // hidden: descendants hidden when this row is collapsed
	PadLeft                              string
	Rails                                []string // left offsets of the depth guides
	HasChildren, Critical, Error         bool
	BarClass, BarStyle, DurStyle, DurTxt string
	Coverage                             []wfCoverage
}

type waterfall struct {
	Empty bool
	Ticks []wfTick
	Rows  []wfRow
}

// childCoverage merges the time ranges covered by a span's direct children (clipped to the span);
// the bar's remainder outside them is the span's self-time.
func childCoverage(start, end int64, children []core.SpanRow) [][2]int64 {
	var iv [][2]int64
	for _, c := range children {
		a, b := max(start, c.StartTime), min(end, c.StartTime+c.Duration)
		if b > a {
			iv = append(iv, [2]int64{a, b})
		}
	}
	sort.SliceStable(iv, func(i, j int) bool { return iv[i][0] < iv[j][0] })
	var merged [][2]int64
	for _, x := range iv {
		if n := len(merged); n > 0 && x[0] <= merged[n-1][1] {
			merged[n-1][1] = max(merged[n-1][1], x[1])
		} else {
			merged = append(merged, x)
		}
	}
	return merged
}

func buildWaterfall(d *core.TraceDetail) waterfall {
	spans := d.Spans
	if len(spans) == 0 {
		return waterfall{Empty: true}
	}
	total := math.Max(float64(d.Summary.DurationMicros), 1)
	traceStart := float64(d.Summary.StartTime)
	w := waterfall{}
	for i := 0; i <= 4; i++ {
		t := total * float64(i) / 4
		w.Ticks = append(w.Ticks, wfTick{Left: pct(t / total * 100), Label: view.FormatMicros(t)})
	}
	critical := map[string]bool{}
	for _, id := range d.CriticalPath {
		critical[id] = true
	}
	children := map[string][]core.SpanRow{}
	for _, s := range spans {
		if s.ParentSpanID != "" {
			children[s.ParentSpanID] = append(children[s.ParentSpanID], s)
		}
	}
	for i, s := range spans {
		hidden := 0 // spans are DFS-ordered: descendants are the following, deeper rows
		for j := i + 1; j < len(spans) && spans[j].Depth > s.Depth; j++ {
			hidden++
		}
		left := math.Min(100, math.Max(0, (float64(s.StartTime)-traceStart)/total*100))
		width := math.Min(100-left, math.Max(0.4, float64(s.Duration)/total*100))
		r := wfRow{
			SpanID: s.SpanID, Op: s.Operation, Title: s.Participant + " · " + s.Operation, Color: serviceColor(s.Participant),
			Depth: s.Depth, Hidden: hidden, PadLeft: view.Num(float64(6 + s.Depth*12)),
			HasChildren: i+1 < len(spans) && spans[i+1].Depth > s.Depth,
			Critical:    critical[s.SpanID], Error: s.Status == "error",
			DurTxt: view.Micros(s.Duration),
		}
		for dd := range s.Depth {
			r.Rails = append(r.Rails, view.Num(float64(6+dd*12)))
		}
		r.Class = "wf-row"
		if r.Critical {
			r.Class += " wf-row-critical"
		}
		r.BarClass = "wf-bar"
		r.BarStyle = "left:" + pct(left) + ";width:" + pct(width)
		if r.Error {
			r.BarClass += " wf-bar-error"
		} else {
			r.BarStyle += ";background:" + r.Color
		}
		if r.Critical {
			r.BarClass += " wf-bar-critical"
		}
		if width > 12 {
			r.DurStyle = "left:" + pct(left) + ";right:auto;margin-left:4px"
		} else {
			r.DurStyle = "left:" + pct(left+width) + ";right:auto;margin-left:4px"
		}
		if r.HasChildren {
			for _, c := range childCoverage(s.StartTime, s.StartTime+s.Duration, children[s.SpanID]) {
				r.Coverage = append(r.Coverage, wfCoverage{
					Left:  pct((float64(c[0]) - traceStart) / total * 100),
					Width: pct(float64(c[1]-c[0]) / total * 100),
				})
			}
		}
		w.Rows = append(w.Rows, r)
	}
	return w
}

// --- span detail ---------------------------------------------------------------------------

// icuLess approximates ICU root collation (what JS localeCompare uses) for ASCII keys:
// punctuation in ICU order before digits before letters, letters case-insensitively first,
// lowercase before uppercase on ties. Byte order would put "http.x" before "http_x"; ICU doesn't.
const icuPunct = "_-,;:!?.'\"()[]{}@*/\\&#%`^+<=>|~$"

func collationKey(r rune) (primary int, tertiary int) {
	if i := strings.IndexRune(icuPunct, r); i >= 0 && r < 128 {
		return 100 + i, 0
	}
	switch {
	case r >= '0' && r <= '9':
		return 200 + int(r-'0'), 0
	case r >= 'a' && r <= 'z':
		return 300 + int(r-'a')*2, 0
	case r >= 'A' && r <= 'Z':
		return 300 + int(r-'A')*2, 1
	case r == ' ':
		return 50, 0
	}
	return 1000 + int(r), 0
}

func icuLess(a, b string) bool {
	ra, rb := []rune(a), []rune(b)
	tie := 0
	for i := 0; i < len(ra) && i < len(rb); i++ {
		pa, ta := collationKey(ra[i])
		pb, tb := collationKey(rb[i])
		if pa != pb {
			return pa < pb
		}
		if tie == 0 && ta != tb {
			tie = ta - tb
		}
	}
	if len(ra) != len(rb) {
		return len(ra) < len(rb)
	}
	return tie < 0
}

type kv struct{ K, V string }

type spanDetail struct {
	Op, Participant, Color, Kind, Peer, Status string
	Error                                      bool
	Duration, StartOffset                      string
	Attrs                                      []kv
}

func buildSpanDetail(s core.SpanRow, traceStart int64) spanDetail {
	d := spanDetail{Op: s.Operation, Participant: s.Participant, Color: serviceColor(s.Participant), Kind: s.Kind, Peer: s.Peer,
		Status: s.Status, Error: s.Status == "error", Duration: view.Micros(s.Duration), StartOffset: view.Micros(s.StartTime - traceStart)}
	for k, v := range s.Attributes {
		d.Attrs = append(d.Attrs, kv{k, attrText(v)})
	}
	slices.SortFunc(d.Attrs, func(a, b kv) int {
		switch {
		case icuLess(a.K, b.K):
			return -1
		case icuLess(b.K, a.K):
			return 1
		}
		return 0
	})
	return d
}

func attrText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return view.Num(x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	}
	return ""
}

// initialSpan is the default selection: the failing span, else the root (the URL #hash, if any,
// overrides it client-side).
func initialSpan(spans []core.SpanRow) string {
	for _, s := range spans {
		if s.Status == "error" {
			return s.SpanID
		}
	}
	if len(spans) > 0 {
		return spans[0].SpanID
	}
	return ""
}

// --- sequence diagram ----------------------------------------------------------------------

const (
	seqColW, seqHeaderH, seqRowH, seqMarginX, seqMarginTop = 170.0, 56.0, 46.0, 40.0, 20.0
	seqActW, seqActLane                                    = 10.0, 6.0
)

type seqParticipant struct {
	RectX, LabelX, Label, Class, LifeTop, LifeBottom string
}

type seqActivation struct {
	SpanID, Class, X, Y, H string
	GroupHref              string // a collapsed group's bar expands the group
}

type seqMsg struct {
	SpanID, Class, Title, GroupHref string
	HitX, HitY, HitW                string
	LabelX, LabelY, Label           string
	LabelClass, Anchor              string
	// a line or (for self-calls) a path
	Line                    bool
	X1, Y1, X2, Y2, D       string
	LineClass, Marker, Dash string
}

type seqDiagram struct {
	Empty        bool
	W, H         string
	Participants []seqParticipant
	Activations  []seqActivation
	Messages     []seqMsg
}

func expandedSet(raw string) map[string]bool {
	out := map[string]bool{}
	for _, id := range strings.Split(raw, ",") {
		if id != "" {
			out[id] = true
		}
	}
	return out
}

// toggleExpandHref is the URL that opens (or closes) one group, keeping the rest of the view.
func toggleExpandHref(traceID string, expanded map[string]bool, id string) string {
	ids := []string{}
	for x := range expanded {
		if x != id {
			ids = append(ids, x)
		}
	}
	if !expanded[id] {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	q := url.Values{"tab": {"sequence"}}
	if len(ids) > 0 {
		q.Set("expand", strings.Join(ids, ","))
	}
	return traceHref(traceID) + "?" + q.Encode()
}

func buildSequence(d *core.TraceDetail, expanded map[string]bool) seqDiagram {
	seq := d.Sequence
	if seq.Externals == nil {
		seq.Externals = []string{}
	}
	externals := map[string]bool{}
	for _, x := range seq.Externals {
		externals[x] = true
	}
	colX := map[string]float64{}
	for i, p := range seq.Participants {
		colX[p] = seqMarginX + seqColW/2 + float64(i)*seqColW
	}
	layout := core.LayoutSequence(seq, expanded)
	collapsedGroup := map[string]bool{}
	for _, g := range layout.Groups {
		if !expanded[g.ID] {
			collapsedGroup[g.ID] = true
		}
	}
	rowY := func(row int) float64 { return seqHeaderH + seqMarginTop + float64(row)*seqRowH }
	laneX := func(p string, depth int) float64 { return colX[p] + float64(depth)*seqActLane }

	width := seqMarginX*2 + float64(len(seq.Participants))*seqColW
	height := seqHeaderH + seqMarginTop + float64(max(len(layout.Rows), 1))*seqRowH + 40
	sd := seqDiagram{W: num(width), H: num(height)}

	for _, p := range seq.Participants {
		x := colX[p]
		class := "seq-participant"
		if externals[p] {
			class += " seq-participant-external"
		}
		sd.Participants = append(sd.Participants, seqParticipant{
			RectX: num(x - seqColW/2 + 8), LabelX: num(x), Label: truncate(p, labelMax), Class: class,
			LifeTop: num(seqHeaderH), LifeBottom: num(height - 20),
		})
	}

	for _, a := range layout.Activations {
		top := rowY(a.FromRow)
		act := seqActivation{SpanID: a.SpanID, X: num(laneX(a.Participant, a.Depth) - seqActW/2), Y: num(top),
			H: num(math.Max(rowY(a.ToRow)-top, 8)), Class: "seq-activation"}
		if a.Status == "error" {
			act.Class += " seq-activation-error"
		}
		if collapsedGroup[a.SpanID] {
			act.Class += " seq-activation-group"
			act.GroupHref = toggleExpandHref(d.Summary.TraceID, expanded, a.SpanID)
		}
		sd.Activations = append(sd.Activations, act)
	}

	for i, row := range layout.Rows {
		m := row.Message
		fromX, okFrom := colX[m.From]
		if _, okTo := colX[m.To]; !okFrom || !okTo {
			continue
		}
		y := rowY(i)
		isErr := m.Status == "error"
		g := m.Group
		isGroupRow := m.GroupRole == "collapsed" || m.GroupRole == "lead"
		suspectN1 := g != nil && !g.Concurrent && g.Count >= 5

		msg := seqMsg{SpanID: m.SpanID, Class: "seq-msg seq-msg-clickable"}
		if isGroupRow {
			msg.Class += " seq-msg-group"
			msg.GroupHref = toggleExpandHref(d.Summary.TraceID, expanded, g.ID)
			msg.SpanID = ""
		}
		if suspectN1 && m.GroupRole == "collapsed" {
			msg.Class += " seq-msg-n1"
		}
		raw := truncate(m.Label, 40)
		switch m.GroupRole {
		case "collapsed":
			prefix := "▸ "
			if suspectN1 {
				prefix = "⚠ "
			}
			msg.Label = prefix + raw + " ×" + view.Itoa(g.Count)
		case "lead":
			msg.Label = "▾ " + raw + " (×" + view.Itoa(g.Count) + ")"
		default:
			msg.Label = raw
		}
		if g != nil {
			kind := "sequential"
			if suspectN1 {
				kind += ", likely N+1"
			}
			if g.Concurrent {
				kind = "concurrent (fan-out)"
			}
			msg.Title = view.Itoa(g.Count) + " identical calls — " + kind + ". Σ" + view.Micros(g.TotalDurationMicros) +
				", avg " + view.Micros(g.AvgDurationMicros) + ", max " + view.Micros(g.MaxDurationMicros)
			if g.ErrorCount > 0 {
				msg.Title += ", " + view.Itoa(g.ErrorCount) + " error"
				if g.ErrorCount > 1 {
					msg.Title += "s"
				}
			}
			action := "collapse"
			if m.GroupRole == "collapsed" {
				action = "expand"
			}
			msg.Title += ". Click to " + action + "."
		}

		calleeCenter := laneX(m.To, m.Depth)
		dir := 1.0
		if calleeCenter < fromX {
			dir = -1
		}
		calleeEdge := calleeCenter - dir*(seqActW/2)
		midX := (fromX + calleeEdge) / 2
		msg.HitX, msg.HitY, msg.HitW = num(math.Min(fromX, calleeCenter)-6), num(y-18), num(math.Abs(calleeCenter-fromX)+12)
		lineClass, retClass := "seq-line", "seq-return"
		if isErr {
			lineClass, retClass = "seq-line seq-line-error", "seq-return seq-return-error"
		}

		if m.From == m.To { // self-message: a loop on the call row only
			if row.Kind == "return" {
				continue
			}
			x := calleeCenter
			msg.HitX, msg.HitY, msg.HitW = num(x-4), num(y-16), num(seqColW)
			msg.D = "M " + num(x+seqActW/2) + " " + num(y) + " h 26 v 16 h -26"
			msg.LineClass, msg.Marker = lineClass, "url(#seq-arrow)"
			if isErr {
				msg.Marker = "url(#seq-arrow-error)"
			}
			msg.LabelX, msg.LabelY, msg.Anchor, msg.LabelClass = num(x+32), num(y-3), "start", "seq-msg-label"
			sd.Messages = append(sd.Messages, msg)
			continue
		}

		msg.Line = true
		msg.LabelX, msg.LabelY, msg.Anchor = num(midX), num(y-6), "middle"
		if row.Kind == "call" {
			msg.X1, msg.Y1, msg.X2, msg.Y2 = num(fromX), num(y), num(calleeEdge), num(y)
			msg.LineClass, msg.LabelClass = lineClass, "seq-msg-label"
			switch {
			case m.Async && isErr:
				msg.Marker = "url(#seq-arrow-open-error)"
			case m.Async:
				msg.Marker = "url(#seq-arrow-open)"
			case isErr:
				msg.Marker = "url(#seq-arrow-error)"
			default:
				msg.Marker = "url(#seq-arrow)"
			}
		} else {
			// Return: a dashed reply back to the caller, labelled with the call's duration (or, on
			// a collapsed group, the cluster's aggregate).
			msg.X1, msg.Y1, msg.X2, msg.Y2 = num(calleeEdge), num(y), num(fromX), num(y)
			msg.LineClass, msg.Dash, msg.Marker = retClass, "4 4", "url(#seq-arrow-open)"
			if isErr {
				msg.Marker = "url(#seq-arrow-open-error)"
			}
			msg.LabelClass = "seq-msg-label seq-return-label"
			msg.Label = ""
			if g != nil && m.GroupRole == "collapsed" {
				kind := "sequential"
				if g.Concurrent {
					kind = "concurrent"
				}
				msg.Label = "Σ" + view.Micros(g.TotalDurationMicros) + " · " + kind
				if g.ErrorCount > 0 {
					msg.Label += " · " + view.Itoa(g.ErrorCount) + " err"
				}
				msg.LabelClass += " seq-group-badge"
			} else if m.DurationMicros > 0 {
				msg.Label = view.Micros(m.DurationMicros)
			}
		}
		sd.Messages = append(sd.Messages, msg)
	}
	return sd
}

// --- handlers ------------------------------------------------------------------------------

type traceData struct {
	Header    headerState
	ID        string
	Tab       string // waterfall | sequence
	Detail    *core.TraceDetail
	Err       string
	Waterfall waterfall
	Sequence  seqDiagram
	Selected  string
	Span      *spanDetail
	Mermaid   string
}

func tracePage(b Backend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		q := r.URL.Query()
		d := traceData{Header: headerFor(b, ""), ID: id, Tab: "waterfall"}
		if q.Get("tab") == "sequence" {
			d.Tab = "sequence"
		}
		detail, err := b.TraceDetail(id)
		switch {
		case err != nil:
			d.Err = err.Error()
		case detail == nil:
			w.WriteHeader(http.StatusNotFound)
		default:
			d.Detail = detail
			d.Mermaid = detail.MermaidSequence
			d.Selected = initialSpan(detail.Spans)
			if d.Tab == "waterfall" {
				d.Waterfall = buildWaterfall(detail)
			} else {
				d.Sequence = buildSequence(detail, expandedSet(q.Get("expand")))
			}
			for _, s := range detail.Spans {
				if s.SpanID == d.Selected {
					sd := buildSpanDetail(s, detail.Summary.StartTime)
					d.Span = &sd
				}
			}
		}
		render(w, r, tracePageView(d))
	}
}

// spanFragment serves one span's detail panel (fetched when the selection changes).
func spanFragment(b Backend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		detail, err := b.TraceDetail(r.PathValue("id"))
		if err != nil || detail == nil {
			http.NotFound(w, r)
			return
		}
		for _, s := range detail.Spans {
			if s.SpanID == r.PathValue("span") {
				render(w, r, spanPanel(buildSpanDetail(s, detail.Summary.StartTime)))
				return
			}
		}
		http.NotFound(w, r)
	}
}
