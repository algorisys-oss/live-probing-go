package core

import (
	"cmp"
	"slices"
	"sync"
)

type TraceNode struct {
	Event    Event        `json:"event"`
	Children []*TraceNode `json:"children"`
}

type AssembledTrace struct {
	TraceID string       `json:"traceId"`
	Roots   []*TraceNode `json:"roots"`
	Events  []Event      `json:"events"`
	Start   int64        `json:"start"` // earliest span start (micros)
	End     int64        `json:"end"`   // latest span end (micros)
}

type Edge struct {
	From              string `json:"from"`
	To                string `json:"to"`
	Calls             int    `json:"calls"`
	Errors            int    `json:"errors"`
	AvgDurationMicros int64  `json:"avgDurationMicros"`
}

type Topology struct {
	Nodes []string `json:"nodes"`
	Edges []Edge   `json:"edges"`
	// Nodes known only as a client span's peer — nothing instrumented reported from their side.
	// Rendered as "ghost" nodes so partial rollouts stay visible. Datastores and names that also
	// appear as participants are not listed.
	Externals []string `json:"externals"`
}

type WindowOptions struct {
	MaxTraces     int   // default 1000
	HorizonMicros int64 // default 5 minutes
}

// JS Maps iterate in insertion order and the TS version's output order depends on it. Go maps
// don't, so buckets and spans carry an insertion sequence and every listing sorts by it.
type spanEntry struct {
	event Event
	seq   uint64
}

type bucket struct {
	spans    map[string]*spanEntry
	maxStart int64
	seq      uint64
	nextSpan uint64
}

func (b *bucket) orderedSpans() []Event {
	entries := make([]*spanEntry, 0, len(b.spans))
	for _, e := range b.spans {
		entries = append(entries, e)
	}
	slices.SortFunc(entries, func(a, b *spanEntry) int { return cmp.Compare(a.seq, b.seq) })
	out := make([]Event, len(entries))
	for i, e := range entries {
		out[i] = e.event
	}
	return out
}

type traceEntry struct {
	id string
	b  *bucket
}

// TraceWindow is a rolling in-memory window of recent traces. Spans arrive in any order and are
// assembled into trees on demand. Old traces are evicted by age and by a hard cap so memory
// stays bounded. Safe for concurrent use.
type TraceWindow struct {
	mu        sync.RWMutex
	traces    map[string]*bucket
	nextTrace uint64
	newest    int64
	maxTraces int
	horizon   int64
}

func NewTraceWindow(opts WindowOptions) *TraceWindow {
	w := &TraceWindow{traces: map[string]*bucket{}, maxTraces: opts.MaxTraces, horizon: opts.HorizonMicros}
	if w.maxTraces <= 0 {
		w.maxTraces = 1000
	}
	if w.horizon <= 0 {
		w.horizon = 5 * 60 * 1_000_000
	}
	return w
}

func (w *TraceWindow) Add(events []Event) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, e := range events {
		b := w.traces[e.TraceID]
		if b == nil {
			b = &bucket{spans: map[string]*spanEntry{}, seq: w.nextTrace}
			w.nextTrace++
			w.traces[e.TraceID] = b
		}
		if existing := b.spans[e.SpanID]; existing != nil {
			existing.event = e // a re-sent span keeps its original position, like Map.set
		} else {
			b.spans[e.SpanID] = &spanEntry{event: e, seq: b.nextSpan}
			b.nextSpan++
		}
		b.maxStart = max(b.maxStart, e.StartTime)
		w.newest = max(w.newest, e.StartTime)
	}
	w.prune()
}

func (w *TraceWindow) ordered() []traceEntry {
	out := make([]traceEntry, 0, len(w.traces))
	for id, b := range w.traces {
		out = append(out, traceEntry{id, b})
	}
	slices.SortFunc(out, func(a, b traceEntry) int { return cmp.Compare(a.b.seq, b.b.seq) })
	return out
}

func (w *TraceWindow) prune() {
	for id, b := range w.traces {
		if w.newest-b.maxStart > w.horizon {
			delete(w.traces, id)
		}
	}
	if len(w.traces) > w.maxTraces {
		byAge := w.ordered()
		slices.SortStableFunc(byAge, func(a, b traceEntry) int { return cmp.Compare(a.b.maxStart, b.b.maxStart) })
		for _, e := range byAge[:len(w.traces)-w.maxTraces] {
			delete(w.traces, e.id)
		}
	}
}

func (w *TraceWindow) Size() int {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return len(w.traces)
}

// TraceIDs lists the window's traces in first-seen order.
func (w *TraceWindow) TraceIDs() []string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	ordered := w.ordered()
	out := make([]string, len(ordered))
	for i, e := range ordered {
		out[i] = e.id
	}
	return out
}

func byStart(a, b *TraceNode) int {
	return cmp.Or(cmp.Compare(a.Event.StartTime, b.Event.StartTime), cmp.Compare(a.Event.SpanID, b.Event.SpanID))
}

func sortRec(n *TraceNode) {
	slices.SortStableFunc(n.Children, byStart)
	for _, c := range n.Children {
		sortRec(c)
	}
}

// Assemble rebuilds one trace's span tree; nil if the trace isn't in the window.
func (w *TraceWindow) Assemble(traceID string) *AssembledTrace {
	w.mu.RLock()
	b := w.traces[traceID]
	var events []Event
	if b != nil {
		events = b.orderedSpans()
	}
	w.mu.RUnlock()
	if b == nil {
		return nil
	}
	return AssembleEvents(traceID, events)
}

// AssembleEvents builds the span tree for one trace's events (also used for stored history).
func AssembleEvents(traceID string, events []Event) *AssembledTrace {
	nodes := make(map[string]*TraceNode, len(events))
	order := make([]*TraceNode, 0, len(events))
	for _, e := range events {
		n := &TraceNode{Event: e, Children: []*TraceNode{}}
		if _, dup := nodes[e.SpanID]; !dup {
			order = append(order, n)
		}
		nodes[e.SpanID] = n
	}

	roots := []*TraceNode{}
	for _, n := range order {
		n = nodes[n.Event.SpanID]
		if parent := nodes[n.Event.ParentSpanID]; n.Event.ParentSpanID != "" && parent != nil {
			parent.Children = append(parent.Children, n)
		} else {
			roots = append(roots, n)
		}
	}
	slices.SortStableFunc(roots, byStart)
	for _, r := range roots {
		sortRec(r)
	}

	var start, end int64
	for i, e := range events {
		if i == 0 || e.StartTime < start {
			start = e.StartTime
		}
		end = max(end, e.StartTime+e.Duration)
	}
	return &AssembledTrace{TraceID: traceID, Roots: roots, Events: events, Start: start, End: end}
}

// AssembleAll assembles every trace in the window, in first-seen order.
func (w *TraceWindow) AssembleAll() []*AssembledTrace {
	w.mu.RLock()
	ordered := w.ordered()
	spans := make([][]Event, len(ordered))
	for i, e := range ordered {
		spans[i] = e.b.orderedSpans()
	}
	w.mu.RUnlock()
	out := make([]*AssembledTrace, len(ordered))
	for i, e := range ordered {
		out[i] = AssembleEvents(e.id, spans[i])
	}
	return out
}

// orderedSet is an insertion-ordered string set.
type orderedSet struct {
	seen  map[string]bool
	items []string
}

func newOrderedSet() *orderedSet { return &orderedSet{seen: map[string]bool{}, items: []string{}} }

func (s *orderedSet) add(v string) {
	if !s.seen[v] {
		s.seen[v] = true
		s.items = append(s.items, v)
	}
}

// bridgedSpans returns the spans whose callee reported its own span (the other side is
// instrumented): any parent with a cross-participant child.
func bridgedSpans(spans []Event, byID map[string]Event) map[string]bool {
	bridged := map[string]bool{}
	for _, e := range spans {
		if parent, ok := byID[e.ParentSpanID]; ok && e.ParentSpanID != "" && parent.Participant != e.Participant {
			bridged[parent.SpanID] = true
		}
	}
	return bridged
}

// Topology aggregates the whole window into a service+datastore graph. Service->service edges
// come from parent/child spans that cross a participant boundary. Peer edges (datastores and
// uninstrumented "ghost" services) come from spans whose callee never reported a span of its
// own — when the callee did report, the peer edge is suppressed so the call isn't drawn twice.
func (w *TraceWindow) Topology() Topology {
	w.mu.RLock()
	ordered := w.ordered()
	traces := make([][]Event, len(ordered))
	for i, e := range ordered {
		traces[i] = e.b.orderedSpans()
	}
	w.mu.RUnlock()
	return TopologyOf(traces)
}

// TopologyOf aggregates span lists (one per trace) into a topology.
func TopologyOf(traces [][]Event) Topology {
	type agg struct {
		calls, errors int
		dur           int64
	}
	type edgeKey struct{ from, to string }
	nodes := newOrderedSet()
	aggs := map[edgeKey]*agg{}
	var edgeOrder []edgeKey
	bump := func(from, to string, dur int64, err bool) {
		nodes.add(from)
		nodes.add(to)
		k := edgeKey{from, to}
		a := aggs[k]
		if a == nil {
			a = &agg{}
			aggs[k] = a
			edgeOrder = append(edgeOrder, k)
		}
		a.calls++
		if err {
			a.errors++
		}
		a.dur += dur
	}

	participants := map[string]bool{}
	ghostTargets := newOrderedSet()
	for _, spans := range traces {
		for _, e := range spans {
			participants[e.Participant] = true
		}
	}

	for _, spans := range traces {
		byID := make(map[string]Event, len(spans))
		for _, e := range spans {
			byID[e.SpanID] = e
		}
		bridged := bridgedSpans(spans, byID)
		for _, e := range spans {
			isErr := e.Status == StatusError
			if parent, ok := byID[e.ParentSpanID]; ok && e.ParentSpanID != "" && parent.Participant != e.Participant {
				bump(parent.Participant, e.Participant, e.Duration, isErr)
			}
			if e.Peer != "" && e.Peer != e.Participant && !bridged[e.SpanID] {
				bump(e.Participant, e.Peer, e.Duration, isErr)
				if !DatastoreSystems[e.Peer] {
					ghostTargets.add(e.Peer)
				}
			}
		}
	}

	edges := make([]Edge, 0, len(edgeOrder))
	for _, k := range edgeOrder {
		a := aggs[k]
		edges = append(edges, Edge{From: k.from, To: k.to, Calls: a.calls, Errors: a.errors,
			AvgDurationMicros: int64(jsRound(float64(a.dur) / float64(a.calls)))})
	}
	externals := []string{}
	for _, g := range ghostTargets.items {
		if !participants[g] {
			externals = append(externals, g)
		}
	}
	return Topology{Nodes: nodes.items, Edges: edges, Externals: externals}
}
