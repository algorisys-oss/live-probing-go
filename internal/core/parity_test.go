package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Differential parity, Go side. Runs the inputs written by tools/parity/core-parity.ts through
// the port and writes go-output.json in the same shape as ts-output.json. Skipped unless
// LIVEPROBE_PARITY_DIR is set; see tools/parity/README.md.
func TestParityWithTypeScript(t *testing.T) {
	dir := os.Getenv("LIVEPROBE_PARITY_DIR")
	if dir == "" {
		t.Skip("LIVEPROBE_PARITY_DIR not set")
	}
	rawInput, err := os.ReadFile(filepath.Join(dir, "input.json"))
	if err != nil {
		t.Fatal(err)
	}
	var input any
	if err := json.Unmarshal(rawInput, &input); err != nil {
		t.Fatal(err)
	}
	rawOTLP, err := os.ReadFile(filepath.Join(dir, "otlp.json"))
	if err != nil {
		t.Fatal(err)
	}
	otlp, err := DecodeOTLP(rawOTLP)
	if err != nil {
		t.Fatal(err)
	}

	coerced := CoerceEvents(input)
	for i := range coerced {
		if coerced[i].TraceID == "hostile" && coerced[i].SpanID == "future" {
			coerced[i].StartTime = 0
		}
	}
	otlpEvents := NormalizeOTLP(otlp)
	masked := MaskEvents(coerced, MaskOptions{})

	w := NewTraceWindow(WindowOptions{MaxTraces: 1e9, HorizonMicros: 1e15})
	w.Add(masked)
	traceIDs := w.TraceIDs()
	traces := make([]*AssembledTrace, len(traceIDs))
	perTrace := make([]map[string]any, len(traceIDs))
	for i, id := range traceIDs {
		tr := w.Assemble(id)
		traces[i] = tr
		seq := SequenceFor(tr)
		layout := LayoutSequence(seq, nil)
		all := map[string]bool{}
		for _, g := range layout.Groups {
			all[g.ID] = true
		}
		perTrace[i] = map[string]any{
			"assembled":    tr,
			"sequence":     seq,
			"layout":       layout,
			"expanded":     LayoutSequence(seq, all),
			"criticalPath": CriticalPath(tr),
			"mermaid":      ToMermaidSequence(seq),
		}
	}
	topology := w.Topology()
	red := RedMetrics(traces)

	type flame struct {
		Op   string     `json:"op"`
		Root *FlameNode `json:"root"`
	}
	byRoot := map[string][][]FlameSpan{}
	var rootOrder []string
	for _, tr := range traces {
		op := ""
		if len(tr.Roots) > 0 {
			op = tr.Roots[0].Event.Operation
		}
		if _, ok := byRoot[op]; !ok {
			rootOrder = append(rootOrder, op)
		}
		spans := make([]FlameSpan, len(tr.Events))
		for j, e := range tr.Events {
			spans[j] = FlameSpan{SpanID: e.SpanID, ParentSpanID: e.ParentSpanID, Operation: e.Operation, Participant: e.Participant, Duration: e.Duration}
		}
		byRoot[op] = append(byRoot[op], spans)
	}
	flamegraphs := []flame{}
	for _, op := range rootOrder {
		flamegraphs = append(flamegraphs, flame{op, AggregateFlamegraph(byRoot[op])})
	}

	small := NewTraceWindow(WindowOptions{MaxTraces: 300, HorizonMicros: 120_000_000})
	for i := 0; i < len(coerced); i += 500 {
		small.Add(coerced[i:min(i+500, len(coerced))])
	}
	buckets := make([]int, len(coerced))
	for i, e := range coerced {
		buckets[i] = LatencyBucketIndex(float64(e.Duration))
	}

	out, err := json.Marshal(map[string]any{
		"coerced":        coerced,
		"otlpEvents":     otlpEvents,
		"masked":         masked,
		"traceIds":       traceIDs,
		"perTrace":       perTrace,
		"topology":       topology,
		"red":            red,
		"alerts":         EvaluateAlerts(red, topology.Edges, DefaultAlertThresholds),
		"mermaidFlow":    ToMermaidFlow(topology),
		"flamegraphs":    flamegraphs,
		"buckets":        buckets,
		"smallWindowIds": small.TraceIDs(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go-output.json"), out, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("go: %d traces -> %s", len(traceIDs), dir)
}
