# Handoff

Rolling doc — current state of the Go migration. Contract: `docs/migration-plan.md`.
Repo: https://github.com/algorisys-oss/live-probing-go (`dev` is the working branch; `main` fast-forwards
to it — same workflow as `live-probing`'s CONTRIBUTING.md).

## Last task
**Phase 5, slice 2 — every remaining UI page, plus dev scripts (2026-10-27).**
- `./dev-start.sh` / `./dev-stop.sh` + `cmd/liveprobe-loadgen` (synthetic checkout / browse / login traffic).
  `--testbed [--chaos]` runs the TS Docker testbed against the Go server; `--watch` rebuilds on change.
- Pages: history (trends), day (rollups, throughput, top endpoints → `?endpoint=…#latency` with latency
  lines, histogram, heatmap, flamegraph with hover; slowest), search (GET form, empty fields dropped),
  errors, alerts (live list via the stream, ticking "firing for"), service, compare.
- Verified against the React UI on copies of a TS-written history DB: **all 866 chart shapes identical**
  attribute-by-attribute, day pages' rendered text identical, 24 browser checks; found + fixed the search
  limit (React asks for 200).
- 151 Go tests, race-clean. The whole UI is now Go; `PUBLIC_DIR` remains only for side-by-side checks.

## How to run / verify
- **`./dev-start.sh`** (synthetic traffic) or **`./dev-start.sh --watch`**, open http://localhost:4319;
  `./dev-stop.sh` to stop. `--testbed` uses the Docker testbed from `../live-probing`.
- `go test -race ./...` · `go vet ./...` · `go generate ./internal/ui` after editing `*.templ`.
- Parity harnesses: `tools/parity/README.md`. Browser checks so far are scratch Playwright scripts.

## Next
1. **Go e2e** with `chromedp` (skipped when no Chrome): port `../live-probing/e2e/*.spec.ts` plus the
   scratch checks (live 20, trace 16, pages 24).
2. Phase 6 — testbed in Go (shared OTel setup, 9 services, loadgen, compose + chaos overlay).
3. Phase 7 — Docker image, `serve.sh`, deploy, docs, drop `PUBLIC_DIR`, cutover.

## Gotchas
- **Never point a server at the real `../live-probing/packages/server/data/liveprobe.db`** without
  `RETENTION_DAYS=0` — its July 2026 data would be pruned at startup. Use a sample copy.
- Playwright MCP browser crashes on this machine; use headless `chromium.launch({channel:"chrome"})`.
- **`pkill -f <pattern>` kills the Bash tool's own shell** when the pattern text appears in the command.
  Use `pkill -x liveprobe` / `pkill -x liveprobe-loadg` — process names are truncated to **15 chars**.
  `dev-stop.sh` uses pid files and is safe.
- Selectors: Playwright `text=errors` hits the nav link; `input[name=q]` hits the header search box
  first — scope to `.depmatrix-metric` / `.search-form`.
- Pre-existing TS UI bug kept for parity: flow-graph arrowheads scale with edge stroke width.
- A missing trace returns HTTP 404 (the SPA returned 200) — Chrome logs it as a console error; expected.
- `LayoutSequence` (func) vs `SequenceLayout` (type); time is `int64` micros; `RetentionDays` is `*int`;
  tsx compiles `.ts` here as CJS → harness scripts are `.mts`.
