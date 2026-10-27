package core

import (
	"encoding/json"
	"math"
)

// CoerceEvents validates/coerces a batch of already-normalized events (the shape adapters
// produce and the server accepts at POST /v1/events). input is decoded JSON: either an array of
// events or {"events": [...]}. Anything missing traceId/spanId is dropped; other fields get safe
// defaults so a sloppy adapter can't poison the window.
func CoerceEvents(input any) []Event {
	var arr []any
	switch v := input.(type) {
	case []any:
		arr = v
	case map[string]any:
		arr, _ = v["events"].([]any)
	}

	startCeil := StartTimeCeiling()
	out := make([]Event, 0, len(arr))
	for _, raw := range arr {
		r, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		traceID, ok1 := r["traceId"].(string)
		spanID, ok2 := r["spanId"].(string)
		if !ok1 || !ok2 {
			continue
		}

		kind := KindInternal
		if k, _ := r["kind"].(string); validKind(SpanKind(k)) {
			kind = SpanKind(k)
		}
		status := StatusUnset
		if s, _ := r["status"].(string); s == "ok" || s == "error" || s == "unset" {
			status = SpanStatus(s)
		}
		participant := "unknown"
		if p, _ := r["participant"].(string); p != "" {
			participant = p
		}
		parent, _ := r["parentSpanId"].(string)
		peer, _ := r["peer"].(string)
		operation, _ := r["operation"].(string)

		out = append(out, Event{
			TraceID:      traceID,
			SpanID:       spanID,
			ParentSpanID: parent,
			Participant:  participant,
			Peer:         peer,
			Operation:    operation,
			Kind:         kind,
			StartTime:    ClampMicros(toNumber(r["startTime"]), startCeil),
			Duration:     ClampMicros(toNumber(r["duration"]), MaxEpochMicros),
			Status:       status,
			Attributes:   scalarAttributes(r["attributes"]),
		})
	}
	return out
}

func validKind(k SpanKind) bool {
	switch k {
	case KindClient, KindServer, KindProducer, KindConsumer, KindInternal:
		return true
	}
	return false
}

// scalarAttributes keeps only string/number/bool values, which is what Attributes promises.
func scalarAttributes(v any) Attributes {
	out := Attributes{}
	m, ok := v.(map[string]any)
	if !ok {
		return out
	}
	for k, val := range m {
		switch x := val.(type) {
		case string, bool, float64:
			out[k] = x
		case json.Number:
			if f, err := x.Float64(); err == nil {
				out[k] = f
			}
		case int:
			out[k] = float64(x)
		case int64:
			out[k] = float64(x)
		}
	}
	return out
}

// toNumber mirrors JavaScript's Number(value) for the inputs ingest can see.
func toNumber(v any) float64 {
	switch x := v.(type) {
	case nil:
		return 0
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return math.NaN()
		}
		return f
	case bool:
		if x {
			return 1
		}
		return 0
	case string:
		return ParseJSNumber(x)
	}
	return math.NaN()
}
