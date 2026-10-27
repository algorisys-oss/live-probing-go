# LiveProbe (Go)

Go port of `../live-probing` (TypeScript). Same product: live sequence and flow diagrams of
running systems, built from observed spans, not parsed source. Read `../live-probing/CLAUDE.md`
for the product model; this file covers only what differs.

## Status

Migration complete (Phases 0–7). `docs/migration-plan.md` is the contract and records every
deliberate delta from TS; `HANDOFF.md` is the current state. Product docs are in `docs/`.

## Layout

```
cmd/liveprobe/             server binary (ingest + API + WS + UI)            [phase 2]
cmd/liveprobe-collector/   collector binary                                   [phase 4]
cmd/liveprobe-loadgen/     synthetic traffic for development (dev-start.sh)
cmd/liveprobe-deploy/      interactive deploy: docker build/save → ssh/scp → remote load  [phase 7]
internal/core/             event model, window, projections, masking, export  [phase 1]
internal/server/           HTTP/WS server                                     [phase 2]
internal/history/          SQLite history store                               [phase 3]
internal/collector/        sources + adapters                                 [phase 4]
internal/ui/               templ pages, /ui/live SSE, static assets (embed)   [phase 5]
internal/ui/view/          pure view logic (health, matrix, layout, format)   [phase 5]
testbed/                   Go Shopwave demo system — its own module (`shopwave`)  [phase 6]
```

Do not scaffold empty directories ahead of the code that fills them.

## Rules (from LOOPS.md, unchanged)

- Scope lock, TDD, contract first, disk not context.
- The TS test suites are the parity oracle: port the test, then the code.
- Wire format (JSON on `/v1`, `/api`, `/ws`) is identical to the TS server.
- Lowercase-hyphenated file names; Go test files are `name_test.go`.
- Minimal deps; the approved list is in `docs/migration-plan.md`.
- After every task update `HANDOFF.md` and tick `docs/migration-plan.md`.

## Commands

- `./dev-start.sh [--watch] [--testbed|--ts-testbed [--chaos]] [--no-load] [--fresh]` — build + run on :4319 with traffic;
  `./dev-stop.sh [--wipe]` stops it (pid files in `.dev/`, never pattern kills).
- `go test -race -short ./...` — unit + integration (fast). `go test ./e2e/` — browser e2e (headless Chrome, ~80s;
  `LIVEPROBE_CHROME` to pick a binary). `go vet ./...` — lint.
- `go build -o liveprobe ./cmd/liveprobe && ./liveprobe` — server (env: see `cmd/liveprobe/main.go`);
  `./serve.sh [--build] [--collector]` — the same for a real app; `docker compose [--profile collector|otel] up -d`.
- `liveprobe healthcheck` — exit 0 if the server on `$PORT` answers `/healthz` (the distroless image has no shell).
- `go generate ./internal/ui` — regenerate templ output after editing `*.templ` (commit the `*_templ.go`).

## UI rules

- Every number a label prints goes through `core.FormatJSNumber` / `core.ToFixed` / `view.FormatMicros`
  (spec-exact JS formatting) so the Go UI prints exactly what the React UI did.
- View state belongs in the URL; the browser script only does zoom/pan, pause, collapse and selection.
- Live fragments are rendered by the same templ components as the initial page — never two templates
  for one region.
- Toggle visibility with the `hidden` attribute (styles.css forces `[hidden] { display: none !important }`).
- e2e assertions check *visibility* (`checkVisibility()`), not attributes — that is what caught the
  `[hidden]` bug. SVG elements have no `.click()`; dispatch a bubbling `MouseEvent`.

## Testbed rules

- `testbed/` is a separate Go module: run its tests from there (`cd testbed && go test ./...`). Its
  dependencies (OTel SDK, pgx, go-redis, x/crypto) must never be imported by LiveProbe's module.
- Keep span shapes LiveProbe-friendly: route-named server spans with `http.route`, `pg.query:<VERB>`
  queries, producer/consumer spans carrying the trace across RabbitMQ.
