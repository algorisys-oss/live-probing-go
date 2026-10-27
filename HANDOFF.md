# Handoff

Rolling doc — current state of the Go migration. Contract: `docs/migration-plan.md`.
Repo: https://github.com/algorisys-oss/live-probing-go (`dev` is the working branch; `main` fast-forwards
to it — same workflow as `live-probing`'s CONTRIBUTING.md).

## Last task
**Phase 5 complete — Go browser e2e (2026-10-27).** Earlier today: phases 0–4, the whole server-rendered UI,
and `dev-start.sh` / `dev-stop.sh`.
- `e2e/` (chromedp v0.21, headless system Chrome; in-process server on `:memory:`): all 22 TS Playwright
  specs ported on the same seed fixtures + 9 Go-UI behaviours → **31 tests, all green** (~80s).
- They caught a real bug: author `display` rules (e.g. `.wf-row { display:flex }`) overrode `[hidden]`, so
  collapsed waterfall rows and other hidden regions stayed visible. Fixed globally in `styles.css`.
- Phase 5 is done: every page is Go, verified against React (chart shapes, rendered text, behaviour).
- All servers/test processes stopped; scratch DB copies removed.

## How to run / verify
- **`./dev-start.sh`** (synthetic traffic) or **`./dev-start.sh --watch`**, open http://localhost:4319;
  `./dev-stop.sh` to stop. `--testbed` uses the Docker testbed from `../live-probing`.
- `go test -race -short ./...` (fast) · `go test ./e2e/` (browser) · `go vet ./...` ·
  `go generate ./internal/ui` after editing `*.templ`.
- Parity harnesses vs the TS implementation: `tools/parity/README.md`.

## Next
1. **Phase 6 — testbed in Go**: shared OTel Go SDK setup + chaos/resilient helpers (port
   `../live-probing/testbed/packages/shared` with its tests), then gateway, auth, catalog, cart, order,
   payment/inventory/notification workers, loadgen; docker-compose + dev/chaos overlays. Until then,
   `./dev-start.sh --testbed` runs the TS testbed against the Go server.
2. **Phase 7** — Docker image (distroless), `serve.sh`, deploy command, docs, drop `PUBLIC_DIR`, cutover.

## Gotchas
- **Never point a server at the real `../live-probing/packages/server/data/liveprobe.db`** without
  `RETENTION_DAYS=0` — its July 2026 data would be pruned at startup. Use a sample copy.
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
