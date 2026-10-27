package collector

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"liveprobe/internal/core"
)

// Ports of adapters/adapter-id-1.test.ts and adapters/algo-instrumentation.test.ts. Raw inputs are
// JSON text, decoded exactly as the sources decode them.

func raw(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("bad fixture: %v", err)
	}
	return v
}

func eq[T comparable](t *testing.T, got, want T, what string) {
	t.Helper()
	if got != want {
		t.Errorf("%s: got %v, want %v", what, got, want)
	}
}

func TestAdapterID1MapsStructuredEvent(t *testing.T) {
	events := AdapterID1(raw(t, `{"trace_id":"t1","span_id":"s2","parent_span_id":"s1","service_category":"orders-api",
	  "component":"OrdersModule","service":"OrderState","event_type":"Created","event_value":"42","correlation_id":"ORD-123",
	  "reference_id":"R1","reference_type":"OrderID","level":"ERROR","ou_id":"OU9","timestamp":1700000000000,
	  "details":{"exception_name":"IOException","retries":3}}`))
	eq(t, len(events), 1, "count")
	e := events[0]
	eq(t, e.TraceID, "t1", "traceId")
	eq(t, e.SpanID, "s2", "spanId")
	eq(t, e.ParentSpanID, "s1", "parentSpanId")
	eq(t, e.Participant, "orders-api", "participant")
	eq(t, e.Operation, "OrderState:Created", "operation")
	eq(t, e.Status, core.StatusError, "status")
	eq(t, e.StartTime, 1_700_000_000_000*1000, "ms -> micros")
	eq(t, e.Kind, core.KindInternal, "kind")
	eq(t, e.Attributes["correlation_id"], any("ORD-123"), "correlation_id")
	eq(t, e.Attributes["details.exception_name"], any("IOException"), "details.exception_name")
	eq(t, e.Attributes["details.retries"], any(float64(3)), "details.retries")
}

func TestAdapterID1DropsWithoutIdentity(t *testing.T) {
	eq(t, len(AdapterID1(raw(t, `{"trace_id":"t"}`))), 0, "no span id")
	eq(t, len(AdapterID1(nil)), 0, "null")
}

func TestAdapterID1Timestamps(t *testing.T) {
	// span_start_timestamp wins; a numeric string is ms; an ISO string is parsed.
	eq(t, AdapterID1(raw(t, `{"trace_id":"t","span_id":"s","span_start_timestamp":"1500","timestamp":9}`))[0].StartTime, 1_500_000, "numeric string")
	eq(t, AdapterID1(raw(t, `{"trace_id":"t","span_id":"s","span_start_timestamp":null,"timestamp":2}`))[0].StartTime, 2000, "null falls back")
	eq(t, AdapterID1(raw(t, `{"trace_id":"t","span_id":"s","timestamp":"2026-05-28T10:15:00Z"}`))[0].StartTime, tsMicros, "iso")
	eq(t, AdapterID1(raw(t, `{"trace_id":"t","span_id":"s","timestamp":"garbage"}`))[0].StartTime, 0, "unparseable")
}

const ts = "2026-05-28T10:15:00Z"

var tsMicros = func() int64 { v, _ := time.Parse(time.RFC3339, ts); return v.UnixMilli() * 1000 }()

func TestAlgoInstrumentationRootAndServerSpan(t *testing.T) {
	events := AlgoInstrumentation(raw(t, `{"eventId":"550e8400-e29b-41d4-a716-446655440000","eventType":"instrumentation","timestamp":"`+ts+`",
	  "application":{"name":"propeak","module":"invoice","environment":"prod"},"request":{"requestId":"req-123"},
	  "payload":{"endpoint":"/api/payroll/process","method":"POST","statusCode":200,"durationMs":135,"success":true}}`))
	eq(t, len(events), 2, "count")
	root, span := events[0], events[1]
	eq(t, root.TraceID, "req-123", "root.traceId")
	eq(t, root.SpanID, "req-123", "root.spanId")
	eq(t, root.ParentSpanID, "", "root has no parent")
	eq(t, root.Participant, "client", "root.participant")
	eq(t, root.Kind, core.KindClient, "root.kind")
	eq(t, root.Operation, "POST /api/payroll/process", "root titled by endpoint")

	eq(t, span.TraceID, "req-123", "span.traceId")
	eq(t, span.SpanID, "550e8400-e29b-41d4-a716-446655440000", "span.spanId")
	eq(t, span.ParentSpanID, "req-123", "span.parent")
	eq(t, span.Participant, "propeak.invoice", "participant")
	eq(t, span.Operation, "POST /api/payroll/process", "operation")
	eq(t, span.Kind, core.KindServer, "kind")
	eq(t, span.StartTime, tsMicros-135_000, "anchored at start")
	eq(t, span.Duration, 135_000, "duration")
	eq(t, span.Status, core.StatusOK, "status")
	eq(t, span.Attributes["application"], any("propeak"), "application")
	eq(t, span.Attributes["module"], any("invoice"), "module")
	eq(t, span.Attributes["environment"], any("prod"), "environment")
	eq(t, span.Attributes["statusCode"], any(float64(200)), "statusCode")
}

const algoBase = `"eventId":"e1","eventType":"instrumentation","timestamp":"` + ts + `","application":{"name":"propeak","module":"invoice"},"request":{"requestId":"req-1"}`

func TestAlgoInstrumentationFailures(t *testing.T) {
	eq(t, AlgoInstrumentation(raw(t, `{`+algoBase+`,"payload":{"endpoint":"/x","method":"GET","statusCode":500,"success":false}}`))[1].Status, core.StatusError, "failed")
	eq(t, AlgoInstrumentation(raw(t, `{`+algoBase+`,"payload":{"endpoint":"/x","method":"GET","statusCode":404}}`))[1].Status, core.StatusError, "404")
}

func TestAlgoInstrumentationLogEvent(t *testing.T) {
	events := AlgoInstrumentation(raw(t, `{"eventId":"log-1","eventType":"log","timestamp":"`+ts+`","application":{"name":"hrms","module":"employee","environment":"prod"},
	  "request":{"requestId":"req-456"},"payload":{"level":"ERROR","message":"Failed to process payroll","file":"payroll_service.go","line":225}}`))
	eq(t, len(events), 2, "count")
	s := events[1]
	eq(t, s.Participant, "hrms.employee", "participant")
	eq(t, s.ParentSpanID, "req-456", "parent")
	eq(t, s.Operation, "log ERROR: Failed to process payroll", "operation")
	eq(t, s.Kind, core.KindInternal, "kind")
	eq(t, s.Status, core.StatusError, "status")
	eq(t, s.Attributes["file"], any("payroll_service.go"), "file")
	eq(t, s.Attributes["line"], any(float64(225)), "line")
}

func TestAlgoInstrumentationAuditEvent(t *testing.T) {
	s := AlgoInstrumentation(raw(t, `{"eventId":"aud-1","eventType":"audit","timestamp":"`+ts+`","application":{"name":"hrms","module":"employee","environment":"prod"},
	  "request":{"requestId":"req-789"},"payload":{"entityType":"employee","entityId":"EMP100","action":"UPDATE","before":{"salary":50000},
	  "after":{"salary":70000},"changedFields":["salary"],"reason":"Annual appraisal","createdBy":"admin","userId":"U100","timestamp":"`+ts+`"}}`))[1]
	eq(t, s.Operation, "audit UPDATE employee EMP100", "operation")
	eq(t, s.Kind, core.KindInternal, "kind")
	eq(t, s.Status, core.StatusUnset, "status")
	eq(t, s.Attributes["changedFields"], any("salary"), "changedFields")
	eq(t, s.Attributes["before.salary"], any(float64(50000)), "before")
	eq(t, s.Attributes["after.salary"], any(float64(70000)), "after")
	eq(t, s.Attributes["createdBy"], any("admin"), "createdBy")
	eq(t, s.Attributes["reason"], any("Annual appraisal"), "reason")
}

func TestAlgoInstrumentationWithoutRequestID(t *testing.T) {
	events := AlgoInstrumentation(raw(t, `{"eventId":"solo-1","eventType":"log","timestamp":"`+ts+`","application":{"name":"hrms","module":"employee"},"payload":{"level":"INFO","message":"started"}}`))
	eq(t, len(events), 1, "count")
	eq(t, events[0].TraceID, "solo-1", "traceId")
	eq(t, events[0].ParentSpanID, "", "parent")
}

func TestAlgoInstrumentationDropsWithoutEventID(t *testing.T) {
	eq(t, len(AlgoInstrumentation(raw(t, `{"eventType":"log"}`))), 0, "no eventId")
	eq(t, len(AlgoInstrumentation(nil)), 0, "null")
	eq(t, len(AlgoInstrumentation("nope")), 0, "string")
}

func TestAlgoInstrumentationBatchEnvelope(t *testing.T) {
	events := AlgoInstrumentation(raw(t, `{"events":[
	  {"eventId":"b1","eventType":"instrumentation","timestamp":"`+ts+`","application":{"name":"hrms","module":"roles"},"request":{"requestId":"rq-1"},
	   "payload":{"endpoint":"/roles","method":"GET","status_code":200,"durationMs":58,"success":true}},
	  {"eventId":"b2","eventType":"instrumentation","timestamp":"`+ts+`","application":{"name":"hrms","module":"company"},"request":{"requestId":"rq-2"},
	   "payload":{"endpoint":"/company","method":"GET","status_code":200,"durationMs":129,"success":true}}]}`))
	eq(t, len(events), 4, "two roots + two spans")
	var parts []string
	for _, e := range events {
		parts = append(parts, e.Participant)
	}
	slices.Sort(parts)
	if !slices.Equal(parts, []string{"client", "client", "hrms.company", "hrms.roles"}) {
		t.Errorf("participants: %v", parts)
	}
}

func TestAlgoInstrumentationSnakeCaseStatus(t *testing.T) {
	ok := AlgoInstrumentation(raw(t, `{`+algoBase+`,"payload":{"endpoint":"/x","method":"GET","status_code":200,"success":true}}`))[1]
	eq(t, ok.Status, core.StatusOK, "ok")
	eq(t, ok.Attributes["status_code"], any(float64(200)), "status_code attr")
	// A 5xx with no explicit success flag must still be an error.
	eq(t, AlgoInstrumentation(raw(t, `{`+algoBase+`,"payload":{"endpoint":"/x","method":"GET","status_code":500}}`))[1].Status, core.StatusError, "5xx")
}

func TestAlgoInstrumentationCapturesMetadata(t *testing.T) {
	s := AlgoInstrumentation(raw(t, `{"eventId":"x1","eventType":"instrumentation","timestamp":"`+ts+`",
	  "application":{"name":"hrms","module":"roles","service":"frontend","environment":"dev","version":"0.0.0"},
	  "request":{"requestId":"rq-1"},"severity":"INFO","message":"Web access","tags":{"route_type":"loader"},
	  "payload":{"endpoint":"/roles","method":"GET","status_code":200,"durationMs":58,"success":true,"memory_usage_mb":407.52}}`))[1]
	eq(t, s.Attributes["service"], any("frontend"), "service")
	eq(t, s.Attributes["version"], any("0.0.0"), "version")
	eq(t, s.Attributes["severity"], any("INFO"), "severity")
	eq(t, s.Attributes["message"], any("Web access"), "message")
	eq(t, s.Attributes["tags.route_type"], any("loader"), "tags")
	eq(t, s.Attributes["memory_usage_mb"], any(407.52), "memory")
}

func TestAlgoInstrumentationBackdatesRequestStart(t *testing.T) {
	events := AlgoInstrumentation(raw(t, `{"eventId":"e1","eventType":"instrumentation","timestamp":"`+ts+`","application":{"name":"hrms","module":"register-employee"},
	  "request":{"requestId":"rq-1","traceId":"trace-1","spanId":"req-span-1"},
	  "payload":{"endpoint":"/register-employee","method":"GET","status_code":200,"durationMs":56,"success":true}}`))
	eq(t, events[1].StartTime, tsMicros-56_000, "span start")
	eq(t, events[0].StartTime, tsMicros-56_000, "root start")
	eq(t, events[1].Duration, 56_000, "duration unchanged")
}

func TestAlgoInstrumentationNoDurationKeepsStart(t *testing.T) {
	s := AlgoInstrumentation(raw(t, `{"eventId":"e2","eventType":"log","timestamp":"`+ts+`","application":{"name":"hrms","module":"employee"},
	  "request":{"requestId":"rq-2"},"payload":{"level":"INFO","message":"hi"}}`))[1]
	eq(t, s.StartTime, tsMicros, "start")
}

func TestAlgoInstrumentationRealIdentityAndChildren(t *testing.T) {
	events := AlgoInstrumentation(raw(t, `{"eventId":"evt-1","eventType":"instrumentation","timestamp":"`+ts+`","application":{"name":"hrms","module":"dashboard"},
	  "request":{"requestId":"rq-1","traceId":"trace-abc","spanId":"req-span-1"},
	  "payload":{"endpoint":"/dashboard","method":"GET","status_code":200,"durationMs":129,"success":true},
	  "spans":[
	   {"spanId":"s-auth","parentSpanId":"req-span-1","traceId":"trace-abc","kind":"function","name":"authenticate","startTime":"`+ts+`","endTime":"2026-05-28T10:15:00.002Z","durationMs":1.93,"success":true},
	   {"spanId":"s-perm","parentSpanId":"req-span-1","traceId":"trace-abc","kind":"db","name":"fetchPermissions","startTime":"`+ts+`","endTime":"2026-05-28T10:15:00.005Z","durationMs":5.39,"success":true}]}`))
	eq(t, len(events), 4, "root + request span + 2 children")
	root, req, auth, perm := events[0], events[1], events[2], events[3]
	eq(t, req.TraceID, "trace-abc", "real trace id")
	eq(t, req.SpanID, "req-span-1", "real span id")
	eq(t, req.ParentSpanID, "rq-1", "under the client root")
	eq(t, root.SpanID, "rq-1", "root span")

	eq(t, auth.SpanID, "s-auth", "auth id")
	eq(t, auth.ParentSpanID, "req-span-1", "auth parent")
	eq(t, auth.Participant, "hrms.dashboard", "auth participant")
	eq(t, auth.Operation, "authenticate", "auth op")
	eq(t, auth.Kind, core.KindInternal, "auth kind")
	eq(t, auth.Peer, "", "auth peer")
	eq(t, auth.Duration, 1930, "1.93ms -> micros")

	eq(t, perm.Operation, "fetchPermissions", "perm op")
	eq(t, perm.Kind, core.KindClient, "perm kind")
	eq(t, perm.Peer, "database", "perm peer")
	eq(t, perm.Status, core.StatusOK, "perm status")
	eq(t, perm.Attributes["spanKind"], any("db"), "spanKind")
}

func TestAlgoInstrumentationNestedSpans(t *testing.T) {
	nested := AlgoInstrumentation(raw(t, `{"eventId":"evt-1","eventType":"instrumentation","timestamp":"`+ts+`","application":{"name":"hrms","module":"dashboard"},
	  "request":{"requestId":"rq-1","traceId":"trace-abc","spanId":"req-span-1"},
	  "payload":{"endpoint":"/dashboard","method":"GET","status_code":200,"success":true,
	   "spans":[{"spanId":"s-perm","parentSpanId":"req-span-1","kind":"db","name":"fetchPermissions","startTime":"`+ts+`","durationMs":5.39,"success":true}]}}`))
	eq(t, len(nested), 3, "child under payload not dropped")
	eq(t, nested[2].SpanID, "s-perm", "child id")
	eq(t, nested[2].Peer, "database", "child peer")
}

func TestAlgoInstrumentationOrphanChildAttachesToRequest(t *testing.T) {
	events := AlgoInstrumentation(raw(t, `{"eventId":"evt-2","eventType":"instrumentation","timestamp":"`+ts+`","application":{"name":"hrms","module":"dashboard"},
	  "request":{"requestId":"rq-1","traceId":"trace-abc","spanId":"req-span-1"},
	  "payload":{"endpoint":"/dashboard","method":"GET","status_code":200,"success":true},
	  "spans":[{"spanId":"s-loose","kind":"function","name":"loadWidgets","startTime":"`+ts+`","durationMs":2}]}`))
	i := slices.IndexFunc(events, func(e core.Event) bool { return e.SpanID == "s-loose" })
	eq(t, events[i].ParentSpanID, "req-span-1", "never orphaned to a root")
}

func TestAlgoInstrumentationBackCompat(t *testing.T) {
	events := AlgoInstrumentation(raw(t, `{"eventId":"old-1","eventType":"instrumentation","timestamp":"`+ts+`","application":{"name":"hrms","module":"roles"},
	  "request":{"requestId":"rq-9"},"payload":{"endpoint":"/roles","method":"GET","status_code":200,"success":true}}`))
	eq(t, len(events), 2, "root + span")
	eq(t, events[1].TraceID, "rq-9", "falls back to requestId")
	eq(t, events[1].SpanID, "old-1", "falls back to eventId")
}

func TestGetAdapter(t *testing.T) {
	for _, name := range []string{"adapter-id-1", "algo-instrumentation", "liveprobe-native"} {
		if _, err := GetAdapter(name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := GetAdapter("nope"); err == nil {
		t.Error("unknown adapter should error")
	}
	native, _ := GetAdapter("liveprobe-native")
	eq(t, len(native(raw(t, `{"traceId":"t","spanId":"s"}`))), 1, "native single object")
}
