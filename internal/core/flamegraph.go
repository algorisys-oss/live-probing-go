package core

import (
	"cmp"
	"slices"
)

// FlameSpan is the minimal span shape the aggregator needs — satisfied by both a live Event and
// a stored history row, so it can fold either without adapters.
type FlameSpan struct {
	SpanID       string
	ParentSpanID string
	Operation    string
	Participant  string
	Duration     int64 // micros
}

// FlameNode is one node of the aggregate flamegraph: the same operation-path merged across every
// trace, with total time, self time (total minus what its children cover), and hit count.
type FlameNode struct {
	Operation   string       `json:"operation"`
	Participant string       `json:"participant"`
	TotalMicros int64        `json:"totalMicros"`
	SelfMicros  int64        `json:"selfMicros"`
	Count       int          `json:"count"`
	Children    []*FlameNode `json:"children"`
}

type flameKey struct{ participant, operation string }

// AggregateFlamegraph merges many traces of one endpoint into a single flamegraph: where does
// this endpoint spend its time on average. Spans merge by path (position under the parent
// chain), so the same operation under different parents stays distinct.
func AggregateFlamegraph(traces [][]FlameSpan) *FlameNode {
	root := &FlameNode{Children: []*FlameNode{}}
	childIndex := map[*FlameNode]map[flameKey]*FlameNode{}

	var addSpan func(parent *FlameNode, span FlameSpan, childrenOf map[string][]FlameSpan)
	addSpan = func(parent *FlameNode, span FlameSpan, childrenOf map[string][]FlameSpan) {
		index := childIndex[parent]
		if index == nil {
			index = map[flameKey]*FlameNode{}
			childIndex[parent] = index
		}
		k := flameKey{span.Participant, span.Operation}
		node := index[k]
		if node == nil {
			node = &FlameNode{Operation: span.Operation, Participant: span.Participant, Children: []*FlameNode{}}
			index[k] = node
			parent.Children = append(parent.Children, node)
		}
		node.TotalMicros += max(0, span.Duration)
		node.Count++
		for _, child := range childrenOf[span.SpanID] {
			addSpan(node, child, childrenOf)
		}
	}

	for _, spans := range traces {
		ids := make(map[string]bool, len(spans))
		for _, s := range spans {
			ids[s.SpanID] = true
		}
		childrenOf := map[string][]FlameSpan{}
		for _, s := range spans {
			if s.ParentSpanID != "" && ids[s.ParentSpanID] {
				childrenOf[s.ParentSpanID] = append(childrenOf[s.ParentSpanID], s)
			}
		}
		// A trace's roots: spans with no parent, or whose parent isn't in this trace.
		for _, s := range spans {
			if s.ParentSpanID == "" || !ids[s.ParentSpanID] {
				addSpan(root, s, childrenOf)
			}
		}
	}

	var finalizeSelf func(n *FlameNode)
	finalizeSelf = func(n *FlameNode) {
		var childTotal int64
		for _, c := range n.Children {
			finalizeSelf(c)
			childTotal += c.TotalMicros
		}
		n.SelfMicros = max(0, n.TotalMicros-childTotal)
	}
	for _, c := range root.Children {
		finalizeSelf(c)
	}
	return root
}

// CriticalPath returns the span ids on the chain that determines the trace's end time.
// Shortening a span on it shortens the whole trace; shortening one off it (work running in
// another span's shadow) does not. Computed by sweeping backward from each node's end.
func CriticalPath(trace *AssembledTrace) []string {
	seen := map[string]bool{}
	out := []string{}
	endOf := func(n *TraceNode) int64 { return n.Event.StartTime + n.Event.Duration }

	var walk func(n *TraceNode)
	walk = func(n *TraceNode) {
		if !seen[n.Event.SpanID] {
			seen[n.Event.SpanID] = true
			out = append(out, n.Event.SpanID)
		}
		// Latest-ending child first; cursor starts at this node's end and marches backward.
		kids := slices.Clone(n.Children)
		slices.SortStableFunc(kids, func(a, b *TraceNode) int { return cmp.Compare(endOf(b), endOf(a)) })
		cursor := endOf(n)
		for _, child := range kids {
			// On the path only if it finished at/before the cursor (it wasn't running
			// concurrently inside an already-critical sibling's span).
			if endOf(child) <= cursor {
				walk(child)
				cursor = child.Event.StartTime
			}
		}
	}
	for _, r := range trace.Roots {
		walk(r)
	}
	return out
}
