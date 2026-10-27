package view

import (
	"slices"
	"testing"

	"liveprobe/internal/core"
)

// Ports of ui/src/lib/topology-health.test.ts and dependency-matrix.test.ts, plus format/layout.

func edge(from, to string, calls, errors int, avg int64) core.Edge {
	return core.Edge{From: from, To: to, Calls: calls, Errors: errors, AvgDurationMicros: avg}
}

func topo(nodes []string, edges ...core.Edge) core.Topology {
	return core.Topology{Nodes: nodes, Edges: edges, Externals: []string{}}
}

func TestEdgeErrorRateBands(t *testing.T) {
	h := TopologyHealth(topo([]string{"a", "b", "c", "d"}, edge("a", "b", 100, 6, 1000), edge("a", "c", 100, 2, 1000), edge("a", "d", 100, 0, 1000)), DefaultThresholds)
	if !slices.Equal(h.EdgeHealth, []Health{Error, Warn, OK}) {
		t.Errorf("edge health: %v", h.EdgeHealth)
	}
}

func TestEdgeLatencyIsOnlyEverAmber(t *testing.T) {
	h := TopologyHealth(topo([]string{"a", "b", "c", "d", "e"},
		edge("a", "b", 100, 0, 1000), edge("a", "c", 100, 0, 1000), edge("a", "d", 100, 0, 1000), edge("a", "e", 100, 0, 5000)), DefaultThresholds)
	if h.MedianLatencyMicros != 1000 || !slices.Equal(h.EdgeHealth, []Health{OK, OK, OK, Warn}) {
		t.Errorf("median %v health %v", h.MedianLatencyMicros, h.EdgeHealth)
	}
}

func TestErrorDominatesLatency(t *testing.T) {
	h := TopologyHealth(topo([]string{"a", "b", "c"}, edge("a", "b", 100, 0, 1000), edge("a", "c", 100, 10, 9000)), DefaultThresholds)
	if h.EdgeHealth[1] != Error {
		t.Errorf("got %v", h.EdgeHealth[1])
	}
}

func TestNodeHealthPointsAtCallee(t *testing.T) {
	h := TopologyHealth(topo([]string{"gateway", "auth", "redis"}, edge("gateway", "auth", 100, 8, 1000), edge("auth", "redis", 100, 0, 1000)), DefaultThresholds)
	if h.NodeHealth["auth"] != Error || h.NodeHealth["gateway"] != OK || h.NodeHealth["redis"] != OK {
		t.Errorf("node health: %v", h.NodeHealth)
	}
}

func TestNodeErrorRateAggregatesInbound(t *testing.T) {
	h := TopologyHealth(topo([]string{"x", "y", "svc"}, edge("x", "svc", 100, 2, 1000), edge("y", "svc", 100, 2, 1000)), DefaultThresholds)
	if h.NodeHealth["svc"] != Warn {
		t.Errorf("svc: %v", h.NodeHealth["svc"])
	}
}

func TestEmptyAndSingleEdgeHealth(t *testing.T) {
	h := TopologyHealth(topo(nil), DefaultThresholds)
	if len(h.EdgeHealth) != 0 || len(h.NodeHealth) != 0 || h.MedianLatencyMicros != 0 {
		t.Errorf("empty: %+v", h)
	}
	one := TopologyHealth(topo([]string{"a", "b"}, edge("a", "b", 5, 0, 1000)), DefaultThresholds)
	if one.EdgeHealth[0] != OK || one.NodeHealth["b"] != OK {
		t.Errorf("single: %+v", one)
	}
}

func TestHealthThresholdsOverridable(t *testing.T) {
	tp := topo([]string{"a", "b"}, edge("a", "b", 100, 3, 1000))
	strict := DefaultThresholds
	strict.ErrorRateError = 0.02
	if TopologyHealth(tp, DefaultThresholds).EdgeHealth[0] != Warn || TopologyHealth(tp, strict).EdgeHealth[0] != Error {
		t.Error("threshold override")
	}
}

func TestMatrixRowsColsCells(t *testing.T) {
	m := DependencyMatrix(topo([]string{"gateway", "order", "auth", "postgresql"},
		edge("gateway", "order", 100, 0, 1000), edge("gateway", "auth", 100, 0, 1000), edge("order", "postgresql", 100, 0, 1000)), DefaultThresholds)
	if !slices.Equal(m.Rows, []string{"gateway", "order"}) || !slices.Equal(m.Cols, []string{"auth", "order", "postgresql"}) || len(m.Cells) != 3 {
		t.Errorf("matrix: %+v", m)
	}
	if _, ok := m.Cell("gateway", "order"); !ok {
		t.Error("gateway→order cell")
	}
	if _, ok := m.Cell("gateway", "postgresql"); ok {
		t.Error("no such call")
	}
}

func TestMatrixCellCarriesHealth(t *testing.T) {
	m := DependencyMatrix(topo([]string{"a", "b", "c", "d"}, edge("a", "b", 100, 8, 1000), edge("a", "c", 100, 2, 1000), edge("a", "d", 100, 0, 1000)), DefaultThresholds)
	ab, _ := m.Cell("a", "b")
	ac, _ := m.Cell("a", "c")
	ad, _ := m.Cell("a", "d")
	if ab.Calls != 100 || ab.Errors != 8 || ab.ErrorRate != 0.08 || ab.AvgDurationMicros != 1000 || ab.Health != Error || ac.Health != Warn || ad.Health != OK {
		t.Errorf("cells: %+v %+v %+v", ab, ac, ad)
	}
}

func TestMatrixLatencyAmberAndSelfLoops(t *testing.T) {
	m := DependencyMatrix(topo([]string{"a", "b", "c", "d", "slow"},
		edge("a", "b", 100, 0, 1000), edge("a", "c", 100, 0, 1000), edge("a", "d", 100, 0, 1000), edge("a", "slow", 100, 0, 5000)), DefaultThresholds)
	if c, _ := m.Cell("a", "slow"); c.Health != Warn || m.MedianLatencyMicros != 1000 {
		t.Errorf("slow cell %+v median %v", c, m.MedianLatencyMicros)
	}
	self := DependencyMatrix(topo([]string{"a", "b"}, edge("a", "a", 50, 0, 1000), edge("a", "b", 100, 0, 1000)), DefaultThresholds)
	if !slices.Equal(self.Rows, []string{"a"}) || !slices.Equal(self.Cols, []string{"b"}) || len(self.Cells) != 1 {
		t.Errorf("self loop: %+v", self)
	}
	empty := DependencyMatrix(topo(nil), DefaultThresholds)
	if len(empty.Rows)+len(empty.Cols)+len(empty.Cells) != 0 {
		t.Error("empty matrix")
	}
}

func TestFormatMicros(t *testing.T) {
	cases := map[float64]string{0: "0µs", 999.4: "999µs", 999.5: "1000µs", 1000: "1.0ms", 9999: "10.0ms", 12_345: "12ms",
		999_499: "999ms", 1_000_000: "1.00s", 9_999_999: "10.00s", 12_340_000: "12.3s"}
	for in, want := range cases {
		if got := FormatMicros(in); got != want {
			t.Errorf("FormatMicros(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestIsDatastoreAndHash(t *testing.T) {
	for _, ds := range []string{"postgresql", "Redis", "orders-postgresql", "cache:redis", "postgres"} {
		if !IsDatastore(ds) {
			t.Errorf("%s should be a datastore", ds)
		}
	}
	if IsDatastore("gateway") {
		t.Error("gateway is not a datastore")
	}
	// FNV-1a over UTF-16 code units, as the TS hashString (values from Node).
	if HashString("gateway") != 3888792057 || HashString("") != 2166136261 || HashString("é") != 1812687940 {
		t.Errorf("hash: %d %d %d", HashString("gateway"), HashString(""), HashString("é"))
	}
}

func TestLayoutColumnsAndDeterminism(t *testing.T) {
	nodes := []string{"gateway", "auth", "catalog", "postgresql", "order", "rabbitmq"}
	edges := []core.Edge{edge("gateway", "auth", 1, 0, 1), edge("gateway", "catalog", 1, 0, 1), edge("catalog", "postgresql", 1, 0, 1),
		edge("gateway", "order", 1, 0, 1), edge("order", "rabbitmq", 1, 0, 1)}
	l := ComputeLayout(nodes, edges)
	if l.Positions["gateway"].X != 80 { // source → column 0
		t.Errorf("gateway x %v", l.Positions["gateway"].X)
	}
	if l.Positions["postgresql"].X != l.Positions["rabbitmq"].X || l.Positions["postgresql"].X <= l.Positions["order"].X {
		t.Error("datastores share the far-right column")
	}
	again := ComputeLayout(slices.Clone(nodes), edges)
	for _, n := range nodes {
		if again.Positions[n] != l.Positions[n] {
			t.Errorf("layout not deterministic for %s", n)
		}
	}
}
