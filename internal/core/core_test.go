package core

import (
	"regexp"
	"slices"
	"strconv"
	"testing"
	"time"
)

// Port of packages/core/src/core.test.ts.

func strAttr(key, value string) OTLPKeyValue {
	return OTLPKeyValue{Key: key, Value: &OTLPAnyValue{StringValue: &value}}
}

type spanOpts struct {
	parent string
	attrs  []OTLPKeyValue
	err    bool
}

func otlpSpan(spanID, name string, kind int, startNano, endNano int64, o spanOpts) OTLPSpan {
	code := 0
	if o.err {
		code = 2
	}
	var parent any
	if o.parent != "" {
		parent = o.parent
	}
	return OTLPSpan{
		TraceID:           "trace-1",
		SpanID:            spanID,
		ParentSpanID:      parent,
		Name:              name,
		Kind:              kind,
		StartTimeUnixNano: strconv.FormatInt(startNano, 10),
		EndTimeUnixNano:   strconv.FormatInt(endNano, 10),
		Attributes:        o.attrs,
		Status:            &OTLPStatus{Code: code},
	}
}

const ns = int64(1_000_000_000)

// A gateway -> catalog -> postgres/redis trace, in OTLP/HTTP JSON shape.
func fixture() OTLPPayload {
	return OTLPPayload{ResourceSpans: []OTLPResourceSpans{
		{
			Resource: &OTLPResource{Attributes: []OTLPKeyValue{strAttr("service.name", "gateway")}},
			ScopeSpans: []OTLPScopeSpans{{Spans: []OTLPSpan{
				otlpSpan("g1", "POST /api/products/:id", 2, ns, ns+50_000_000, spanOpts{}),
				otlpSpan("g2", "GET", 3, ns+5_000_000, ns+45_000_000, spanOpts{parent: "g1", attrs: []OTLPKeyValue{strAttr("server.address", "catalog")}}),
			}}},
		},
		{
			Resource: &OTLPResource{Attributes: []OTLPKeyValue{strAttr("service.name", "catalog")}},
			ScopeSpans: []OTLPScopeSpans{{Spans: []OTLPSpan{
				otlpSpan("c1", "GET /products/:id", 2, ns+10_000_000, ns+40_000_000, spanOpts{parent: "g2"}),
				otlpSpan("c2", "pg.query:SELECT", 3, ns+15_000_000, ns+30_000_000, spanOpts{parent: "c1", attrs: []OTLPKeyValue{strAttr("db.system", "postgresql")}, err: true}),
				otlpSpan("c3", "get", 3, ns+31_000_000, ns+35_000_000, spanOpts{parent: "c1", attrs: []OTLPKeyValue{strAttr("db.system", "redis")}}),
			}}},
		},
	}}
}

func findEvent(t *testing.T, events []Event, spanID string) Event {
	t.Helper()
	for _, e := range events {
		if e.SpanID == spanID {
			return e
		}
	}
	t.Fatalf("no event with spanId %q", spanID)
	return Event{}
}

func eq[T comparable](t *testing.T, got, want T, what string) {
	t.Helper()
	if got != want {
		t.Errorf("%s: got %v, want %v", what, got, want)
	}
}

func eqSlice[T comparable](t *testing.T, got, want []T, what string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s: got %v, want %v", what, got, want)
	}
}

func TestNormalizeOTLPMapsSpansToEvents(t *testing.T) {
	events := NormalizeOTLP(fixture())
	eq(t, len(events), 5, "event count")

	g1 := findEvent(t, events, "g1")
	eq(t, g1.Participant, "gateway", "g1.participant")
	eq(t, g1.Kind, KindServer, "g1.kind")
	eq(t, g1.ParentSpanID, "", "g1 is a root")
	eq(t, g1.StartTime, 1_000_000, "1e9 ns -> 1e6 us")
	eq(t, g1.Duration, 50_000, "g1.duration")
	eq(t, g1.Peer, "", "server spans have no peer")

	c2 := findEvent(t, events, "c2")
	eq(t, c2.Participant, "catalog", "c2.participant")
	eq(t, c2.Kind, KindClient, "c2.kind")
	eq(t, c2.Peer, "postgresql", "c2.peer")
	eq(t, c2.Status, StatusError, "c2.status")

	c3 := findEvent(t, events, "c3")
	eq(t, c3.Peer, "redis", "c3.peer")
	eq(t, c3.Status, StatusUnset, "c3.status")
}

func singleHTTPSpanPayload(service, name string, attrs ...OTLPKeyValue) OTLPPayload {
	return OTLPPayload{ResourceSpans: []OTLPResourceSpans{{
		Resource: &OTLPResource{Attributes: []OTLPKeyValue{strAttr("service.name", service)}},
		ScopeSpans: []OTLPScopeSpans{{Spans: []OTLPSpan{{
			TraceID: "t", SpanID: "s", Name: name, Kind: 2,
			StartTimeUnixNano: strconv.FormatInt(ns, 10),
			EndTimeUnixNano:   strconv.FormatInt(ns+1, 10),
			Attributes:        attrs,
			Status:            &OTLPStatus{Code: 0},
		}}}},
	}}}
}

func TestNormalizeOTLPRebuildsEndpointFromBareMethod(t *testing.T) {
	p := singleHTTPSpanPayload("gateway", "POST", strAttr("http.method", "POST"), strAttr("http.target", "/api/cart/items?x=1"))
	e := NormalizeOTLP(p)[0]
	eq(t, e.Operation, "POST /api/cart/items", "method + path, query stripped")
}

func TestNormalizeOTLPIgnoresWildcardRoute(t *testing.T) {
	// Catch-all frameworks (Remix/SPA) report http.route="*", which would hide every URL
	// behind "GET *". Prefer the concrete request path (http.target/url.path) in that case.
	p := singleHTTPSpanPayload("hrms", "GET *",
		strAttr("http.method", "GET"), strAttr("http.route", "*"), strAttr("http.target", "/permissions/4"))
	e := NormalizeOTLP(p)[0]
	eq(t, e.Operation, "GET /permissions/4", "wildcard route ignored in favor of the path")
}

func TestNormalizeOTLPDecodesJSONWireShapes(t *testing.T) {
	// Real collectors send kind/status as enum strings and nanos as strings that overflow
	// float64 precision; the decoder must accept both string and number forms.
	body := `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"svc"}}]},
	  "scopeSpans":[{"spans":[{"traceId":"t","spanId":"s","name":"op","kind":"SPAN_KIND_CLIENT",
	  "startTimeUnixNano":"1700000000123456789","endTimeUnixNano":1700000000124456789,
	  "attributes":[{"key":"n","value":{"intValue":"42"}},{"key":"b","value":{"boolValue":true}}],
	  "status":{"code":"STATUS_CODE_ERROR"}}]}]}]}`
	p, err := DecodeOTLP([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	e := NormalizeOTLP(p)[0]
	eq(t, e.Kind, KindClient, "kind")
	eq(t, e.Status, StatusError, "status")
	eq(t, e.StartTime, 1700000000123456, "start micros")
	eq(t, e.Duration, 1000, "duration micros")
	eq(t, e.Attributes["n"], any(float64(42)), "intValue -> number")
	eq(t, e.Attributes["b"], any(true), "boolValue")
}

func TestTraceWindowAssemblesOutOfOrder(t *testing.T) {
	events := NormalizeOTLP(fixture())
	slices.Reverse(events) // children before parents
	w := NewTraceWindow(WindowOptions{})
	w.Add(events)

	tr := w.Assemble("trace-1")
	if tr == nil {
		t.Fatal("trace not found")
	}
	eq(t, len(tr.Roots), 1, "root count")
	eq(t, tr.Roots[0].Event.SpanID, "g1", "root")
	eq(t, tr.Roots[0].Children[0].Event.SpanID, "g2", "child")
	c1 := tr.Roots[0].Children[0].Children[0]
	eq(t, c1.Event.SpanID, "c1", "grandchild")
	// children sorted by startTime: c2 (15ms) before c3 (31ms)
	var ids []string
	for _, n := range c1.Children {
		ids = append(ids, n.Event.SpanID)
	}
	eqSlice(t, ids, []string{"c2", "c3"}, "c1 children order")
}

func findEdge(topo Topology, from, to string) *Edge {
	for i := range topo.Edges {
		if topo.Edges[i].From == from && topo.Edges[i].To == to {
			return &topo.Edges[i]
		}
	}
	return nil
}

func TestTopologyAggregatesServiceAndDatastoreEdges(t *testing.T) {
	w := NewTraceWindow(WindowOptions{})
	w.Add(NormalizeOTLP(fixture()))
	topo := w.Topology()

	nodes := slices.Clone(topo.Nodes)
	slices.Sort(nodes)
	eqSlice(t, nodes, []string{"catalog", "gateway", "postgresql", "redis"}, "nodes")
	for _, pair := range [][2]string{{"gateway", "catalog"}, {"catalog", "postgresql"}, {"catalog", "redis"}} {
		if findEdge(topo, pair[0], pair[1]) == nil {
			t.Errorf("missing edge %s -> %s", pair[0], pair[1])
		}
	}
	eq(t, findEdge(topo, "catalog", "postgresql").Errors, 1, "c2 errored")
	eq(t, findEdge(topo, "gateway", "catalog").Errors, 0, "gateway->catalog errors")
}

func messageStrings(seq Sequence, f func(Message) string) []string {
	out := make([]string, 0, len(seq.Messages))
	for _, m := range seq.Messages {
		out = append(out, f(m))
	}
	return out
}

func TestSequenceForOrdersMessagesAndParticipants(t *testing.T) {
	w := NewTraceWindow(WindowOptions{})
	w.Add(NormalizeOTLP(fixture()))
	seq := SequenceFor(w.Assemble("trace-1"))

	eqSlice(t, seq.Participants, []string{"gateway", "catalog", "postgresql", "redis"}, "participants")
	eqSlice(t, messageStrings(seq, func(m Message) string { return m.From + "->" + m.To + ":" + m.Label }),
		[]string{"gateway->catalog:GET /products/:id", "catalog->postgresql:pg.query:SELECT", "catalog->redis:get"}, "messages")
}

// A trace where gateway calls an instrumented service (catalog reports its own server
// span) and an uninstrumented one (legacy-api exists only as the client span's peer).
func ghostFixture() []Event {
	base := Event{TraceID: "t-ghost", Status: StatusUnset, Attributes: Attributes{}, Duration: 10}
	mk := func(spanID, parent, participant, peer, op string, kind SpanKind, start int64) Event {
		e := base
		e.SpanID, e.ParentSpanID, e.Participant, e.Peer, e.Operation, e.Kind, e.StartTime = spanID, parent, participant, peer, op, kind, start
		return e
	}
	return []Event{
		mk("r", "", "gateway", "", "GET /home", KindServer, 1_000),
		// instrumented callee: client span + the callee's own server span
		mk("c", "r", "gateway", "catalog", "GET /products", KindClient, 1_100),
		mk("s", "c", "catalog", "", "GET /products", KindServer, 1_200),
		// uninstrumented callee: only the client span, peer never reports
		mk("g", "r", "gateway", "legacy-api", "GET /legacy", KindClient, 1_300),
	}
}

func TestSequenceForTagsMessagesWithSpan(t *testing.T) {
	w := NewTraceWindow(WindowOptions{})
	w.Add(ghostFixture())
	seq := SequenceFor(w.Assemble("t-ghost"))
	// request arrow -> the callee's server span; peer arrow -> the client span
	eqSlice(t, messageStrings(seq, func(m Message) string { return m.From + "->" + m.To + ":" + m.SpanID }),
		[]string{"gateway->catalog:s", "gateway->legacy-api:g"}, "messages")
}

func TestMermaidExporters(t *testing.T) {
	w := NewTraceWindow(WindowOptions{})
	w.Add(NormalizeOTLP(fixture()))
	seqText := ToMermaidSequence(SequenceFor(w.Assemble("trace-1")))
	for _, re := range []string{`^sequenceDiagram`, `participant gateway as gateway`, `gateway->>catalog: GET /products/:id`} {
		if !regexp.MustCompile(re).MatchString(seqText) {
			t.Errorf("sequence text missing %q:\n%s", re, seqText)
		}
	}
	flowText := ToMermaidFlow(w.Topology())
	for _, re := range []string{`^graph LR`, `catalog\["catalog"\]`, `gateway -->\|1\| catalog`} {
		if !regexp.MustCompile(re).MatchString(flowText) {
			t.Errorf("flow text missing %q:\n%s", re, flowText)
		}
	}
}

func TestTopologyGhostEdgesOncePerCall(t *testing.T) {
	w := NewTraceWindow(WindowOptions{})
	w.Add(ghostFixture())
	topo := w.Topology()

	if !slices.Contains(topo.Nodes, "legacy-api") {
		t.Error("legacy-api should be a node")
	}
	ghost := findEdge(topo, "gateway", "legacy-api")
	if ghost == nil {
		t.Fatal("gateway -> legacy-api ghost edge")
	}
	eq(t, ghost.Calls, 1, "ghost calls")
	eqSlice(t, topo.Externals, []string{"legacy-api"}, "externals")
	// the instrumented callee is NOT double-counted (parent/child edge only)
	eq(t, findEdge(topo, "gateway", "catalog").Calls, 1, "real edge calls")
}

func TestTopologyExternalsExcludeDatastoresAndParticipants(t *testing.T) {
	w := NewTraceWindow(WindowOptions{})
	w.Add(NormalizeOTLP(fixture()))
	eq(t, len(w.Topology().Externals), 0, "externals")
}

func TestSequenceForGhostPeers(t *testing.T) {
	w := NewTraceWindow(WindowOptions{})
	w.Add(ghostFixture())
	seq := SequenceFor(w.Assemble("t-ghost"))
	eqSlice(t, messageStrings(seq, func(m Message) string { return m.From + "->" + m.To + ":" + m.Label }),
		[]string{"gateway->catalog:GET /products", "gateway->legacy-api:GET /legacy"}, "messages")
	eqSlice(t, seq.Participants, []string{"gateway", "catalog", "legacy-api"}, "participants")
	eqSlice(t, seq.Externals, []string{"legacy-api"}, "externals")
}

func TestMermaidFlowStylesExternalsDashed(t *testing.T) {
	w := NewTraceWindow(WindowOptions{})
	w.Add(ghostFixture())
	flowText := ToMermaidFlow(w.Topology())
	if !regexp.MustCompile(`classDef external`).MatchString(flowText) || !regexp.MustCompile(`class legacy_api external`).MatchString(flowText) {
		t.Errorf("external styling missing:\n%s", flowText)
	}
	clean := NewTraceWindow(WindowOptions{})
	clean.Add(NormalizeOTLP(fixture()))
	if regexp.MustCompile(`classDef external`).MatchString(ToMermaidFlow(clean.Topology())) {
		t.Error("no externals -> no classDef noise")
	}
}

func TestWindowEvictsByHorizonAndCap(t *testing.T) {
	w := NewTraceWindow(WindowOptions{HorizonMicros: 100})
	old := Event{TraceID: "old", SpanID: "o1", Participant: "svc", Operation: "x", Kind: KindServer, StartTime: 1_000_000, Duration: 10, Status: StatusOK, Attributes: Attributes{}}
	w.Add([]Event{old})
	eq(t, w.Size(), 1, "size")
	// a much newer trace pushes the horizon past the old one
	newer := old
	newer.TraceID, newer.SpanID, newer.StartTime = "new", "n1", 5_000_000
	w.Add([]Event{newer})
	eqSlice(t, w.TraceIDs(), []string{"new"}, "trace ids")

	capped := NewTraceWindow(WindowOptions{MaxTraces: 2})
	for i := range 3 {
		e := old
		e.TraceID, e.SpanID, e.StartTime = "t"+strconv.Itoa(i), "s"+strconv.Itoa(i), 1_000_000+int64(i)
		capped.Add([]Event{e})
	}
	eq(t, capped.Size(), 2, "capped size")
	eqSlice(t, capped.TraceIDs(), []string{"t1", "t2"}, "oldest evicted first")
}

func TestCoerceEventsClampsFarFuture(t *testing.T) {
	now := time.Now().UnixMilli() * 1000
	// A genuine, current trace...
	real := CoerceEvents([]any{map[string]any{"traceId": "real", "spanId": "r", "participant": "svc", "operation": "GET /x", "startTime": float64(now), "duration": float64(1000)}})
	// ...and a hostile event dated far in the future (pre-clamp it would become the window clock).
	poison := CoerceEvents([]any{map[string]any{"traceId": "poison", "spanId": "p", "participant": "svc", "operation": "x", "startTime": 1e30, "duration": float64(1)}})
	if poison[0].StartTime > now+60_000_000+1_000_000 {
		t.Errorf("future start time not clamped: %d", poison[0].StartTime)
	}

	w := NewTraceWindow(WindowOptions{HorizonMicros: 5 * 60 * 1_000_000})
	w.Add(real)
	w.Add(poison)
	if !slices.Contains(w.TraceIDs(), "real") {
		t.Error("the recent trace should survive a future-dated poison event")
	}
}

func TestCoerceEventsDefaultsAndDrops(t *testing.T) {
	out := CoerceEvents(map[string]any{"events": []any{
		map[string]any{"spanId": "no-trace"}, // dropped
		"junk",                               // dropped
		map[string]any{"traceId": "t", "spanId": "s", "kind": "bogus", "status": "nope", "startTime": -5,
			"attributes": map[string]any{"ok": "v", "n": float64(1), "nested": map[string]any{"x": 1}}},
	}})
	eq(t, len(out), 1, "kept events")
	e := out[0]
	eq(t, e.Participant, "unknown", "participant default")
	eq(t, e.Kind, KindInternal, "kind default")
	eq(t, e.Status, StatusUnset, "status default")
	eq(t, e.StartTime, 0, "negative start clamped")
	eq(t, len(e.Attributes), 2, "non-scalar attribute dropped")
}

// --- sequenceLayout: activation bars + call/return bracketing ---

func msg(over func(*Message)) Message {
	m := Message{From: "a", To: "b", SpanID: "s", Label: "op", StartTime: 0, DurationMicros: 10, Status: "ok"}
	over(&m)
	return m
}

func seqOf(messages ...Message) Sequence {
	var participants []string
	for _, m := range messages {
		for _, p := range []string{m.From, m.To} {
			if !slices.Contains(participants, p) {
				participants = append(participants, p)
			}
		}
	}
	return Sequence{Participants: participants, Messages: messages, Externals: []string{}}
}

func laid(t *testing.T, l SequenceLayout, spanID string) *LaidOutMessage {
	t.Helper()
	for _, m := range l.Messages {
		if m.SpanID == spanID {
			return m
		}
	}
	t.Fatalf("no laid-out message %q", spanID)
	return nil
}

func activation(t *testing.T, l SequenceLayout, spanID string) Activation {
	t.Helper()
	for _, a := range l.Activations {
		if a.SpanID == spanID {
			return a
		}
	}
	t.Fatalf("no activation %q", spanID)
	return Activation{}
}

func call(from, to, spanID string, start, dur int64) Message {
	return msg(func(m *Message) { m.From, m.To, m.SpanID, m.StartTime, m.DurationMicros = from, to, spanID, start, dur })
}

func TestSequenceLayoutBracketsNestedSyncCall(t *testing.T) {
	l := LayoutSequence(seqOf(call("a", "b", "P", 0, 100), call("b", "c", "A", 10, 20)), nil)
	P, A := laid(t, l, "P"), laid(t, l, "A")
	// rows: call P(0), call A(1), return A(2), return P(3)
	eqSlice(t, []int{P.CallRow, A.CallRow, A.ReturnRow, P.ReturnRow}, []int{0, 1, 2, 3}, "rows")
	eq(t, len(l.Rows), 4, "row count")

	actP, actA := activation(t, l, "P"), activation(t, l, "A")
	if !(actP.FromRow < actA.FromRow && actA.ToRow < actP.ToRow) {
		t.Error("P activation should enclose A")
	}
}

func TestSequenceLayoutSiblingReturnsBeforeNextCall(t *testing.T) {
	l := LayoutSequence(seqOf(call("a", "b", "P", 0, 100), call("b", "c", "A", 10, 20), call("b", "d", "B", 40, 20)), nil)
	if !(laid(t, l, "A").ReturnRow < laid(t, l, "B").CallRow) {
		t.Error("A returns before B is called")
	}
}

func TestSequenceLayoutAsyncHasNoReturn(t *testing.T) {
	l := LayoutSequence(seqOf(msg(func(m *Message) { m.From, m.To, m.SpanID, m.Async = "a", "queue", "M", true })), nil)
	M := laid(t, l, "M")
	eq(t, M.ReturnRow, M.CallRow, "no separate return row")
	eq(t, len(l.Rows), 1, "row count")
	eq(t, l.Rows[0].Kind, "call", "row kind")
	eq(t, len(l.Activations), 0, "activations")
}

func TestSequenceLayoutDepthOffsetsOverlaps(t *testing.T) {
	l := LayoutSequence(seqOf(call("a", "c", "A", 10, 50), call("a", "c", "B", 20, 50)), nil)
	eq(t, laid(t, l, "A").Depth, 0, "A depth")
	eq(t, laid(t, l, "B").Depth, 1, "B opens while A is still active on lifeline c")
}

// --- sequenceLayout: collapsing repeated sibling leaf calls into a ×N group ---

func dbCalls(starts []int64, dur int64, label string) []Message {
	out := make([]Message, len(starts))
	for i, s := range starts {
		out[i] = msg(func(m *Message) {
			m.From, m.To, m.Label, m.SpanID, m.StartTime, m.DurationMicros = "a", "db", label, "s"+strconv.Itoa(i), s, dur
		})
	}
	return out
}

func TestSequenceLayoutFoldsRepeatedLeafCalls(t *testing.T) {
	l := LayoutSequence(seqOf(dbCalls([]int64{0, 20, 40, 60}, 10, "db.find")...), nil)
	eq(t, len(l.Groups), 1, "group count")
	g := l.Groups[0]
	eq(t, g.Count, 4, "count")
	eqSlice(t, g.SpanIDs, []string{"s0", "s1", "s2", "s3"}, "members")

	// Collapsed by default: the four calls become one synthetic sync call → call + return = 2 rows.
	eq(t, len(l.Rows), 2, "row count")
	for _, r := range l.Rows {
		if r.Kind == "call" {
			eq(t, r.Message.GroupRole, "collapsed", "group role")
			eq(t, r.Message.Group.Count, 4, "group count on row")
		}
	}
}

func TestSequenceLayoutNoGroupBelowThreshold(t *testing.T) {
	l := LayoutSequence(seqOf(dbCalls([]int64{0, 20}, 10, "db.find")...), nil)
	eq(t, len(l.Groups), 0, "groups")
	for _, r := range l.Rows {
		if r.Message.Group != nil {
			t.Error("row should not be grouped")
		}
	}
}

func TestSequenceLayoutNeverFoldsNonLeaf(t *testing.T) {
	// Three identical a→b parents, each strictly containing a distinct child — parents are not leaves.
	var messages []Message
	for i, base := range []int64{0, 100, 200} {
		messages = append(messages,
			msg(func(m *Message) {
				m.From, m.To, m.Label, m.SpanID, m.StartTime, m.DurationMicros = "a", "b", "handle", "P"+strconv.Itoa(i), base, 50
			}),
			msg(func(m *Message) {
				m.From, m.To, m.Label, m.SpanID, m.StartTime, m.DurationMicros = "b", "x", "child"+strconv.Itoa(i), "C"+strconv.Itoa(i), base+10, 20
			}),
		)
	}
	eq(t, len(LayoutSequence(seqOf(messages...), nil).Groups), 0, "parents contain children → not grouped")
}

func TestSequenceLayoutGroupAggregates(t *testing.T) {
	var calls []Message
	for i, d := range []int64{10, 20, 30, 40} {
		calls = append(calls, msg(func(m *Message) {
			m.From, m.To, m.Label, m.SpanID = "a", "db", "db.find", "s"+strconv.Itoa(i)
			m.StartTime, m.DurationMicros = int64(i)*100, d // non-overlapping → sequential
			if i == 1 {
				m.Status = "error"
			}
		}))
	}
	g := LayoutSequence(seqOf(calls...), nil).Groups[0]
	eq(t, g.TotalDurationMicros, 100, "total")
	eq(t, g.MinDurationMicros, 10, "min")
	eq(t, g.MaxDurationMicros, 40, "max")
	eq(t, g.AvgDurationMicros, 25, "avg")
	eq(t, g.ErrorCount, 1, "errors")
	eq(t, g.Concurrent, false, "non-overlapping run is sequential (N+1 shape)")
}

func TestSequenceLayoutOverlappingGroupIsConcurrent(t *testing.T) {
	g := LayoutSequence(seqOf(dbCalls([]int64{0, 10, 20}, 100, "db.find")...), nil).Groups[0]
	eq(t, g.Concurrent, true, "concurrent")
}

func TestSequenceLayoutInterleavedClusterGroupsPerSignature(t *testing.T) {
	mk := func(label, id string, start int64) Message {
		return msg(func(m *Message) {
			m.From, m.To, m.Label, m.SpanID, m.StartTime, m.DurationMicros = "a", "db", label, id, start, 10
		})
	}
	l := LayoutSequence(seqOf(
		mk("dept.find", "d0", 0), mk("emp.find", "e0", 12), mk("dept.find", "d1", 24),
		mk("emp.find", "e1", 36), mk("dept.find", "d2", 48), mk("emp.find", "e2", 60),
	), nil)
	eq(t, len(l.Groups), 2, "groups")
	eqSlice(t, []int{l.Groups[0].Count, l.Groups[1].Count}, []int{3, 3}, "counts")
}

func TestSequenceLayoutExpandsGroup(t *testing.T) {
	calls := dbCalls([]int64{0, 20, 40, 60}, 10, "db.find")
	g := LayoutSequence(seqOf(calls...), nil).Groups[0]
	l := LayoutSequence(seqOf(calls...), map[string]bool{g.ID: true})

	found := false
	for _, r := range l.Rows {
		if r.Message.SpanID == "s0" {
			found = true
		}
		if r.Message.GroupRole == "collapsed" {
			t.Error("no synthetic collapsed row while expanded")
		}
	}
	if !found {
		t.Error("member rows are drawn when expanded")
	}
}

// --- criticalPath: the chain that determines the trace end time ---

func TestCriticalPath(t *testing.T) {
	ev := func(traceID, spanID, parent, op string, kind SpanKind, start, dur int64) Event {
		return Event{TraceID: traceID, SpanID: spanID, ParentSpanID: parent, Participant: "gw", Operation: op, Kind: kind, StartTime: start, Duration: dur, Status: StatusUnset, Attributes: Attributes{}}
	}
	// root [0,100]; A [10,30] then B [40,90] run sequentially under root (both on the path);
	// C [45,60] runs inside B and ends B's tail before the cursor, so it is critical within B.
	w := NewTraceWindow(WindowOptions{})
	w.Add([]Event{
		ev("cp", "root", "", "GET /", KindServer, 0, 100),
		ev("cp", "A", "root", "a", KindInternal, 10, 20),
		ev("cp", "B", "root", "b", KindInternal, 40, 50),
		ev("cp", "C", "B", "c", KindInternal, 45, 15),
	})
	cp := CriticalPath(w.Assemble("cp"))
	for _, id := range []string{"root", "A", "B", "C"} {
		if !slices.Contains(cp, id) {
			t.Errorf("%s should be on the path: %v", id, cp)
		}
	}

	// Now make D run entirely in B's shadow (ends well before B ends) → off the path.
	w2 := NewTraceWindow(WindowOptions{})
	w2.Add([]Event{
		ev("cp2", "root", "", "GET /", KindServer, 0, 100),
		ev("cp2", "B", "root", "b", KindInternal, 40, 50),
		ev("cp2", "D", "root", "d", KindInternal, 45, 20),
	})
	cp2 := CriticalPath(w2.Assemble("cp2"))
	if !slices.Contains(cp2, "root") || !slices.Contains(cp2, "B") {
		t.Errorf("root and B on path: %v", cp2)
	}
	if slices.Contains(cp2, "D") {
		t.Error("D ran concurrently in B's shadow → not on the critical path")
	}
}

func TestNormalizeOTLPReadsNewDBSemconv(t *testing.T) {
	// OTel semconv >= 1.33 renamed db.system to db.system.name (e.g. otelpgx emits it). Both must
	// resolve to the datastore, not fall through to server.address ("postgres" host → a ghost).
	for _, key := range []string{"db.system", "db.system.name"} {
		p := OTLPPayload{ResourceSpans: []OTLPResourceSpans{{
			Resource: &OTLPResource{Attributes: []OTLPKeyValue{strAttr("service.name", "catalog")}},
			ScopeSpans: []OTLPScopeSpans{{Spans: []OTLPSpan{{TraceID: "t", SpanID: "s", Name: "SELECT", Kind: 3,
				StartTimeUnixNano: "1000", EndTimeUnixNano: "2000",
				Attributes: []OTLPKeyValue{strAttr(key, "postgresql"), strAttr("server.address", "postgres")}}}}},
		}}}
		if peer := NormalizeOTLP(p)[0].Peer; peer != "postgresql" {
			t.Errorf("%s: peer = %q, want postgresql", key, peer)
		}
	}
}
