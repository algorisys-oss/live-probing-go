package core

import (
	"cmp"
	"math"
	"slices"
)

// RedMetric is RED (Rate, Errors, Duration) for one service over the live window. Computed from
// server-kind spans, since those are the requests a service actually handled; datastores and
// ghosts have no server span and are omitted.
type RedMetric struct {
	Service    string  `json:"service"`
	RatePerSec float64 `json:"ratePerSec"`
	ErrorRate  float64 `json:"errorRate"` // 0..1
	P95Micros  int64   `json:"p95Micros"`
	Calls      int     `json:"calls"`
	Errors     int     `json:"errors"`
}

func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[min(len(sorted)-1, int(math.Floor(float64(len(sorted))*p)))]
}

// RedMetrics computes per-service RED over assembled traces, most-trafficked first. Rate uses a
// shared denominator — the observed span time range — so services are comparable.
func RedMetrics(traces []*AssembledTrace) []RedMetric {
	type acc struct {
		durations []int64
		errors    int
	}
	byService := map[string]*acc{}
	var order []string
	minStart, maxEnd := int64(math.MaxInt64), int64(math.MinInt64)

	for _, t := range traces {
		for _, e := range t.Events {
			if e.Kind != KindServer {
				continue
			}
			s := byService[e.Participant]
			if s == nil {
				s = &acc{}
				byService[e.Participant] = s
				order = append(order, e.Participant)
			}
			s.durations = append(s.durations, e.Duration)
			if e.Status == StatusError {
				s.errors++
			}
			minStart = min(minStart, e.StartTime)
			maxEnd = max(maxEnd, e.StartTime+e.Duration)
		}
	}

	windowSec := 1.0
	if len(order) > 0 {
		windowSec = max(1, float64(maxEnd-minStart)/1_000_000)
	}

	out := make([]RedMetric, 0, len(order))
	for _, name := range order {
		s := byService[name]
		sorted := slices.Clone(s.durations)
		slices.Sort(sorted)
		calls := len(sorted)
		m := RedMetric{Service: name, Calls: calls, Errors: s.errors, P95Micros: percentile(sorted, 0.95), RatePerSec: float64(calls) / windowSec}
		if calls > 0 {
			m.ErrorRate = float64(s.errors) / float64(calls)
		}
		out = append(out, m)
	}
	slices.SortStableFunc(out, func(a, b RedMetric) int { return cmp.Compare(b.Calls, a.Calls) })
	return out
}

// Alert is a firing condition derived from the live window. Error-rate alerts fire on the
// *service* handling the failing requests; latency alerts fire on the *edge* (the specific slow
// call). `since` is stamped by the server, not here — evaluation stays pure.
type Alert struct {
	ID        string  `json:"id"`     // stable across ticks, so the same condition de-dupes
	Kind      string  `json:"kind"`   // "service" | "edge"
	Target    string  `json:"target"` // "order" or "gateway→order"
	Metric    string  `json:"metric"` // "error-rate" | "latency"
	Severity  string  `json:"severity"`
	Value     float64 `json:"value"`     // error rate 0..1, or latency µs
	Threshold float64 `json:"threshold"` // the boundary it crossed
	Message   string  `json:"message"`
}

type AlertThresholds struct {
	ErrorRateError    float64
	ErrorRateWarn     float64
	LatencyWarnFactor float64 // edge latency ≥ factor × median edge latency
}

var DefaultAlertThresholds = AlertThresholds{ErrorRateError: 0.05, ErrorRateWarn: 0.01, LatencyWarnFactor: 2}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

func fmtMs(micros float64) string {
	if micros >= 1000 {
		return jsNumberString(jsRound(micros/1000)) + "ms"
	}
	return jsNumberString(jsRound(micros)) + "µs"
}

func fmtPct(r float64) string {
	digits := 1
	if r >= 0.1 {
		digits = 0
	}
	return toFixed(r*100, digits) + "%"
}

// EvaluateAlerts evaluates active alerts from a snapshot of the live window. Pure and
// order-independent. Same thresholds as the topology health colouring, so the graph, the RED
// tiles, and the alerts all agree on what "red" means.
func EvaluateAlerts(services []RedMetric, edges []Edge, t AlertThresholds) []Alert {
	alerts := []Alert{}
	for _, s := range services {
		var sev string
		var threshold float64
		switch {
		case s.ErrorRate >= t.ErrorRateError:
			sev, threshold = "error", t.ErrorRateError
		case s.ErrorRate >= t.ErrorRateWarn:
			sev, threshold = "warn", t.ErrorRateWarn
		default:
			continue
		}
		alerts = append(alerts, Alert{
			ID: "service:" + s.Service + ":error-rate", Kind: "service", Target: s.Service,
			Metric: "error-rate", Severity: sev, Value: s.ErrorRate, Threshold: threshold,
			Message: s.Service + " error rate " + fmtPct(s.ErrorRate) + " (" + itoa(s.Errors) + "/" + itoa(s.Calls) + ")",
		})
	}

	var durs []float64
	for _, e := range edges {
		if e.Calls > 0 {
			durs = append(durs, float64(e.AvgDurationMicros))
		}
	}
	if med := median(durs); med > 0 {
		cutoff := med * t.LatencyWarnFactor
		for _, e := range edges {
			if e.Calls > 0 && float64(e.AvgDurationMicros) >= cutoff {
				target := e.From + "→" + e.To
				alerts = append(alerts, Alert{
					ID: "edge:" + e.From + "->" + e.To + ":latency", Kind: "edge", Target: target,
					Metric: "latency", Severity: "warn", Value: float64(e.AvgDurationMicros), Threshold: cutoff,
					Message: target + " avg " + fmtMs(float64(e.AvgDurationMicros)) + " ≥ " + fmtMs(cutoff) + " (2× median)",
				})
			}
		}
	}
	return alerts
}

func itoa(n int) string { return jsNumberString(float64(n)) }

// LatencyEdgesMicros are fixed log-ish latency bucket boundaries shared by the distribution
// histogram and the time×latency heatmap. Durations span orders of magnitude, so linear buckets
// are useless.
var LatencyEdgesMicros = []int64{1_000, 2_000, 5_000, 10_000, 25_000, 50_000, 100_000, 250_000, 500_000, 1_000_000, 2_500_000}

// NumLatencyBuckets is one more than the edges: below the first, between each, and at/above the last.
var NumLatencyBuckets = len(LatencyEdgesMicros) + 1

var LatencyBucketLabels = []string{
	"<1ms", "1–2ms", "2–5ms", "5–10ms", "10–25ms", "25–50ms", "50–100ms",
	"100–250ms", "250–500ms", "0.5–1s", "1–2.5s", "≥2.5s",
}

// LatencyBucketIndex returns the bucket (0 .. NumLatencyBuckets-1) for a duration in micros.
// Non-finite input falls into the first bucket.
func LatencyBucketIndex(micros float64) int {
	if math.IsNaN(micros) || math.IsInf(micros, 0) {
		micros = 0
	}
	for i, e := range LatencyEdgesMicros {
		if micros < float64(e) {
			return i
		}
	}
	return len(LatencyEdgesMicros)
}
