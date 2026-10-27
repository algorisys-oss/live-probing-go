# Handoff

Rolling doc — current state of the Go migration. Contract: `docs/migration-plan.md`.
Repo: https://github.com/algorisys-oss/live-probing-go (`dev` is the working branch; `main` fast-forwards
to it — same workflow as `live-probing`'s CONTRIBUTING.md).

## Last task
**Phase 7 complete: packaging + cutover (2026-10-28). The migration is done (Phases 0–7).**
- `Dockerfile` (golang:1.26-alpine → `distroless/static-debian12:nonroot`, ~35 MB, `liveprobe` +
  `liveprobe-collector`). The healthcheck is the new `liveprobe healthcheck` subcommand. `docker-compose.yml`
  has the TS file's `collector` and `otel` profiles, plus `otel-collector-config.yaml`. Verified: the stack
  comes up healthy and the collector forwards.
- `serve.sh` (`--build`, `--collector`). `cmd/liveprobe-deploy` ports `scripts/deploy.js`; its pure parts
  are unit-tested. **It has never been run against a real host.**
- Product docs ported to `docs/` (index: `docs/README.md`). Links into the TS repo go to GitHub.
- `PUBLIC_DIR` (React UI hosting) removed.
- Sign-off: core (1.5k traces), adapter fuzz (20k inputs; only documented deltas #1/#4) and server
  (88 + 68 routes, WS snapshot) differentials re-run, all identical. Unit/race, e2e (31) and testbed tests
  green.
- Fixed two flakes. (1) An e2e test named traces with a 19-digit nanosecond number; about 10% of those
  pass Luhn, get masked as card numbers, and the test never found its row. Ids are base 36 now.
  (2) server-parity compared WS alerts in `since` order, which is wall-clock; they are compared as a set now.

## How to run / verify
- **`./dev-start.sh`** (synthetic traffic), `--watch`, `--testbed [--chaos]` (Go Shopwave in Docker); open
  http://localhost:4319; `./dev-stop.sh [--wipe]`.
- **`./serve.sh [--collector]`** for a real app, or **`docker compose up -d`**.
- `go test -race -short ./...` · `go test ./e2e/` · `(cd testbed && go test ./...)` · `go vet ./...` ·
  `go generate ./internal/ui` after editing `*.templ`.
- Parity harnesses vs the TS implementation: `tools/parity/README.md`.

## Next
- **Archiving the TS tree is the owner's call.** Only optional things here still reference `../live-probing`:
  the parity harnesses, `dev-start.sh --ts-testbed`, and GitHub links in the docs.
- Not ported (by decision): the testbed's React storefront SPA and `examples/otel-react-go`.
- `liveprobe-deploy` should get a first supervised run against a staging host.

## Gotchas
- **Never point a server at the real `../live-probing/packages/server/data/liveprobe.db`** without
  `RETENTION_DAYS=0` — its July 2026 data would be pruned at startup. Use a sample copy.
- Test data with long digit runs (timestamps, ids) can be masked at ingest (Luhn → card number). Use base 36.
- Playwright MCP browser crashes on this machine; use headless `chromium.launch({channel:"chrome"})`.
- **`pkill -f <pattern>` kills the Bash tool's own shell** when the pattern text appears in the command.
  Use `pkill -x liveprobe` / `pkill -x liveprobe-loadg` — process names are truncated to **15 chars**.
  `dev-stop.sh` uses pid files and is safe.
- chromedp v0.21 is the new generic API (`Do`, `Run`, `Evaluate[T]`, `chromedp.CSS(...)`, `Console` iterator)
  — older examples online won't compile. SVG elements have no `.click()`: dispatch a `MouseEvent`.
- Selectors: Playwright `text=errors` hits the nav link; `input[name=q]` hits the header search box
  first — scope to `.depmatrix-metric` / `.search-form`.
- Pre-existing TS UI bug kept for parity: flow-graph arrowheads scale with edge stroke width.
- A missing trace returns HTTP 404 (the SPA returned 200) — Chrome logs it as a console error; expected.
- `LayoutSequence` (func) vs `SequenceLayout` (type); time is `int64` micros; `RetentionDays` is `*int`;
  tsx compiles `.ts` here as CJS → harness scripts are `.mts`.
