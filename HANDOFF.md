# Handoff

Rolling doc — current state of the Go migration. Contract: `docs/migration-plan.md`.
Repo: https://github.com/algorisys-oss/live-probing-go (`dev` is the working branch; `main` fast-forwards
to it — same workflow as `live-probing`'s CONTRIBUTING.md).

## Last task
**Phase 4 — collector (2026-10-27).** (Phases 0–3 — plan, core, server, history — done earlier the same day.)
- `internal/collector`: sources (http, stdin NDJSON, RabbitMQ via `amqp091-go` behind a small
  `Conn`/`Channel`/`Delivery` interface with reconnect/backoff), adapters (`adapter-id-1`,
  `algo-instrumentation`, `liveprobe-native`), batching `Sink`. `cmd/liveprobe-collector` with TS env parity.
- All TS collector tests ported + extras; **real RabbitMQ 3.13 integration test** passes, including a
  broker-forced disconnect and reconnect.
- **Adapter fuzz vs TS (20k inputs)**: identical except documented deltas. It caught 3 real bugs, fixed by
  new `core.ParseJSNumber` / `FormatJSNumber` / `SaturatingMicros` (spec-exact JS coercions, now used
  by all ingest paths and history's `attrs_text`).
- Also earlier today: server + history verified route-for-route against the TS server, and the React UI
  runs on the Go binary.

## How to run / verify
- `go test -race ./...` · `go vet ./...`
- `go build ./cmd/...` → `liveprobe` (server) + `liveprobe-collector`.
- Parity harnesses (core, server, adapters): `tools/parity/README.md`.

## Next
**Phase 5 — server-rendered UI** (`internal/ui`): templ + htmx, server-built SVG, small vanilla live
client. Start by porting the pure UI logic with its tests (`../live-probing/packages/ui/src/lib/`:
topology-health, dependency-matrix, layout, format, sequence-layout is already in core), then the shell
+ live page, then trace page, then the rest. Decide `templ` codegen workflow (commit generated files or
`go generate` in CI) and the Go e2e driver (playwright-go vs chromedp) at the start.

## Gotchas
- **Never point a server at the real `../live-probing/packages/server/data/liveprobe.db`** without
  `RETENTION_DAYS=0` — its July 2026 data would be pruned at startup. Use a sample copy.
- The Playwright MCP browser crashes on this machine; headless Chrome via a playwright script works.
- `pkill -f <pattern>` from the Bash tool can kill the tool's own shell; use `pkill -x liveprobe` or a
  bracketed pattern like `'[t]sx packages/server'`.
- A far-future event (clamped to now+60s) legitimately evicts older traces from the live window.
- Layout **function** `LayoutSequence`, **type** `SequenceLayout`; time is `int64` micros; `RetentionDays`
  is `*int`; `MaskOptions.SkipIdentityFields` inverts TS `maskIdentityFields`.
- tsx compiles `.ts` here as CJS (no package.json): harness scripts that use top-level await are `.mts`.
- A throwaway broker for the integration test: `docker run -d --rm --name lp-go-rabbit-test -p 5699:5672 rabbitmq:3.13-alpine`.
