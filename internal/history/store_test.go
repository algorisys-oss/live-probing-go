package history

import (
	"encoding/json"
	"math"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	"liveprobe/internal/core"
)

// Port of packages/server/src/history-store.test.ts.

func open(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func must[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func checkoutTrace(t *testing.T) *core.AssembledTrace {
	// 1e9 ns -> 1e6 us -> 1970-01-01
	body := `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"gateway"}}]},
	"scopeSpans":[{"spans":[
	 {"traceId":"t1","spanId":"g1","name":"POST","kind":2,"startTimeUnixNano":"1000000000","endTimeUnixNano":"1010000000",
	  "attributes":[{"key":"http.method","value":{"stringValue":"POST"}},{"key":"http.target","value":{"stringValue":"/api/checkout"}}],"status":{"code":2}},
	 {"traceId":"t1","spanId":"g2","parentSpanId":"g1","name":"GET","kind":3,"startTimeUnixNano":"1001000000","endTimeUnixNano":"1005000000",
	  "attributes":[{"key":"db.system","value":{"stringValue":"postgresql"}}],"status":{"code":0}}]}]}]}`
	p, err := core.DecodeOTLP([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	w := core.NewTraceWindow(core.WindowOptions{})
	w.Add(core.NormalizeOTLP(p))
	return w.Assemble("t1")
}

func TestPersistsAndAggregatesByDay(t *testing.T) {
	trace := checkoutTrace(t)
	s := open(t, ":memory:")
	for range 2 { // idempotent upsert (trace grows) — still one row
		if err := s.UpsertMany([]*core.AssembledTrace{trace}); err != nil {
			t.Fatal(err)
		}
	}

	days := must[[]DayInfo](t)(s.Days())
	if len(days) != 1 || days[0].Day != "1970-01-01" || days[0].Requests != 1 || days[0].Errors != 1 {
		t.Fatalf("days: %+v", days)
	}

	sum := must[DaySummary](t)(s.DaySummary("1970-01-01"))
	if sum.Requests != 1 || sum.ErrorRate != 1 || sum.P95Micros <= 0 {
		t.Errorf("summary: %+v", sum)
	}
	if sum.TopEndpoints[0].Operation != "POST /api/checkout" || sum.Slowest[0].TraceID != "t1" || len(sum.Throughput) < 1 {
		t.Errorf("summary rollups: %+v", sum)
	}

	list := must[[]core.TraceSummary](t)(s.DayTraces("1970-01-01", 10))
	if len(list) != 1 || list[0].RootOperation != "POST /api/checkout" {
		t.Errorf("day traces: %+v", list)
	}

	raw := must[json.RawMessage](t)(s.GetDetail("t1"))
	var d core.TraceDetail
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`sequenceDiagram`).MatchString(d.MermaidSequence) {
		t.Errorf("detail mermaid: %q", d.MermaidSequence)
	}
}

const day = int64(86_400_000_000) // one UTC day in micros

func traceDur(id string, start, dur int64) *core.AssembledTrace {
	w := core.NewTraceWindow(core.WindowOptions{HorizonMicros: math.MaxInt64})
	w.Add([]core.Event{{TraceID: id, SpanID: id + "-s", Participant: "gw", Operation: "GET /x", Kind: core.KindServer, StartTime: start, Duration: dur, Status: core.StatusOK, Attributes: core.Attributes{}}})
	return w.Assemble(id)
}

func dayNames(t *testing.T, s *Store) []string {
	var out []string
	for _, d := range must[[]DayInfo](t)(s.Days()) {
		out = append(out, d.Day)
	}
	return out
}

func TestPruneDropsOldDays(t *testing.T) {
	s := open(t, ":memory:")
	if err := s.UpsertMany([]*core.AssembledTrace{
		traceDur("old", 1_000_000, 1000), traceDur("mid", 4*day+1_000_000, 1000), traceDur("new", 9*day+1_000_000, 1000),
	}); err != nil {
		t.Fatal(err)
	}
	// Keep the 7 most recent days including "today" (1970-01-10): cutoff is 1970-01-04.
	dropped := must[int](t)(s.Prune(7, 9*day+12*3_600_000_000))
	if dropped != 1 {
		t.Errorf("dropped %d", dropped)
	}
	if got := dayNames(t, s); !slices.Equal(got, []string{"1970-01-10", "1970-01-05"}) {
		t.Errorf("days: %v", got)
	}
	if raw := must[json.RawMessage](t)(s.GetDetail("old")); raw != nil {
		t.Error("detail should be gone with the day")
	}
}

func TestPruneDisabled(t *testing.T) {
	s := open(t, ":memory:")
	if err := s.UpsertMany([]*core.AssembledTrace{traceDur("old", 1_000_000, 1000)}); err != nil {
		t.Fatal(err)
	}
	for _, r := range []int{0, -1} {
		if n := must[int](t)(s.Prune(r, 9*day)); n != 0 {
			t.Errorf("prune(%d) = %d", r, n)
		}
	}
	if len(dayNames(t, s)) != 1 {
		t.Error("day should remain")
	}
}

func TestSearchFilters(t *testing.T) {
	s := open(t, ":memory:")
	if err := s.UpsertMany([]*core.AssembledTrace{checkoutTrace(t)}); err != nil {
		t.Fatal(err)
	}
	yes, no := true, false
	cases := []struct {
		name string
		opts SearchOptions
		want int
	}{
		{"q hit", SearchOptions{Q: "checkout"}, 1},
		{"q miss", SearchOptions{Q: "nonexistent"}, 0},
		{"service hit", SearchOptions{Service: "gateway"}, 1},
		{"service miss", SearchOptions{Service: "catalog"}, 0},
		{"error true", SearchOptions{Error: &yes}, 1},
		{"error false", SearchOptions{Error: &no}, 0},
		{"min 5ms", SearchOptions{MinMicros: 5_000}, 1},
		{"min 50ms", SearchOptions{MinMicros: 50_000}, 0},
		{"max 5ms", SearchOptions{MaxMicros: 5_000}, 0},
		{"max 20ms", SearchOptions{MaxMicros: 20_000}, 1},
		{"min spans 2", SearchOptions{MinSpans: 2}, 1},
		{"min spans 3", SearchOptions{MinSpans: 3}, 0},
		{"slowest", SearchOptions{Sort: "slowest"}, 1},
		{"trace id", SearchOptions{TraceID: "t1"}, 1},
		{"attr pair", SearchOptions{Attr: "db.system=postgresql"}, 1},
		{"attr word", SearchOptions{Attr: "postgresql"}, 1},
		{"attr miss", SearchOptions{Attr: "db.system=redis"}, 0},
	}
	for _, c := range cases {
		if got := len(must[[]core.TraceSummary](t)(s.Search(c.opts))); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

func TestEndpointDistribution(t *testing.T) {
	s := open(t, ":memory:")
	if err := s.UpsertMany([]*core.AssembledTrace{
		traceDur("a", 1_000_000, 500),     // minute 0, bucket 0
		traceDur("b", 2_000_000, 1_500),   // minute 0, bucket 1
		traceDur("c", 3_000_000, 1_500),   // minute 0, bucket 1
		traceDur("d", 61_000_000, 60_000), // minute 1, bucket 6
	}); err != nil {
		t.Fatal(err)
	}
	dist := must[LatencyDistribution](t)(s.EndpointDistribution("1970-01-01", "GET /x"))
	if len(dist.Histogram) != 12 || dist.Histogram[0] != 1 || dist.Histogram[1] != 2 || dist.Histogram[6] != 1 {
		t.Errorf("histogram: %v", dist.Histogram)
	}
	if len(dist.Heatmap) != 2 || dist.Heatmap[0].Minute >= dist.Heatmap[1].Minute ||
		dist.Heatmap[0].Counts[0] != 1 || dist.Heatmap[0].Counts[1] != 2 || dist.Heatmap[1].Counts[6] != 1 {
		t.Errorf("heatmap: %+v", dist.Heatmap)
	}
	other := must[LatencyDistribution](t)(s.EndpointDistribution("1970-01-01", "GET /nope"))
	sum := 0
	for _, c := range other.Histogram {
		sum += c
	}
	if sum != 0 || len(other.Heatmap) != 0 {
		t.Errorf("other endpoint: %+v", other)
	}
}

func TestEndpointLatencyAndFlamegraphAndErrors(t *testing.T) {
	s := open(t, ":memory:")
	if err := s.UpsertMany([]*core.AssembledTrace{checkoutTrace(t), traceDur("a", 1_000_000, 500), traceDur("b", 2_000_000, 1_500)}); err != nil {
		t.Fatal(err)
	}
	lat := must[[]LatencyBucket](t)(s.EndpointLatency("1970-01-01", "GET /x"))
	if len(lat) != 1 || lat[0].Count != 2 || lat[0].P50 != 1_500 {
		t.Errorf("latency: %+v", lat)
	}
	flame := must[*core.FlameNode](t)(s.EndpointFlamegraph("1970-01-01", "POST /api/checkout", 150))
	if len(flame.Children) != 1 || flame.Children[0].TotalMicros != 10_000 || len(flame.Children[0].Children) != 1 {
		t.Errorf("flame: %+v", flame)
	}
	groups := must[[]ErrorGroup](t)(s.ErrorGroups(100))
	if len(groups) != 1 || groups[0].Endpoint != "POST /api/checkout" || groups[0].Label != "gateway: POST /api/checkout" || groups[0].SampleTraceID != "t1" {
		t.Errorf("error groups: %+v", groups)
	}
}

func TestReopensFileDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertMany([]*core.AssembledTrace{traceDur("a", 1_000_000, 500)}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2 := open(t, path)
	if len(dayNames(t, s2)) != 1 {
		t.Error("data should survive a reopen")
	}
}
