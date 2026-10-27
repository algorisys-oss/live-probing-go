# Handoff

Rolling doc — current state of the Go migration. Contract: `docs/migration-plan.md`.
Repo: https://github.com/algorisys-oss/live-probing-go (`dev` is the working branch; `main` fast-forwards
to it — same workflow as `live-probing`'s CONTRIBUTING.md).

## Last task
**Phase 5, slice 1 — server-rendered UI: live page + trace page (2026-10-27).**
- Design (see plan): templ pages; `/ui/live` SSE pushes pre-rendered HTML fragments per client view;
  `static/live.js` (vanilla, ~400 lines) applies them in one rAF batch; view state in the URL.
- `internal/ui/view`: health, matrix, layout, format — TS tests ported + **differential vs TS identical**
  (1,500 topologies / 3,000 durations; `tools/parity/view-parity.mts`).
- `core.ToFixed`: spec-exact `Number#toFixed` (Node-verified cases).
- Server: `ui.Backend` implemented (`LiveState`, `Subscribe`, `TraceDetail`, `ServiceDetail`, `History`);
  the tick computes live state once and fans out to `/ws` and UI subscribers. `FiringAlert` and
  `ServiceDetail` moved to `core`. Non-API GETs go to the Go UI unless `PUBLIC_DIR` is set.
- **Live page** and **trace page** done; verified in headless Chrome against live traffic (20 + 16 checks)
  and side-by-side screenshots with the React UI (pixel-near-identical).
- 144 Go tests, race-clean.

## How to run / verify
- **`./dev-start.sh`** (synthetic traffic) or **`./dev-start.sh --watch`**, open http://localhost:4319;
  `./dev-stop.sh` to stop. `--testbed` uses the Docker testbed from `../live-probing`.
- `go test -race ./...` · `go vet ./...` · `go generate ./internal/ui` after editing `*.templ`.
- `go build -o liveprobe ./cmd/liveprobe && PORT=4319 DB_PATH=:memory: ./liveprobe`, open http://localhost:4319.
- Browser checks used so far live in the session scratchpad (Playwright, headless Chrome); they become
  Go `chromedp` tests in the e2e step.

## Next
1. Remaining pages: alerts (simplest), errors, search, history, day (throughput/latency charts,
   histogram, heatmap, flamegraph), service, compare. TS sources: `../live-probing/packages/ui/src/pages/`.
2. Go e2e with `chromedp` (gated on Chrome) porting `e2e/*.spec.ts` + the scratch checks.
3. Drop `PUBLIC_DIR` once every page is ported. Then Phase 6 (testbed) / 7 (packaging).

## Gotchas
- **Never point a server at the real `../live-probing/packages/server/data/liveprobe.db`** without
  `RETENTION_DAYS=0` — its July 2026 data would be pruned at startup. Use a sample copy.
- Playwright MCP browser crashes on this machine; use headless `chromium.launch({channel:"chrome"})`.
- **`pkill -f <pattern>` kills the Bash tool's own shell whenever the pattern text appears anywhere in
  that command** (even later in the script). Put kills in their own Bash call; prefer `pkill -x liveprobe`.
- Pre-existing TS UI bug kept for parity: flow-graph arrowheads scale with edge stroke width
  (`markerUnits` default), so busy edges get huge heads. Fix in both or neither.
- Playwright `text=errors` is case-insensitive and hits the **Errors** nav link — scope selectors.
- The UI's `sequence-layout.ts` is the core algorithm verbatim → `core.LayoutSequence` serves the diagram.
- A missing trace returns HTTP 404 (the SPA returned 200) — Chrome logs it as a console error; expected.
- Earlier gotchas still apply: `LayoutSequence` (func) vs `SequenceLayout` (type); time is `int64` micros;
  `RetentionDays` is `*int`; tsx compiles `.ts` here as CJS → harness scripts are `.mts`.
