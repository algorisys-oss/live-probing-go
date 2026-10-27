# algo-instrumentation — internal instrumentation-service format

Client spec. Adapter: `AlgoInstrumentation` in [`internal/collector/adapters.go`](../../internal/collector/adapters.go)
(a port of the TS adapter, identical output under a 20k-input fuzz).
Generic mapping doc: [integration-adapters.md](../integration-adapters.md#algo-instrumentation-field-mapping-instrumentation-service-format--event).

> Formerly `adapter-id-2` (renamed 2026-07-06). Registry key / `ADAPTER=` value is now
> `algo-instrumentation`.

## Transport

Apps POST JSON events to the internal instrumentation service:

- `POST /v1/event/instrumentation`
- `POST /v1/event/log`
- `POST /v1/event/audit`

The collector's `http` source is path-agnostic and accepts the same POSTs, so the emitters
(or a forwarder in the instrumentation service) can be pointed at it unchanged:

```bash
SOURCE=http ADAPTER=algo-instrumentation HTTP_PORT=4320 \
  LIVEPROBE_URL=http://localhost:4319 ./liveprobe-collector
```

`./serve.sh --collector` (or `docker compose --profile collector up -d`) runs exactly this next
to the server.

The `eventType` field in the payload decides the mapping — the URL path is ignored.

### Batch envelope

Emitters may POST a **batch** wrapping many events (as the HRMS web app does):

```json
{ "events": [ { …event… }, { …event… } ] }
```

The adapter unwraps `{ events: [...] }` and maps each element independently (a single event
never carries an `events` array, so this is unambiguous). A bare single event and a bare
top-level JSON array are also accepted.

## Event schemas (as provided; instrumentation refreshed from live HRMS 2026-07-06)

Fields beyond the originals below are tolerated and, where useful, captured as attributes:
`application.service`, `application.version`, top-level `severity` / `message`, and `tags.*`.
The instrumentation status code arrives as **`status_code`** from HRMS (older samples used
`statusCode`); the adapter reads either.

### Instrumentation

```json
{
  "eventId": "019f375b-5b20-7521-9e04-5f8a4d9490ff",
  "eventType": "instrumentation",
  "timestamp": "2026-07-06T12:16:01.824Z",
  "application": {
    "name": "HRMS WEB APPLICATION (LOCAL)",
    "module": "roles",
    "service": "frontend",
    "environment": "dev",
    "version": "0.0.0"
  },
  "request": { "requestId": "22e22789-f696-4a64-99eb-3423272e6afd" },
  "severity": "INFO",
  "message": "Web access",
  "tags": { "route_type": "loader" },
  "payload": {
    "endpoint": "/roles",
    "method": "GET",
    "status_code": 200,
    "durationMs": 58,
    "success": true,
    "memory_usage_mb": 407.52
  }
}
```

Older sample (still accepted — `statusCode`, single event, no envelope):

```json
{
  "eventId": "550e8400-e29b-41d4-a716-446655440000",
  "eventType": "instrumentation",
  "timestamp": "2026-05-28T10:15:00Z",
  "application": { "name": "propeak", "module": "invoice", "environment": "prod" },
  "request": { "requestId": "req-123" },
  "payload": {
    "endpoint": "/api/payroll/process",
    "method": "POST",
    "statusCode": 200,
    "durationMs": 135,
    "success": true
  }
}
```

### Log

```json
{
  "eventId": "...",
  "eventType": "log",
  "timestamp": "2026-05-28T10:15:00Z",
  "application": { "name": "hrms", "module": "employee", "environment": "prod" },
  "request": { "requestId": "req-456" },
  "payload": {
    "level": "ERROR",
    "message": "Failed to process payroll",
    "file": "payroll_service.go",
    "line": 225
  }
}
```

### Audit

```json
{
  "eventId": "...",
  "eventType": "audit",
  "timestamp": "2026-05-28T10:15:00Z",
  "application": { "name": "hrms", "module": "employee", "environment": "prod" },
  "request": { "requestId": "req-789" },
  "payload": {
    "entityType": "employee",
    "entityId": "EMP100",
    "action": "UPDATE",
    "before": { "salary": 50000 },
    "after": { "salary": 70000 },
    "changedFields": ["salary"],
    "reason": "Annual appraisal",
    "createdBy": "admin",
    "userId": "U100",
    "timestamp": "2026-05-28T10:15:00Z"
  }
}
```

## Real trace structure (2026-07-06 — HRMS now emits it)

HRMS added genuine identity and a child-span array; the adapter prefers them and falls back
to the synthesis below when they're absent (older emitters).

- `request.traceId` / `request.spanId` — real IDs. `traceId = request.traceId ?? requestId ??
  eventId`; the event's own span uses `spanId = request.spanId` so children can attach to it.
- `spans[]` — real child operations of the request. Accepted **top-level (`spans`) or nested
  under `payload` (`payload.spans`)** — emitters send it either way and both are unpacked. Each
  entry:
  ```json
  { "spanId","parentSpanId","traceId","kind","name","startTime","endTime","durationMs","success" }
  ```
  A child with no `parentSpanId` attaches to the request span (the handler), never orphaned to a
  root. Emitters today parent every child to `request.spanId` (a one-level fan-out); the adapter
  honours whatever `parentSpanId` a child carries, so real nested trees render as soon as the
  emitter provides them. Mapping:
  | span `kind` | LiveProbe `kind` | `peer` | effect |
  |---|---|---|---|
  | `db` | `client` | `database` | draws a **database** lifeline + topology edge per query |
  | `redis` / `cache` | `client` | `redis` / `cache` | dedicated peer node |
  | `http` | `client` | — | outbound call |
  | `function` | `internal` | — | in-process, nests in the waterfall |
  `name` → operation, `durationMs` (or `endTime − startTime`) → micros, `success` → status.
  IDs are UUIDs (not OTel hex), so these traces don't merge with the OTel `:4318` path.

## Mapping design — synthesis fallback (when traceId/spans are absent)

The original format had **no span ids and no parent ids**, only `request.requestId`. To make
sequence diagrams render, the adapter synthesizes structure:

- `traceId = request.requestId` — instrumentation + log + audit events of one request
  assemble into one trace. Fallback `eventId` when there is no requestId.
- **Synthetic `client` root span**, `spanId = requestId` (deterministic). Every event
  re-emits it; the trace window dedupes by spanId, so arrival order and missing siblings
  don't matter. All arrows originate from this lifeline. (Still emitted even with real IDs:
  it anchors the browser→server entry arrow, with the request span as its child.)
- Each event → one child span, `spanId = request.spanId ?? eventId`, `participant =
  application.name + "." + application.module` (dots, not slashes — a `/` would break the
  `/service/:name` UI route). **Application and module are the sequence-diagram lifelines**.
- `environment` is a span attribute → searchable (`attr=environment=prod`). So are
  `service`, `version`, `severity`, `message`, and any `tags.*`, plus per-type payload
  fields (instrumentation adds `status_code`/`statusCode` and `memory_usage_mb`).
- Instrumentation status: `error` when `success === false` **or** the status code
  (`status_code` or `statusCode`) is `>= 400` — so a 5xx with no explicit `success` flag
  still renders as an error.

Verified end to end 2026-07-03 against the TS implementation (isolated server :4391 + http collector :4392;
the Go adapter matches the TS one under the parity fuzz): sequence
`client → propeak.invoice: POST /api/payroll/process (135ms)`, error log drawn red on
`hrms.employee`, audit as `audit UPDATE employee EMP100`, topology `client → app.module`
edges, attribute search hits on `module`/`application`.

Refreshed 2026-07-06 for the live HRMS format: `{ events: [...] }` batch envelope unwrapped,
`status_code` (snake_case) honored, and `service`/`version`/`severity`/`message`/`tags.*`/
`memory_usage_mb` captured. Real HRMS batch of 7 `/roles`, `/company`, `/features`, … loader
hits maps to `HRMS WEB APPLICATION (LOCAL).<module>` lifelines. (Adapter renamed from
`adapter-id-2`.)

## Known limitations / open questions for the client

- **No causality between events** → service→service nesting (`propeak → hrms`) cannot be
  drawn; everything fans out from `client`. Fix requires the emitting library to add a
  span id + parent id (or at least a caller field). Phase-2 conversation.
- The trace-feed title is the **child operation** promoted onto the synthetic root — the
  endpoint (`GET /roles`) for instrumentation, or the `log …` / `audit …` label otherwise —
  instead of the opaque requestId. HRMS gives each event its own requestId (one child per
  trace), so this is unambiguous; when several events share a requestId the last-flushed one
  wins (window dedup is last-write-wins). *(Updated 2026-07-06; was the raw requestId.)*
- Log/audit arrows from `client` are presentation artifacts (prefixed `log`/`audit` in the
  label to keep them honest) — they mark *when* the event happened on *whose* lifeline.
- Does one requestId ever span multiple applications? (Assumed yes; harmless if no.)
