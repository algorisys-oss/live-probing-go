// Package core is the normalized event model and every pure projection of it: the rolling
// trace window, sequence/flow projections, metrics, alerts, masking and exporters. Every
// diagram LiveProbe draws is a projection of a stream of Events, so this is the single
// source of truth.
package core

import (
	"math"
	"strconv"
	"time"
)

type SpanKind string

const (
	KindClient   SpanKind = "client"
	KindServer   SpanKind = "server"
	KindProducer SpanKind = "producer"
	KindConsumer SpanKind = "consumer"
	KindInternal SpanKind = "internal"
)

type SpanStatus string

const (
	StatusOK    SpanStatus = "ok"
	StatusError SpanStatus = "error"
	StatusUnset SpanStatus = "unset"
)

// Attributes hold scalar values only: string, float64 or bool.
type Attributes map[string]any

// Event is the normalized interaction event. One event == one span.
type Event struct {
	TraceID      string     `json:"traceId"`
	SpanID       string     `json:"spanId"`
	ParentSpanID string     `json:"parentSpanId,omitempty"` // empty for a trace root
	Participant  string     `json:"participant"`            // logical name of the side doing this span's work (the service)
	Peer         string     `json:"peer,omitempty"`         // the other side for client/producer/consumer spans (service or datastore)
	Operation    string     `json:"operation"`              // span name / method / route
	Kind         SpanKind   `json:"kind"`
	StartTime    int64      `json:"startTime"` // epoch microseconds
	Duration     int64      `json:"duration"`  // microseconds
	Status       SpanStatus `json:"status"`
	Attributes   Attributes `json:"attributes"`
}

// MaxEpochMicros is the absolute upper bound for an epoch-microsecond timestamp (~year 2243).
// Rejects absurd inputs (e.g. 1e30) that would otherwise poison time-based logic downstream.
const MaxEpochMicros int64 = 8_640_000_000_000_000

// FutureSkewMicros is the allowed clock skew for an ingested start time: timestamps up to this
// far past "now" are accepted as-is; further-future values are clamped down. Left unclamped, a
// far-future start time becomes the live window's clock reference and evicts every
// genuinely-recent trace (a live-view denial of service).
const FutureSkewMicros int64 = 60_000_000 // 60 seconds

// ClampMicros coerces an untrusted timestamp/duration into a finite, non-negative, in-range
// number of microseconds. Non-finite or non-positive becomes 0; anything above max (capped at
// MaxEpochMicros) is clamped down. Fractional micros are truncated.
func ClampMicros(n float64, max int64) int64 {
	if math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 {
		return 0
	}
	ceil := max
	if ceil <= 0 || ceil > MaxEpochMicros {
		ceil = MaxEpochMicros
	}
	if n > float64(ceil) {
		return ceil
	}
	return int64(n)
}

// StartTimeCeiling is the current ceiling for an acceptable start time (now + allowed skew).
func StartTimeCeiling() int64 {
	return time.Now().UnixMilli()*1000 + FutureSkewMicros
}

// DatastoreSystems are peers that are infrastructure rather than services. Used to decide when
// an edge or a message points at a datastore node vs another service.
var DatastoreSystems = map[string]bool{
	"postgresql":    true,
	"mysql":         true,
	"redis":         true,
	"rabbitmq":      true,
	"amqp":          true,
	"kafka":         true,
	"mongodb":       true,
	"memcached":     true,
	"elasticsearch": true,
}

// jsRound reproduces JavaScript's Math.round: ties round toward +Infinity.
func jsRound(x float64) float64 {
	return math.Floor(x + 0.5)
}

// toFixed reproduces Number.prototype.toFixed for display strings (ties round up).
func toFixed(x float64, digits int) string {
	p := math.Pow10(digits)
	return strconv.FormatFloat(math.Floor(x*p+0.5)/p, 'f', digits, 64)
}

// jsNumberString formats a float the way String(n) does.
func jsNumberString(x float64) string { return FormatJSNumber(x) }
