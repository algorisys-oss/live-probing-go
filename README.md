# LiveProbe (Go)

Live sequence and flow diagrams of running systems, drawn from observed runtime behavior
rather than parsed source. This is the Go port of [`../live-probing`](../live-probing) (TypeScript).
The migration is in progress. The server and collector are feature-complete and verified against
the TS implementation; the UI and testbed are still to come.

Send LiveProbe OpenTelemetry traces (OTLP/HTTP JSON) or its native event JSON. It keeps a rolling
live window, persists history to SQLite by UTC day, and serves a REST API and a live WebSocket
stream with per-trace sequence data, aggregate topology, per-service RED metrics and alerts.

## Status

| Part                                                         | State                                        |
| ------------------------------------------------------------ | -------------------------------------------- |
| Core: event model, OTLP/native ingest, PII masking, window, sequence/layout, critical path, RED, alerts, flamegraph, Mermaid | **done**, output identical to TS on real data |
| Server: ingest, REST, `/ws`, CORS/origin policy, alert webhook | **done**, every route identical to TS server |
| History: SQLite by UTC day, search, retention                 | **done**, reads databases the TS server wrote |
| Collector: http / stdin / RabbitMQ sources, adapters          | **done**, adapters identical to TS under 20k-input fuzz |
| UI: server-rendered Go (templ, server-built SVG, SSE live fragments) | **in progress**: live + trace pages done |
| Shopwave testbed in Go                                        | Phase 6                                      |
| Docker image, run scripts, deploy                             | Phase 7                                      |

The phase checklists are in [docs/migration-plan.md](docs/migration-plan.md), and the current
state is in [HANDOFF.md](HANDOFF.md).

## Quick start

Requires Go 1.26+. There is no cgo; SQLite is the pure-Go `modernc.org/sqlite`.

The fastest way to see it working:

```bash
./dev-start.sh            # build + run on :4319 with synthetic traffic → open http://localhost:4319
./dev-start.sh --watch    # same, rebuilding + restarting on every .go / .templ / static change
./dev-start.sh --testbed  # real traffic from the Shopwave Docker testbed in ../live-probing (add --chaos for failures)
./dev-stop.sh             # stop everything (--wipe also deletes .dev/data and the testbed volumes)
```

`dev-start.sh` keeps its binaries, logs, pids and history DB under `.dev/` (gitignored); `--fresh`
keeps history in memory, `--no-load` skips traffic. The synthetic traffic comes from
`cmd/liveprobe-loadgen` (`TRACES_PER_SEC`, `ERROR_RATE`, `DURATION_SECONDS`): checkout with an N+1
query and a flaky order service, cached product browsing, and logins.

To run the server by hand:

```bash
go build -o liveprobe ./cmd/liveprobe
PORT=4319 ./liveprobe          # history goes to ./data/liveprobe.db
```

Send it a trace:

```bash
curl -s -XPOST localhost:4319/v1/events -H 'content-type: application/json' -d '{"events":[
  {"traceId":"t1","spanId":"a","participant":"gateway","operation":"GET /products","kind":"server",
   "startTime":'"$(date +%s%6N)"',"duration":12000,"status":"ok","attributes":{}},
  {"traceId":"t1","spanId":"b","parentSpanId":"a","participant":"gateway","peer":"postgresql",
   "operation":"SELECT products","kind":"client","startTime":'"$(date +%s%6N)"',"duration":4000,"status":"ok","attributes":{}}]}'

curl -s localhost:4319/api/traces/t1 | head -c 400     # sequence + waterfall rows + Mermaid
curl -s localhost:4319/api/topology                    # aggregate graph + Mermaid flow
```

To send traces from an OpenTelemetry-instrumented app, configure an OTel collector to export
**OTLP/HTTP JSON** to `http://<host>:4319/v1/traces`. Integration is the same as for the TS
server; see [../live-probing/docs/integrating-your-app.md](../live-probing/docs/integrating-your-app.md).

### UI

Open `http://localhost:4319`. The UI is rendered by the Go binary (no Node, no build step): the
live view (trace feed, RED tiles, health-coloured flow graph ⇄ dependency matrix) and the trace page
(waterfall with critical path, sequence diagram with N+1 grouping) are done; the history, day,
search, errors, alerts, service and compare pages are still being ported.

Until they are, the Go server can host the existing React UI instead:

```bash
(cd ../live-probing && npm ci && npm run build -w @liveprobe/ui)
PUBLIC_DIR=../live-probing/packages/ui/dist PORT=4319 ./liveprobe
```

## Configuration

All configuration is by environment variable, with the same names and defaults as the TS server.

| Variable | Default | Meaning |
| --- | --- | --- |
| `PORT` | `4318` | HTTP port (ingest, API, `/ws`) |
| `HOST` | `127.0.0.1` | Bind address. Ingest and API have **no auth**, so `0.0.0.0` is opt-in |
| `DB_PATH` | `./data/liveprobe.db` | SQLite history file, or `:memory:` |
| `RETENTION_DAYS` | `30` | Keep the N most recent UTC days; pruned at startup and hourly. `0` disables |
| `LIVE_WINDOW_MINUTES` | `5` | How long a trace stays in the live window |
| `MAX_TRACES` | `2000` | Cap on traces held live in memory |
| `CORS_ORIGINS` | — | Comma-separated browser origins allowed in addition to loopback |
| `MASK_PII` | on | `off` disables PII/secret scrubbing at ingest (not recommended) |
| `ALERT_WEBHOOK_URL` | — | POST target, fired once each time an alert starts firing |
| `ALERT_WEBHOOK_TIMEOUT_MS` / `_RETRIES` / `_COOLDOWN_MS` | `5000` / `2` / `60000` | Webhook delivery tuning |
| `PUBLIC_DIR` | — | Serve a built UI from disk (migration aid, Go-only) |

> ⚠️ Retention runs at startup. If you open an existing database whose data is older than
> `RETENTION_DAYS`, those days are **deleted**. Set `RETENTION_DAYS=0` to inspect old data.

## Collector

`liveprobe-collector` bridges telemetry that isn't OTLP: one **source** (transport) + one **adapter**
(format) → batched `POST /v1/events`. Same env vars as the TS collector.

```bash
go build -o liveprobe-collector ./cmd/liveprobe-collector

SOURCE=http ADAPTER=algo-instrumentation HTTP_PORT=4320 ./liveprobe-collector   # POST JSON events, any path
SOURCE=stdin ADAPTER=adapter-id-1 ./liveprobe-collector < events.ndjson
SOURCE=rabbitmq ADAPTER=adapter-id-1 RABBITMQ_URL=amqp://… QUEUE=liveprobe.tap \
  EXCHANGE=instrumentation ROUTING_KEY='#' QUEUE_MAX_LENGTH=100000 ./liveprobe-collector  # non-intrusive tap
```

| Variable | Default | Meaning |
| --- | --- | --- |
| `SOURCE` | `stdin` | `stdin`, `http` or `rabbitmq` |
| `ADAPTER` | `adapter-id-1` | `adapter-id-1`, `algo-instrumentation`, `liveprobe-native` |
| `LIVEPROBE_URL` | `http://localhost:4319` | Where batches are POSTed (`/v1/events`) |
| `HTTP_PORT` | `4320` | `http` source port |
| `RABBITMQ_URL` / `QUEUE` | `amqp://localhost:5672` / `instrumentation.events` | Broker and queue |
| `EXCHANGE` / `EXCHANGE_TYPE` / `ROUTING_KEY` | — / — / `#` | Tap an exchange; without a type it is only verified (passive) |
| `QUEUE_MAX_LENGTH` / `QUEUE_MESSAGE_TTL_MS` | — | Bound the tap queue (drop-head) while the collector is down |

Adapter field mappings are documented in
[../live-probing/docs/integration-adapters.md](../live-probing/docs/integration-adapters.md).

## API

The wire format is identical to the TS server, so existing clients work unchanged.

| Method & path | Returns |
| --- | --- |
| `POST /v1/traces` | OTLP/HTTP JSON (gzip ok). 8 MB body cap, 32 MB decompressed |
| `POST /v1/events` | Native events: `[...]` or `{"events":[...]}` → `{"accepted":n}` |
| `GET /api/traces?limit=` | Recent trace summaries from the live window |
| `GET /api/traces/:id` | Trace detail (live, falling back to history): sequence, spans, critical path, Mermaid |
| `GET /api/topology` | Live service/datastore graph + Mermaid flow |
| `GET /api/service/:name` | Inbound/outbound deps, top operations, error rate |
| `GET /api/errors?limit=` | Errored traces grouped by endpoint + error label |
| `GET /api/search?q=&service=&attr=&error=&minMs=&maxMs=&minSpans=&traceId=&sort=&limit=` | History search |
| `GET /api/days` | Days with request/error counts |
| `GET /api/day/:day/{summary,traces,latency,flamegraph}` | Day rollups; `latency` and `flamegraph` take `?endpoint=` |
| `GET /healthz` | `{"status":"ok"}` |
| `WS /ws` | `snapshot` on connect, then `traces` deltas and `topology` ticks (with `red`, `alerts`) |

## Layout

```
cmd/liveprobe/        server binary
cmd/liveprobe-collector/  collector binary (sources + adapters → /v1/events)
cmd/liveprobe-loadgen/    synthetic multi-service traffic for development
dev-start.sh / dev-stop.sh  dev runner (synthetic traffic or the Docker testbed, optional watch mode)
internal/core/        event model and every pure projection of it (stdlib only)
internal/history/     SQLite history store (same schema as TS)
internal/server/      HTTP + WebSocket server, alert webhook
internal/collector/   http / stdin / RabbitMQ sources, adapters, batching sink
internal/ui/          server-rendered UI: templ pages, /ui/live SSE stream, static assets (embedded)
internal/ui/view/     pure view logic: health, matrix, flow layout, formatting
tools/parity/         differential harnesses against the TS implementation
docs/                 migration plan (the contract)
```

## Development

```bash
go test -race ./...     # unit + integration tests (all TS suites ported)
go vet ./...
go generate ./internal/ui   # after editing *.templ (generated *_templ.go files are committed)
```

The TS implementation is the oracle. Each TS test suite is ported before the code it covers.
[tools/parity/](tools/parity/README.md) also runs real recorded traffic through both
implementations and diffs the results: core projections, every server route and the WebSocket
snapshot, and the collector adapters under a seeded fuzz corpus. Known, deliberate differences are listed in
[docs/migration-plan.md](docs/migration-plan.md#known-semantic-deltas-deliberate-documented).

Conventions: file names are lowercase and hyphenated (`trace-window.go`), and tests are
`*_test.go`. Keep dependencies minimal; the approved list is in the migration plan. After each
task, update `HANDOFF.md` and tick the plan. See [CLAUDE.md](CLAUDE.md).
