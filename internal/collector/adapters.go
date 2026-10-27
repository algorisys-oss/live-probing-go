package collector

import (
	"fmt"
	"slices"
	"strings"

	"liveprobe/internal/core"
)

// Adapter maps one raw source payload into normalized events (the narrow waist).
type Adapter func(raw any) []core.Event

var adapters = map[string]Adapter{
	"adapter-id-1":         AdapterID1,
	"algo-instrumentation": AlgoInstrumentation,
	// Passthrough for sources that already emit normalized LiveProbe events.
	"liveprobe-native": func(raw any) []core.Event {
		if arr, ok := raw.([]any); ok {
			return core.CoerceEvents(arr)
		}
		return core.CoerceEvents([]any{raw})
	},
}

func GetAdapter(name string) (Adapter, error) {
	if a, ok := adapters[name]; ok {
		return a, nil
	}
	names := make([]string, 0, len(adapters))
	for n := range adapters {
		names = append(names, n)
	}
	slices.Sort(names)
	return nil, fmt.Errorf("unknown adapter %q (have: %s)", name, strings.Join(names, ", "))
}

// --- adapter-id-1 --------------------------------------------------------------------------

// Reference adapter for a structured instrumentation format that already carries distributed
// tracing ids (trace_id / span_id / parent_span_id). One client event maps to one span. Field
// mapping is documented in ../live-probing/docs/integration-adapters.md.

// looseStr mirrors the TS str(): strings as-is, null/undefined → "", anything else String(v).
func looseStr(m map[string]any, k string) string {
	v, ok := m[k]
	if !ok || v == nil {
		return ""
	}
	return jsString(v)
}

// id1Micros: a number is epoch ms; a string is ms or a date.
func id1Micros(v any) int64 {
	switch x := v.(type) {
	case float64:
		if finite(x) {
			return jsRound(x * 1000)
		}
	case string:
		if strings.TrimSpace(x) != "" {
			if n := jsNumber(x, true); finite(n) {
				return jsRound(n * 1000)
			}
			if ms, ok := dateParse(x); ok {
				return ms * 1000
			}
		}
	}
	return 0
}

func AdapterID1(raw any) []core.Event {
	r, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	traceID, spanID := looseStr(r, "trace_id"), looseStr(r, "span_id")
	if traceID == "" || spanID == "" {
		return nil // need span identity to place it in a trace
	}

	service, eventType := looseStr(r, "service"), looseStr(r, "event_type")
	participant := firstNonEmpty(looseStr(r, "service_category"), looseStr(r, "component"), service, "unknown")
	operation := firstNonEmpty(looseStr(r, "span_name"), service, eventType, "event")
	if service != "" && eventType != "" {
		operation = service + ":" + eventType
	}
	status := core.StatusUnset
	if level := strings.ToUpper(looseStr(r, "level")); level == "ERROR" || level == "CRITICAL" {
		status = core.StatusError
	}

	attrs := core.Attributes{}
	for _, k := range []string{"event_value", "correlation_id", "reference_id", "reference_type", "ou_id", "event_source", "level"} {
		if isScalar(r[k]) {
			attrs[k] = r[k]
		}
	}
	for k, v := range entries(r["details"]) {
		if isScalar(v) {
			attrs["details."+k] = v
		}
	}

	start := r["span_start_timestamp"]
	if start == nil { // ?? — missing or null falls through
		start = r["timestamp"]
	}
	return []core.Event{{
		TraceID: traceID, SpanID: spanID, ParentSpanID: looseStr(r, "parent_span_id"),
		Participant: participant, Operation: operation,
		Kind:      core.KindInternal, // this format has no span kind; topology still works via parent/child
		StartTime: id1Micros(start),
		Duration:  0, // event-centric format: no per-span duration (documented gap)
		Status:    status, Attributes: attrs,
	}}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// --- algo-instrumentation ------------------------------------------------------------------

// Adapter for the internal instrumentation-service format: instrumentation / log / audit events,
// each carrying application{name,module,...} and request{requestId[,traceId,spanId]}, optionally
// with spans[] children (top-level or under payload). Spec + mapping:
// ../live-probing/docs/adapters/algo-instrumentation.md.

func str(v any) string {
	s, _ := v.(string)
	return s
}

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

// isoMicros: timestamps are ISO-8601 strings; epoch micros out (0 when unparseable).
func isoMicros(v any) int64 {
	if s, ok := v.(string); ok {
		if ms, ok := dateParse(s); ok {
			return ms * 1000
		}
	}
	return 0
}

// num mirrors Number(m[k]): a missing key is NaN, null is 0.
func num(m map[string]any, k string) float64 {
	v, ok := m[k]
	return jsNumber(v, ok)
}

var errorLevels = map[string]bool{"ERROR": true, "FATAL": true, "CRITICAL": true}

type childKind struct {
	kind core.SpanKind
	peer string
}

// How a child span's kind maps to LiveProbe. Client kinds carry a peer so they draw as an arrow
// to a dedicated lifeline (and a topology edge); function/internal stay put.
var childKinds = map[string]childKind{
	"db":       {core.KindClient, "database"},
	"redis":    {core.KindClient, "redis"},
	"cache":    {core.KindClient, "cache"},
	"http":     {core.KindClient, ""},
	"function": {core.KindInternal, ""},
}

// childSpan maps one spans[] entry; ok=false if it lacks a spanId. A child with no parentSpanId
// attaches to the request span (parentFallback) rather than becoming a root.
func childSpan(raw any, traceID, participant, parentFallback string) (core.Event, bool) {
	s := obj(raw)
	spanID := str(s["spanId"])
	if spanID == "" {
		return core.Event{}, false
	}
	kindName := strings.ToLower(str(s["kind"]))
	mapped, ok := childKinds[kindName]
	if !ok {
		mapped = childKind{kind: core.KindInternal}
	}
	start := isoMicros(s["startTime"])
	durationMs := num(s, "durationMs")
	duration := max(0, isoMicros(s["endTime"])-start)
	if finite(durationMs) && durationMs > 0 {
		duration = jsRound(durationMs * 1000)
	}

	attrs := core.Attributes{}
	if kindName != "" {
		attrs["spanKind"] = kindName
	}
	if finite(durationMs) {
		attrs["durationMs"] = durationMs
	}
	status := core.StatusUnset
	if success, isBool := s["success"].(bool); isBool {
		attrs["success"] = success
		status = core.StatusError
		if success {
			status = core.StatusOK
		}
	}
	return core.Event{
		TraceID:      firstNonEmpty(str(s["traceId"]), traceID),
		SpanID:       spanID,
		ParentSpanID: firstNonEmpty(str(s["parentSpanId"]), parentFallback),
		Participant:  participant,
		Peer:         mapped.peer,
		Operation:    firstNonEmpty(str(s["name"]), kindName, "span"),
		Kind:         mapped.kind,
		StartTime:    start,
		Duration:     duration,
		Status:       status,
		Attributes:   attrs,
	}, true
}

func joinNonEmpty(sep string, parts ...string) string {
	return strings.Join(slices.DeleteFunc(parts, func(s string) bool { return s == "" }), sep)
}

func AlgoInstrumentation(raw any) []core.Event {
	r, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	// Batch envelope: emitters POST {events: [...]}; a single event never carries an events array.
	if batch, ok := r["events"].([]any); ok {
		var out []core.Event
		for _, e := range batch {
			out = append(out, AlgoInstrumentation(e)...)
		}
		return out
	}

	eventID := str(r["eventId"])
	if eventID == "" {
		return nil
	}
	application, payload, request := obj(r["application"]), obj(r["payload"]), obj(r["request"])
	requestID, reqSpanID, eventType := str(request["requestId"]), str(request["spanId"]), str(r["eventType"])

	// Prefer the real trace identity; fall back to the old requestId synthesis.
	traceID := firstNonEmpty(str(request["traceId"]), requestID, eventID)
	participant := firstNonEmpty(str(application["name"]), "unknown")
	if module := str(application["module"]); module != "" {
		participant += "." + module
	}
	startTime := isoMicros(r["timestamp"])

	attrs := core.Attributes{}
	add := func(k string, v any) {
		if isScalar(v) {
			attrs[k] = v
		}
	}
	add("application", application["name"])
	add("module", application["module"])
	add("service", application["service"])
	add("version", application["version"])
	add("environment", application["environment"])
	add("eventType", eventType)
	add("severity", r["severity"])
	add("message", r["message"])
	for k, v := range obj(r["tags"]) {
		add("tags."+k, v)
	}

	operation := firstNonEmpty(eventType, "event")
	kind := core.KindInternal
	status := core.StatusUnset
	var duration int64

	switch eventType {
	case "instrumentation":
		kind = core.KindServer // the app handled an inbound request (OTel: server side of the call)
		operation = firstNonEmpty(joinNonEmpty(" ", str(payload["method"]), str(payload["endpoint"])), operation)
		if d := num(payload, "durationMs"); finite(d) && d > 0 {
			duration = jsRound(d * 1000)
		}
		// Status code arrives as statusCode (spec) or status_code (real payloads).
		codeKey := "statusCode"
		if payload["statusCode"] == nil {
			codeKey = "status_code"
		}
		code := num(payload, codeKey)
		success, isBool := payload["success"].(bool)
		if (isBool && !success) || (finite(code) && code >= 400) {
			status = core.StatusError
		} else if (isBool && success) || (finite(code) && code > 0) {
			status = core.StatusOK
		}
		for _, k := range []string{"endpoint", "method", "statusCode", "status_code", "durationMs", "success", "memory_usage_mb"} {
			add(k, payload[k])
		}
	case "log":
		level := strings.ToUpper(str(payload["level"]))
		levelTag := ""
		if level != "" {
			levelTag = level + ":"
		}
		operation = joinNonEmpty(" ", "log", levelTag, str(payload["message"]))
		if errorLevels[level] {
			status = core.StatusError
		}
		for _, k := range []string{"level", "message", "file", "line"} {
			add(k, payload[k])
		}
	case "audit":
		operation = joinNonEmpty(" ", "audit", str(payload["action"]), str(payload["entityType"]), str(payload["entityId"]))
		for _, k := range []string{"entityType", "entityId", "action", "reason", "createdBy", "userId"} {
			add(k, payload[k])
		}
		if changed, ok := payload["changedFields"].([]any); ok {
			var names []string
			for _, c := range changed {
				names = append(names, str(c))
			}
			attrs["changedFields"] = joinNonEmpty(",", names...)
		}
		for _, side := range []string{"before", "after"} {
			for k, v := range obj(payload[side]) {
				add(side+"."+k, v)
			}
		}
	default:
		// Unknown event types still render: keep primitive payload fields as attributes.
		for k, v := range payload {
			add(k, v)
		}
	}

	// `timestamp` is the request's *completion* time; anchor the request/root at its start so the
	// entry arrow sorts before the (earlier-starting) children.
	requestStart := startTime
	if duration > 0 {
		requestStart = startTime - duration
	}

	span := core.Event{
		TraceID: traceID,
		// The real request.spanId lets spans[] children attach; older emitters fall back to eventId.
		SpanID:       firstNonEmpty(reqSpanID, eventID),
		ParentSpanID: requestID,
		Participant:  participant,
		Operation:    operation,
		Kind:         kind,
		StartTime:    requestStart,
		Duration:     duration,
		Status:       status,
		Attributes:   attrs,
	}

	var children []core.Event
	rawSpans, ok := r["spans"].([]any)
	if !ok {
		rawSpans, _ = payload["spans"].([]any)
	}
	if eventType == "instrumentation" {
		for _, s := range rawSpans {
			if child, ok := childSpan(s, traceID, participant, span.SpanID); ok {
				children = append(children, child)
			}
		}
	}

	if requestID == "" {
		return append([]core.Event{span}, children...)
	}
	root := core.Event{
		TraceID: traceID,
		SpanID:  requestID, // deterministic: every event of the request re-emits it; the window dedupes
		// Title the trace by the endpoint rather than the opaque requestId.
		Participant: "client", Operation: operation, Kind: core.KindClient,
		StartTime: requestStart, Status: core.StatusUnset, Attributes: core.Attributes{},
	}
	return append([]core.Event{root, span}, children...)
}
