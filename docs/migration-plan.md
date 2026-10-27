# LiveProbe → Go migration plan

Goal: a 100% Go LiveProbe at feature parity with `../live-probing` (TypeScript), then cut over.
This file is the contract (LOOPS XXIX): each phase has a testable "done" checklist. Grade
against it; do not rubber-stamp.

## Scope decisions (agreed 2026-10-27)

| Area | TS today | Go target |
|---|---|---|
| core (event model, window, projections, masking, exporters) | `packages/core` | `internal/core` — stdlib only |
| server (OTLP + native ingest, REST, WebSocket, alerts, webhook) | `packages/server` (node:http, ws) | `internal/server` + `cmd/liveprobe` |
| history (SQLite day-partitioned store) | `node:sqlite` | `internal/history` — `modernc.org/sqlite` (pure Go, no cgo) — **same schema**, existing `.db` files stay readable |
| collector (http/stdin/RabbitMQ sources, adapters) | `packages/collector` (amqplib) | `internal/collector` + `cmd/liveprobe-collector` — `rabbitmq/amqp091-go` |
| UI | React + zustand + Vite SPA | **Server-rendered Go**: `templ` components + `htmx`, server-built SVG for diagrams, a small vanilla-JS live client (WebSocket → swap fragments / rAF-batched SVG patch). Assets embedded with `go:embed` → one binary. |
| testbed (Shopwave demo services) | `testbed/` (TS) | **Later phase**: Go services using the OTel Go SDK |
| e2e | Playwright (TS) | Go browser tests (`playwright-go` or `chromedp`) — chosen in Phase 5 |
| packaging | Node Docker image, bash scripts, `deploy.js` | static binary in a distroless image; bash scripts kept; `deploy` as a Go cmd |

"100% Go" caveat, stated plainly: the browser still executes a little JavaScript (htmx and the
live client). Everything that *runs on a server or is authored as application logic* is Go.

## Conventions

- Module `liveprobe`, Go ≥ 1.26. Binaries under `cmd/`, everything else under `internal/`.
- File names stay lowercase-hyphenated (`trace-window.go`). Tests are `trace-window_test.go`
  (the `_test` suffix is mandated by Go; it is the only underscore allowed).
- Minimal deps (LOOPS VIII). Approved so far: `modernc.org/sqlite`, `github.com/coder/websocket`,
  `github.com/rabbitmq/amqp091-go`, `github.com/a-h/templ`. Anything else needs a written reason here.
- TDD: port the TS test first, see it fail, make it green. The TS suites are the parity oracle.
- **Wire compatibility**: JSON field names on `/v1/*`, `/api/*` and `/ws` messages are identical
  to the TS server, so the TS UI, collector, and e2e suite can be pointed at the Go server as a
  cross-check during migration.

## Known semantic deltas (deliberate, documented)

1. **Time is `int64` microseconds**, not float64. Fractional micros are truncated at ingest.
2. **Ordering**: JS `Map` iterates in insertion order; Go maps don't. `TraceWindow` keeps
   insertion sequence numbers so topology nodes/edges, trace ids and span lists come out in the
   same order the TS version produces.
3. **Rounding**: JS `Math.round` rounds .5 toward +∞ and `toFixed` rounds ties up; helpers
   `jsRound`/`toFixed` reproduce that, so alert messages and averages match byte-for-byte.
4. **Native-ingest attributes** are filtered to scalar values (string/number/bool). TS passes
   nested objects through unchecked, which contradicts its own type; Go enforces the type.
5. `localeCompare` on span ids → byte-wise compare. Identical for hex/ASCII ids.
6. Regex `\s` is ASCII-only in Go RE2 (JS also matches Unicode spaces) — affects only the
   Bearer-token mask on exotic whitespace.

## Phases

### Phase 0 — scaffold + plan ✅
- [x] `live-probing-go/` with `go.mod`, `CLAUDE.md`, `HANDOFF.md`, this plan.

### Phase 1 — core (`internal/core`) ✅
Port of `packages/core/src/*` with every TS test ported.
- [x] event model, `ClampMicros`, `StartTimeCeiling`, `DatastoreSystems`
- [x] `CoerceEvents` (native ingest)
- [x] `NormalizeOTLP` (OTLP/HTTP JSON; string-or-number fields, BigInt-safe nanos)
- [x] masking (`MaskString`, `MaskPII`, `MaskEvents`)
- [x] `TraceWindow` (add/prune/assemble/topology), goroutine-safe
- [x] `SequenceFor`, `SequenceLayout` (groups, activations), `CriticalPath`
- [x] `RedMetrics`, `EvaluateAlerts`, `AggregateFlamegraph`, latency buckets
- [x] Mermaid exporters
- [x] `go test ./internal/core/...` green (59 tests), race-clean, `go vet` clean
- [x] differential parity vs TS core on 1.5k real traces: PARITY OK (`tools/parity/`)

### Phase 2 — server (`internal/server`, `cmd/liveprobe`) ✅
Oracle: `packages/server/src/server.test.ts`, `alert-webhook.test.ts`, summary.ts.
- [x] `POST /v1/traces` (OTLP JSON, gzip) and `POST /v1/events` (native), 8 MB body cap → 413, 32 MB gzip cap
- [x] masking at ingest (`MASK_PII=off` opt-out)
- [x] REST: `/api/traces`, `/api/traces/:id` (live → history fallback), `/api/topology`, `/api/service/:name`,
      `/api/errors`, `/api/day/:d/{summary,traces,latency,flamegraph}`, `/api/search`, `/api/days`, `/healthz`
- [x] WebSocket `/ws`: `snapshot`, `traces`, `topology` (+ `red`, `alerts`) with identical shapes
- [x] Origin/CORS policy (loopback always, `CORS_ORIGINS` extra; cross-site WS → 403), default bind `127.0.0.1`
- [x] alert tick with `since` tracking + webhook notifier (timeout, backoff, per-id cooldown)
- [x] env parity: `PORT HOST DB_PATH CORS_ORIGINS MASK_PII RETENTION_DAYS LIVE_WINDOW_MINUTES MAX_TRACES ALERT_WEBHOOK_*`
      (+ `PUBLIC_DIR`, a migration aid to serve a built UI from disk)
- [x] all 9 server + 7 webhook TS tests ported (+1 service-detail test), race-clean
- [x] server differential vs the TS server (`tools/parity/server-parity.mjs`): 68 routes w/ hostile input,
      88 routes + WS snapshot w/ populated live window, 118 routes over a TS-written history DB — all identical
- [x] cross-check: the TS React UI served by the Go binary — live view (WS, RED tiles, health graph, alerts badge),
      trace waterfall, search/history/errors/alerts pages render with zero console errors
- [ ] TS Playwright e2e suite against the Go server (not run: needs the e2e webServer config pointed at Go;
      superseded by Phase 5's Go e2e anyway)

### Phase 3 — history (`internal/history`) ✅
Oracle: `history-store.test.ts`.
- [x] same SQLite schema + indexes + `ensureColumn` migrations; staged writes + flush interval; read-your-writes on `/api`
- [x] day partitions, retention pruning (startup + hourly, VACUUM), search, errors, endpoint latency/distribution/flamegraph
- [x] opens a DB written by the TS server unchanged (verified on 11.3k real rows / 4 days; old-format
      `detail_json` rows pass through verbatim)
- [x] 5 TS tests ported + 2 (latency/flamegraph/error-groups, file reopen)

### Phase 4 — collector (`internal/collector`, `cmd/liveprobe-collector`) ✅
Oracle: `sources/*.test.ts`, `adapters/*.test.ts`.
- [x] sources: http (any path, array fan-out, 5 MB cap), stdin (NDJSON), rabbitmq (queue / exchange tap,
      passive-verify or declare, routing keys, max-length + drop-head, TTL, prefetch 64, poison → nack
      without requeue, reconnect with exponential backoff, fail-fast initial connect)
- [x] adapters: `algo-instrumentation`, `adapter-id-1`, `liveprobe-native`; batching sink → `LIVEPROBE_URL`
- [x] env parity: `SOURCE ADAPTER HTTP_PORT RABBITMQ_URL QUEUE EXCHANGE EXCHANGE_TYPE ROUTING_KEY QUEUE_MAX_LENGTH QUEUE_MESSAGE_TTL_MS LIVEPROBE_URL`
- [x] all 2 + 15 adapter, 4 http, 9 rabbitmq TS tests ported (+ stdin, sink, panic-nack, js-coercion tests)
- [x] real-broker integration test (opt-in, `LIVEPROBE_AMQP_URL`): declare/bind/bounded args verified on
      RabbitMQ 3.13, poison + unbound key dropped, broker-forced disconnect → reconnect → consumption resumes
- [x] adapter fuzz differential vs TS (`tools/parity/adapter-parity.mts`, 20k seeded mutated inputs):
      `adapter-id-1` and `algo-instrumentation` identical; `liveprobe-native` differs only by deltas #1 and #4.
      The fuzz found and fixed 3 real divergences (JS `String(1e21)`, `Number("0x10")`, int64 overflow) —
      `core.ParseJSNumber`/`FormatJSNumber` now implement the ECMAScript algorithms for all of ingest.

### Phase 5 — UI (server-rendered, `internal/ui`)
Oracle: the TS UI behaviour + `e2e/*.spec.ts`, `ui/src/lib/*.test.ts`.
- [ ] port pure UI logic to Go: topology-health, dependency-matrix, layout (flow), format, latency buckets
- [ ] layout shell + header (nav, alert badge, theme toggle), styles (`styles.css` reused)
- [ ] pages: live (RED tiles, flow graph ⇄ matrix), trace (sequence, waterfall, critical path, export),
      search, history, day, service, errors, alerts, compare
- [ ] server-built SVG: sequence, flow, waterfall, flamegraph, histogram, heatmap, throughput/latency/trends charts
- [ ] live client: WS → batched rAF updates (LOOPS XXXIX), no full-page reloads
- [ ] diagram export (Mermaid / SVG download)
- [ ] e2e ported to Go; all scenarios from `e2e/*.spec.ts` pass

### Phase 6 — testbed in Go (`testbed/`)
- [ ] shared lib (OTel Go SDK setup, chaos + resilient fetch, pg/redis/amqp helpers) with tests
- [ ] services: gateway, auth, catalog, cart, order, payment-worker, inventory-worker,
      notification-worker, loadgen
- [ ] docker-compose (+ dev, chaos overlays) — same topology & traffic shape as TS testbed

### Phase 7 — packaging + cutover
- [ ] multi-stage Dockerfile → distroless static image; healthcheck via the binary
- [ ] `dev-start.sh`, `dev-stop.sh`, `serve.sh` equivalents; `deploy` as a Go command
- [ ] docs ported (README, architecture, integrating-your-app)
- [ ] parity sign-off against this checklist; TS tree archived
