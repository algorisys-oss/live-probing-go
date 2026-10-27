package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strings"
)

// Minimal shapes of OTLP/HTTP JSON. We parse defensively rather than pull in the full protobuf
// schema: the collector is configured to export OTLP as JSON. Fields that real exporters send
// as either a number or a string (enums, int64s, nanos) are typed `any`.

type OTLPAnyValue struct {
	StringValue *string `json:"stringValue,omitempty"`
	BoolValue   *bool   `json:"boolValue,omitempty"`
	IntValue    any     `json:"intValue,omitempty"`
	DoubleValue any     `json:"doubleValue,omitempty"`
}

type OTLPKeyValue struct {
	Key   string        `json:"key"`
	Value *OTLPAnyValue `json:"value,omitempty"`
}

type OTLPStatus struct {
	Code any `json:"code,omitempty"`
}

type OTLPSpan struct {
	TraceID           any            `json:"traceId,omitempty"`
	SpanID            any            `json:"spanId,omitempty"`
	ParentSpanID      any            `json:"parentSpanId,omitempty"`
	Name              any            `json:"name,omitempty"`
	Kind              any            `json:"kind,omitempty"`
	StartTimeUnixNano any            `json:"startTimeUnixNano,omitempty"`
	EndTimeUnixNano   any            `json:"endTimeUnixNano,omitempty"`
	Attributes        []OTLPKeyValue `json:"attributes,omitempty"`
	Status            *OTLPStatus    `json:"status,omitempty"`
}

type OTLPScopeSpans struct {
	Spans []OTLPSpan `json:"spans,omitempty"`
}

type OTLPResource struct {
	Attributes []OTLPKeyValue `json:"attributes,omitempty"`
}

type OTLPResourceSpans struct {
	Resource                    *OTLPResource    `json:"resource,omitempty"`
	ScopeSpans                  []OTLPScopeSpans `json:"scopeSpans,omitempty"`
	InstrumentationLibrarySpans []OTLPScopeSpans `json:"instrumentationLibrarySpans,omitempty"` // older OTLP field name
}

type OTLPPayload struct {
	ResourceSpans []OTLPResourceSpans `json:"resourceSpans,omitempty"`
}

// DecodeOTLP parses an OTLP/HTTP JSON body. Numbers are kept as json.Number so nanosecond
// timestamps never lose precision through float64.
func DecodeOTLP(body []byte) (OTLPPayload, error) {
	var p OTLPPayload
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	err := dec.Decode(&p)
	return p, err
}

func attrValue(v *OTLPAnyValue) (any, bool) {
	switch {
	case v == nil:
		return nil, false
	case v.StringValue != nil:
		return *v.StringValue, true
	case v.BoolValue != nil:
		return *v.BoolValue, true
	case v.IntValue != nil:
		return toNumber(v.IntValue), true
	case v.DoubleValue != nil:
		return toNumber(v.DoubleValue), true
	}
	return nil, false
}

func toAttrs(attributes []OTLPKeyValue) Attributes {
	out := Attributes{}
	for _, a := range attributes {
		if val, ok := attrValue(a.Value); ok {
			out[a.Key] = val
		}
	}
	return out
}

func enumKey(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func mapKind(kind any) SpanKind {
	switch enumKey(kind) {
	case "2", "SPAN_KIND_SERVER":
		return KindServer
	case "3", "SPAN_KIND_CLIENT":
		return KindClient
	case "4", "SPAN_KIND_PRODUCER":
		return KindProducer
	case "5", "SPAN_KIND_CONSUMER":
		return KindConsumer
	}
	return KindInternal
}

func mapStatus(code any) SpanStatus {
	switch enumKey(code) {
	case "2", "STATUS_CODE_ERROR":
		return StatusError
	case "1", "STATUS_CODE_OK":
		return StatusOK
	}
	return StatusUnset
}

// toMicros converts a nanosecond unix timestamp (string or number) to microseconds using
// integer division, since nanos overflow float64 precision. Unparseable input becomes 0.
func toMicros(nano any) float64 {
	var s string
	switch x := nano.(type) {
	case nil:
		return 0
	case float64:
		if x != math.Trunc(x) || math.IsInf(x, 0) {
			return 0
		}
		s = big.NewFloat(x).Text('f', 0)
	default:
		s = strings.TrimSpace(fmt.Sprint(x))
	}
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return 0
	}
	f, _ := new(big.Float).SetInt(n.Quo(n, big.NewInt(1000))).Float64()
	return f
}

// truthyString mirrors JS `x ? String(x) : undefined` for the id-ish fields.
func truthyString(v any) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "", false
	case string:
		return x, x != ""
	case bool:
		return "true", x
	case float64:
		return jsNumberString(x), x != 0 && !math.IsNaN(x)
	case json.Number:
		f, _ := x.Float64()
		return x.String(), f != 0
	}
	s := fmt.Sprint(v)
	return s, s != ""
}

func attrString(attrs Attributes, key string) (string, bool) {
	s, ok := attrs[key].(string)
	return s, ok
}

// httpName rebuilds "METHOD /path" for HTTP spans named by bare method ("GET"). Prefers
// http.route (low cardinality) unless it is a catch-all "*" (Remix/SPA), in which case the
// concrete request path (url.path/http.target) is the useful title.
func httpName(attrs Attributes) (string, bool) {
	method, ok := attrString(attrs, "http.request.method")
	if _, present := attrs["http.request.method"]; !present {
		method, ok = attrString(attrs, "http.method")
	}
	if !ok {
		return "", false
	}
	var path any
	if route, isStr := attrs["http.route"].(string); isStr && route != "*" {
		path = route
	} else if v, present := attrs["url.path"]; present {
		path = v
	} else {
		path = attrs["http.target"]
	}
	p, ok := path.(string)
	if !ok {
		return "", false
	}
	return method + " " + strings.SplitN(p, "?", 2)[0], true
}

// resolvePeer finds the other side of a call: a datastore/broker if the span carries one
// (db.system, or db.system.name from newer semconv),
// otherwise the remote service/host. Only meaningful for client/producer/consumer spans.
func resolvePeer(kind SpanKind, attrs Attributes) string {
	if kind == KindServer || kind == KindInternal {
		return ""
	}
	// db.system.name is the OTel semconv >= 1.33 name for db.system (e.g. otelpgx emits it).
	for _, key := range []string{"db.system", "db.system.name", "messaging.system", "peer.service", "server.address", "net.peer.name", "network.peer.address", "messaging.destination.name"} {
		if v, ok := attrString(attrs, key); ok {
			return v
		}
	}
	return ""
}

// NormalizeOTLP flattens an OTLP/HTTP JSON payload into normalized events.
func NormalizeOTLP(payload OTLPPayload) []Event {
	startCeil := StartTimeCeiling()
	events := []Event{}
	for _, rs := range payload.ResourceSpans {
		var resourceAttrs Attributes
		if rs.Resource != nil {
			resourceAttrs = toAttrs(rs.Resource.Attributes)
		}
		participant := "unknown"
		if v, ok := resourceAttrs["service.name"]; ok {
			participant = attrToString(v)
		}
		scopes := rs.ScopeSpans
		if scopes == nil {
			scopes = rs.InstrumentationLibrarySpans
		}
		for _, scope := range scopes {
			for _, span := range scope.Spans {
				traceID, ok1 := truthyString(span.TraceID)
				spanID, ok2 := truthyString(span.SpanID)
				if !ok1 || !ok2 {
					continue
				}
				parent, _ := truthyString(span.ParentSpanID)
				attrs := toAttrs(span.Attributes)
				kind := mapKind(span.Kind)
				start := ClampMicros(toMicros(span.StartTimeUnixNano), startCeil)
				end := ClampMicros(toMicros(span.EndTimeUnixNano), startCeil)
				operation, ok := httpName(attrs)
				if !ok {
					operation = ""
					if span.Name != nil {
						operation = fmt.Sprint(span.Name)
					}
				}
				var code any
				if span.Status != nil {
					code = span.Status.Code
				}
				events = append(events, Event{
					TraceID:      traceID,
					SpanID:       spanID,
					ParentSpanID: parent,
					Participant:  participant,
					Peer:         resolvePeer(kind, attrs),
					Operation:    operation,
					Kind:         kind,
					StartTime:    start,
					Duration:     max(0, end-start),
					Status:       mapStatus(code),
					Attributes:   attrs,
				})
			}
		}
	}
	return events
}

// attrToString mirrors String(value) for a scalar attribute value.
func attrToString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return jsNumberString(x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	}
	return fmt.Sprint(v)
}
