# LiveProbe (Go)

Go port of `../live-probing` (TypeScript). Same product: live sequence and flow diagrams of
running systems, built from observed spans, not parsed source. Read `../live-probing/CLAUDE.md`
for the product model; this file covers only what differs.

## Status

Mid-migration. `docs/migration-plan.md` is the contract and phase checklist. `HANDOFF.md` is the
current state. Start there.

## Layout

```
cmd/liveprobe/             server binary (ingest + API + WS + UI)            [phase 2]
cmd/liveprobe-collector/   collector binary                                   [phase 4]
internal/core/             event model, window, projections, masking, export  [phase 1]
internal/server/           HTTP/WS server                                     [phase 2]
internal/history/          SQLite history store                               [phase 3]
internal/collector/        sources + adapters                                 [phase 4]
internal/ui/               templ components, static assets (go:embed)         [phase 5]
testbed/                   Go Shopwave demo services                          [phase 6]
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

- `go test -race ./...` — all tests. `go vet ./...` — lint.
- `go build -o liveprobe ./cmd/liveprobe && ./liveprobe` — server (env: see `cmd/liveprobe/main.go`).
