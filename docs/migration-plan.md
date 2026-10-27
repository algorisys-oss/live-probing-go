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
| UI | React + zustand + Vite SPA | **Server-rendered Go**: `templ` components, server-built SVG, a small vanilla-JS live client (SSE → pre-rendered fragments applied in one rAF batch). No htmx (nothing needs it). Assets embedded with `go:embed` → one binary. |
| testbed (Shopwave demo services) | `testbed/` (TS) | **Later phase**: Go services using the OTel Go SDK |
| e2e | Playwright (TS) | Go browser tests with `chromedp` (drives a local Chrome; skipped when absent) |
| packaging | Node Docker image, bash scripts, `deploy.js` | static binary in a distroless image; bash scripts kept; `deploy` as a Go cmd |

"100% Go" caveat, stated plainly: the browser still executes a little JavaScript (the ~400-line
live client, no framework). Everything that *runs on a server or is authored as application logic* is Go.

## Conventions

- Module `liveprobe`, Go ≥ 1.26. Binaries under `cmd/`, everything else under `internal/`.
- File names stay lowercase-hyphenated (`trace-window.go`). Tests are `trace-window_test.go`
  (the `_test` suffix is mandated by Go; it is the only underscore allowed).
- Minimal deps (LOOPS VIII). Approved so far: `modernc.org/sqlite`, `github.com/coder/websocket`,
  `github.com/rabbitmq/amqp091-go`, `github.com/a-h/templ` (+ `chromedp` for e2e, Phase 5). Anything else needs a written reason here.
- TDD: port the TS test first, see it fail, make it green. The TS suites are the parity oracle.
- **Wire compatibility**: JSON field names on `/v1/*`, `/api/*` and `/ws` messages are identical
  to the TS server, so the TS UI, collector, and e2e suite can be pointed at the Go server as a
  cross-check during migration.

## Known semantic deltas (deliberate, documented)

1. **Time is `int64` microseconds**, not float64. Fractional micros are truncated at ingest.
2. **Ordering**: JS `Map` iterates in insertion order; Go maps don't. `TraceWindow` keeps
   insertion sequence numbers so topology nodes/edges, trace ids and span lists come out in the
   same order the TS version produces.
3. **Number formatting is reproduced, not approximated**: `core.ParseJSNumber` (StringToNumber),
   `core.FormatJSNumber` (Number::toString), `core.ToFixed` (exact-decimal `toFixed`, ties up) and
   `jsRound` (`Math.round`) implement the ECMAScript algorithms, so every label matches byte-for-byte.
4. **Native-ingest attributes** are filtered to scalar values (string/number/bool). TS passes
   nested objects through unchecked, which contradicts its own type; Go enforces the type.
5. `localeCompare` on span ids → byte-wise compare (identical for hex/ASCII ids). For displayed
   attribute keys the UI uses an ICU-order comparator (`ui.icuLess`) that matches for ASCII keys.
6. Regex `\s` is ASCII-only in Go RE2 (JS also matches Unicode spaces) — affects only the
   Bearer-token mask on exotic whitespace.
7. **WS backpressure**: a `/ws` (or `/ui/live`) client whose 256-message buffer is full is disconnected;
   it reconnects and gets a fresh snapshot. The TS `ws` server buffered without bound.
8. **`attrs_text`** (attribute-search column) lists keys sorted per span, not in insertion order —
   only matters when a trace exceeds the 400-pair cap.
9. **V8 legacy `Date.parse` heuristics** (bare numbers like `"200"` → year 200, `"12"` → Dec 2001) are not
   reproduced; such strings parse to 0. Ingest clamps both outcomes to the same start time. ISO-8601,
   RFC 1123, `Date#toString()`, Java `+0000` offsets and common `YYYY/MM/DD`, `MM/DD/YYYY`,
   `May 28, 2026` forms match V8 (fuzz-verified).
10. **Out-of-range times saturate** at the int64 range in the collector (JS keeps e.g. 1e24); ingest
    clamps both to the same ceiling.
11. **UI routing**: an unknown path redirects to `/` (as React's catch-all did); a missing trace answers
    **HTTP 404** with the not-found page (the SPA answered 200).
12. **Header trace counter** shows the live window's recent traces (≤ 200) server-side; React counted the
    summaries its store had seen (≤ 200). Same number in steady state.

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

### Phase 5 — UI (server-rendered, `internal/ui`) — in progress
Oracle: the TS UI behaviour + `e2e/*.spec.ts`, `ui/src/lib/*.test.ts`.

Design (decided 2026-10-27):
- Pages are templ components rendered in Go from the same in-process state the API serves (the UI
  implements against a `ui.Backend` interface; the server implements it — no import cycle).
- Live updates: `/ui/live` (SSE) pushes **pre-rendered HTML fragments** per client view; a ~400-line
  vanilla `static/live.js` applies them in one rAF batch (LOOPS XXXIX). Layout, health colouring and all
  SVG geometry are Go. `/ws` JSON stays for API clients.
- View state lives in the URL (`?service=&view=&metric=`, `?tab=&expand=`, `#spanId`), so views are
  shareable and changing one is a plain navigation. Only zoom/pan, pause and waterfall collapse are client-side.
- No htmx (nothing needs it yet — LOOPS VIII). `templ` pinned via `go tool`; generated `*_templ.go` committed
  (`go generate ./internal/ui`). `styles.css` reused verbatim (+ a 6-line link-reset for URL-driven toggles).

- [x] pure UI logic in `internal/ui/view`: topology health, dependency matrix, flow layout, format —
      TS tests ported + differential vs TS (1,500 random topologies, 3,000 durations): identical
- [x] `core.ToFixed` = spec-exact `Number#toFixed` (exact-decimal rounding, ties up) — used by every label
- [x] shell + header (nav, live status pill, alert badge, counters, search, theme toggle)
- [x] live page: trace list (sorted, capped, flash, pause), RED tiles, flow graph (zoom/pan/fit, health,
      ghosts, export mmd/svg/png), dependency matrix (metric toggle), service filter — 20 browser checks
- [x] trace page: waterfall (self-time, critical path, collapse), span detail (ICU attribute order),
      sequence diagram (activations, N+1 groups expand/collapse), deep-link selection, copy mermaid,
      not-found — 16 browser checks; side-by-side screenshots match the React UI
- [ ] history, day (throughput/latency charts, histogram, heatmap, flamegraph), search, errors, alerts,
      service, compare pages
- [ ] e2e as Go tests (decision: `chromedp`, gated on a Chrome binary) covering `e2e/*.spec.ts` + the
      scratch checks above
- [ ] remove `PUBLIC_DIR` once every page is ported

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
