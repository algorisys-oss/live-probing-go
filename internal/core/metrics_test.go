package core

import (
	"math"
	"strconv"
	"testing"
)

// Ports of red-metrics.test.ts, alert-rules.test.ts, flamegraph.test.ts, latency-buckets.test.ts.

var redSeq int

func redEv(participant string, kind SpanKind, start, dur int64, status SpanStatus) Event {
	redSeq++
	return Event{TraceID: "t", SpanID: "sp" + strconv.Itoa(redSeq), Participant: participant, Operation: "op", Kind: kind, StartTime: start, Duration: dur, Status: status, Attributes: Attributes{}}
}

func traceOf(events ...Event) *AssembledTrace {
	return &AssembledTrace{TraceID: "t", Roots: []*TraceNode{}, Events: events}
}

func TestRedMetricsAggregatesServerSpans(t *testing.T) {
	// gw handles 4 requests over a 2s window: durations 1,2,3,4 ms, one error.
	m := RedMetrics([]*AssembledTrace{traceOf(
		redEv("gw", KindServer, 0, 1000, StatusOK),
		redEv("gw", KindServer, 500_000, 2000, StatusOK),
		redEv("gw", KindServer, 1_000_000, 3000, StatusError),
		redEv("gw", KindServer, 2_000_000-4000, 4000, StatusOK),
	)})[0]
	eq(t, m.Service, "gw", "service")
	eq(t, m.Calls, 4, "calls")
	eq(t, m.Errors, 1, "errors")
	eq(t, m.ErrorRate, 0.25, "errorRate")
	eq(t, m.RatePerSec, 2, "window 0..2s -> 4/2 = 2/s")
	eq(t, m.P95Micros, 4000, "p95: floor(4*0.95)=3 -> 4000")
}

func TestRedMetricsIgnoresNonServerSpans(t *testing.T) {
	out := RedMetrics([]*AssembledTrace{traceOf(
		redEv("gw", KindServer, 0, 1000, StatusOK),
		redEv("gw", KindClient, 0, 9999, StatusOK),
		redEv("gw", KindInternal, 0, 5000, StatusOK),
	)})
	eq(t, out[0].Calls, 1, "calls")
	eq(t, out[0].P95Micros, 1000, "p95")
}

func TestRedMetricsOmitsDatastores(t *testing.T) {
	c := redEv("gw", KindClient, 0, 500, StatusOK)
	c.Peer = "postgresql"
	out := RedMetrics([]*AssembledTrace{traceOf(redEv("gw", KindServer, 0, 1000, StatusOK), c)})
	eq(t, len(out), 1, "tiles")
	eq(t, out[0].Service, "gw", "only gw")
}

func TestRedMetricsSortedByVolume(t *testing.T) {
	out := RedMetrics([]*AssembledTrace{traceOf(
		redEv("a", KindServer, 0, 1000, StatusOK),
		redEv("b", KindServer, 0, 1000, StatusOK),
		redEv("b", KindServer, 0, 1000, StatusOK),
		redEv("b", KindServer, 0, 1000, StatusOK),
	)})
	eqSlice(t, []string{out[0].Service, out[1].Service}, []string{"b", "a"}, "order")
}

func TestRedMetricsEmpty(t *testing.T) {
	eq(t, len(RedMetrics(nil)), 0, "no tiles")
}

func svc(service string, errorRate float64) RedMetric {
	return RedMetric{Service: service, RatePerSec: 1, ErrorRate: errorRate, P95Micros: 1000, Calls: 100, Errors: int(jsRound(errorRate * 100))}
}

func edge(from, to string, calls int, avg int64) Edge {
	return Edge{From: from, To: to, Calls: calls, AvgDurationMicros: avg}
}

func TestAlertsServiceErrorRate(t *testing.T) {
	alerts := EvaluateAlerts([]RedMetric{svc("order", 0.08), svc("cart", 0.02), svc("auth", 0)}, nil, DefaultAlertThresholds)
	by := map[string]string{}
	for _, a := range alerts {
		by[a.Target] = a.Severity
		if a.Metric != "error-rate" || a.Kind != "service" {
			t.Errorf("unexpected alert %+v", a)
		}
	}
	eq(t, by["order"], "error", "order")
	eq(t, by["cart"], "warn", "cart")
	_, auth := by["auth"]
	eq(t, auth, false, "auth not alerting")
}

func TestAlertsEdgeLatency(t *testing.T) {
	// median of [1000,1000,1000,5000] = 1000 -> cutoff 2000
	alerts := EvaluateAlerts(nil, []Edge{
		edge("a", "b", 100, 1000), edge("a", "c", 100, 1000), edge("a", "d", 100, 1000), edge("gw", "slow", 100, 5000),
	}, DefaultAlertThresholds)
	eq(t, len(alerts), 1, "alerts")
	a := alerts[0]
	eq(t, a.Target, "gw→slow", "target")
	eq(t, a.Metric, "latency", "metric")
	eq(t, a.Severity, "warn", "severity")
	eq(t, a.Threshold, 2000, "threshold")
	eq(t, a.Message, "gw→slow avg 5ms ≥ 2ms (2× median)", "message")
}

func TestAlertIDsStable(t *testing.T) {
	a1 := EvaluateAlerts([]RedMetric{svc("order", 0.08)}, nil, DefaultAlertThresholds)
	a2 := EvaluateAlerts([]RedMetric{svc("order", 0.09)}, nil, DefaultAlertThresholds)
	eq(t, a1[0].ID, a2[0].ID, "same id despite a different value")
	eq(t, a1[0].ID, "service:order:error-rate", "id")
	eq(t, a1[0].Message, "order error rate 8.0% (8/100)", "message")
}

func TestAlertsNoSpread(t *testing.T) {
	eq(t, len(EvaluateAlerts(nil, []Edge{edge("a", "b", 100, 1000)}, DefaultAlertThresholds)), 0, "alerts")
}

func TestAlertsEmpty(t *testing.T) {
	eq(t, len(EvaluateAlerts(nil, nil, DefaultAlertThresholds)), 0, "alerts")
}

func TestAlertThresholdsOverridable(t *testing.T) {
	strict := DefaultAlertThresholds
	strict.ErrorRateError = 0.02
	eq(t, EvaluateAlerts([]RedMetric{svc("x", 0.03)}, nil, strict)[0].Severity, "error", "strict")
	eq(t, EvaluateAlerts([]RedMetric{svc("x", 0.03)}, nil, DefaultAlertThresholds)[0].Severity, "warn", "default")
}

func fs(spanID, parent, participant, operation string, dur int64) FlameSpan {
	return FlameSpan{SpanID: spanID, ParentSpanID: parent, Participant: participant, Operation: operation, Duration: dur}
}

func flameChild(t *testing.T, n *FlameNode, participant, operation string) *FlameNode {
	t.Helper()
	for _, c := range n.Children {
		if c.Participant == participant && c.Operation == operation {
			return c
		}
	}
	t.Fatalf("no child %s/%s", participant, operation)
	return nil
}

func TestFlamegraphMergesByPath(t *testing.T) {
	t1 := []FlameSpan{fs("a", "", "gw", "GET /x", 100), fs("b", "a", "gw", "query", 40)}
	t2 := []FlameSpan{fs("a", "", "gw", "GET /x", 120), fs("b", "a", "gw", "query", 60)}
	root := AggregateFlamegraph([][]FlameSpan{t1, t2})
	eq(t, len(root.Children), 1, "one endpoint")
	ep := root.Children[0]
	eq(t, ep.Operation, "GET /x", "op")
	eq(t, ep.TotalMicros, 220, "total")
	eq(t, ep.Count, 2, "count")
	q := flameChild(t, ep, "gw", "query")
	eq(t, q.TotalMicros, 100, "query total")
	eq(t, q.Count, 2, "query count")
	eq(t, ep.SelfMicros, 120, "self = total - children")
	eq(t, q.SelfMicros, 100, "leaf: all self")
}

func TestFlamegraphSameOpDifferentParents(t *testing.T) {
	ep := AggregateFlamegraph([][]FlameSpan{{
		fs("r", "", "gw", "GET /x", 100),
		fs("a", "r", "auth", "verify", 30), fs("ar", "a", "redis", "get", 10),
		fs("c", "r", "cart", "list", 40), fs("cr", "c", "redis", "get", 15),
	}}).Children[0]
	eq(t, flameChild(t, flameChild(t, ep, "auth", "verify"), "redis", "get").TotalMicros, 10, "under auth")
	eq(t, flameChild(t, flameChild(t, ep, "cart", "list"), "redis", "get").TotalMicros, 15, "under cart")
}

func TestFlamegraphCountsRepeatedSiblings(t *testing.T) {
	q := flameChild(t, AggregateFlamegraph([][]FlameSpan{{
		fs("r", "", "gw", "GET /x", 100), fs("q1", "r", "db", "query", 5), fs("q2", "r", "db", "query", 6), fs("q3", "r", "db", "query", 7),
	}}).Children[0], "db", "query")
	eq(t, q.Count, 3, "count")
	eq(t, q.TotalMicros, 18, "total")
}

func TestFlamegraphOrphanIsRoot(t *testing.T) {
	root := AggregateFlamegraph([][]FlameSpan{{fs("orphan", "missing", "svc", "op", 50)}})
	eq(t, root.Children[0].Operation, "op", "op")
	eq(t, root.Children[0].TotalMicros, 50, "total")
}

func TestFlamegraphEmpty(t *testing.T) {
	eq(t, len(AggregateFlamegraph(nil).Children), 0, "children")
}

func TestLatencyBucketIndex(t *testing.T) {
	cases := map[float64]int{0: 0, 999: 0, 1_000: 1, 1_500: 1, 4_999: 2, 60_000: 6, 2_499_999: 10, 2_500_000: 11, 9_999_999: 11}
	for d, want := range cases {
		eq(t, LatencyBucketIndex(d), want, "bucket for "+jsNumberString(d))
	}
}

func TestLatencyEdgesAreLowerExclusive(t *testing.T) {
	for i, e := range LatencyEdgesMicros {
		eq(t, LatencyBucketIndex(float64(e)), i+1, "edge")
		eq(t, LatencyBucketIndex(float64(e-1)), i, "edge-1")
	}
}

func TestLatencyLabelsLineUp(t *testing.T) {
	eq(t, NumLatencyBuckets, len(LatencyEdgesMicros)+1, "buckets")
	eq(t, len(LatencyBucketLabels), NumLatencyBuckets, "labels")
}

func TestLatencyNonFinite(t *testing.T) {
	eq(t, LatencyBucketIndex(math.NaN()), 0, "NaN")
	eq(t, LatencyBucketIndex(math.Inf(1)), 0, "Inf")
}
