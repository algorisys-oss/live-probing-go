package core

// TraceSummary is the one-line view of a trace used by lists, search and the live stream.
type TraceSummary struct {
	TraceID        string   `json:"traceId"`
	RootOperation  string   `json:"rootOperation"`
	Services       []string `json:"services"`
	SpanCount      int      `json:"spanCount"`
	StartTime      int64    `json:"startTime"` // micros
	DurationMicros int64    `json:"durationMicros"`
	HasError       bool     `json:"hasError"`
}

func Summarize(trace *AssembledTrace) TraceSummary {
	services := newOrderedSet()
	hasError := false
	for _, e := range trace.Events {
		services.add(e.Participant)
		if e.Status == StatusError {
			hasError = true
		}
	}
	root := ""
	if len(trace.Roots) > 0 {
		root = trace.Roots[0].Event.Operation
	}
	return TraceSummary{
		TraceID: trace.TraceID, RootOperation: root, Services: services.items,
		SpanCount: len(trace.Events), StartTime: trace.Start, DurationMicros: trace.End - trace.Start,
		HasError: hasError,
	}
}

// SpanRow is one span flattened for the waterfall; Depth is its nesting level in the tree.
type SpanRow struct {
	SpanID       string     `json:"spanId"`
	ParentSpanID string     `json:"parentSpanId,omitempty"`
	Depth        int        `json:"depth"`
	Participant  string     `json:"participant"`
	Operation    string     `json:"operation"`
	Kind         string     `json:"kind"`
	Status       string     `json:"status"`
	StartTime    int64      `json:"startTime"` // absolute micros
	Duration     int64      `json:"duration"`  // micros
	Peer         string     `json:"peer,omitempty"`
	Attributes   Attributes `json:"attributes"`
}

// FlameSpan adapts a stored row for AggregateFlamegraph.
func (r SpanRow) FlameSpan() FlameSpan {
	return FlameSpan{SpanID: r.SpanID, ParentSpanID: r.ParentSpanID, Operation: r.Operation, Participant: r.Participant, Duration: r.Duration}
}

// flattenSpans is a depth-first flatten of the trace tree, preserving the assembled child order.
func flattenSpans(trace *AssembledTrace) []SpanRow {
	rows := []SpanRow{}
	var walk func(n *TraceNode, depth int)
	walk = func(n *TraceNode, depth int) {
		e := n.Event
		attrs := e.Attributes
		if attrs == nil {
			attrs = Attributes{}
		}
		rows = append(rows, SpanRow{
			SpanID: e.SpanID, ParentSpanID: e.ParentSpanID, Depth: depth, Participant: e.Participant,
			Operation: e.Operation, Kind: string(e.Kind), Status: string(e.Status), StartTime: e.StartTime,
			Duration: e.Duration, Peer: e.Peer, Attributes: attrs,
		})
		for _, c := range n.Children {
			walk(c, depth+1)
		}
	}
	for _, r := range trace.Roots {
		walk(r, 0)
	}
	return rows
}

type TraceDetail struct {
	Summary         TraceSummary `json:"summary"`
	Sequence        Sequence     `json:"sequence"`
	MermaidSequence string       `json:"mermaidSequence"`
	Spans           []SpanRow    `json:"spans"`
	CriticalPath    []string     `json:"criticalPath"` // span ids that determine the trace's end time
}

func Detail(trace *AssembledTrace) TraceDetail {
	seq := SequenceFor(trace)
	return TraceDetail{
		Summary: Summarize(trace), Sequence: seq, MermaidSequence: ToMermaidSequence(seq),
		Spans: flattenSpans(trace), CriticalPath: CriticalPath(trace),
	}
}
