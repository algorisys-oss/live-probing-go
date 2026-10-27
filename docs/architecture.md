# Architecture

Two systems live in this repo:

- **LiveProbe** (`cmd/`, `internal/`): the tool. It consumes OpenTelemetry traces and renders
  live sequence and flow diagrams of whatever system produced them. One Go binary holds
  ingest, API, history and UI.
- **Shopwave testbed** (`testbed/`): a real distributed e-commerce system that emits those
  traces. It gives LiveProbe an honest, richly instrumented system to observe. It is a separate
  Go module (`shopwave`), so its dependencies never enter LiveProbe's.

The guiding idea is to **draw observed behavior, not parsed source.** Everything LiveProbe shows
comes from what actually executed at runtime.

## The whole pipe

```
 Shopwave services (8) ──OTLP──►  OTel Collector ──┬─OTLP/gRPC─►  Jaeger        (reference UI)
 (OTel Go SDK)                                     │
                                                   └─OTLP/JSON─►  LiveProbe server (cmd/liveprobe)
                                                                    │  POST /v1/traces
                                                                    ▼
                                                   NormalizeOTLP → TraceWindow (rolling)
                                                                    │        │
                                                              sequence   topology (+ RED, alerts)
                                                                    │        │
                                         REST + /ws (JSON)  ◄───────┴────────┴──►  /ui/live (SSE)
                                                                                     │
                                                                                     ▼
                                                        LiveProbe UI (server-rendered HTML): flow + sequence
```

Every box is real and running. A single Shopwave checkout becomes one connected trace. LiveProbe
turns it into a per-trace sequence diagram and also adds it to the aggregate topology graph.

## LiveProbe packages

### `internal/core`: the pure engine (no I/O, stdlib only)

The spine. Every function here is pure, unit-tested, and free of network or framework code.

- **`event.go`**: the normalized `Event` type (one event == one span) and the set of
  datastore systems (`DatastoreSystems`). See [event-model.md](event-model.md).
- **`otlp.go`**: `NormalizeOTLP(payload)` flattens OTLP/HTTP JSON into `[]Event`. It maps span
  kind, status and timestamps (nanosecond strings → `int64` micros) and resolves the `peer`. It
  also rebuilds a real endpoint name (`"POST /api/checkout"`) from HTTP attributes when the
  instrumentation only emitted a bare method.
- **`native.go`**: `CoerceEvents` validates already-normalized events (the `/v1/events`
  body).
- **`mask.go`**: PII/secret scrubbing, run on every ingested event. See [pii-masking.md](pii-masking.md).
- **`trace-window.go`**: `TraceWindow` is a rolling in-memory set of recent traces. It is safe
  for concurrent use.
  - `Add(events)` upserts spans into per-trace buckets, in any order.
  - `Assemble(traceID)` builds the span tree (roots = spans with no parent in the window),
    with children sorted by start time.
  - Eviction drops traces older than a horizon (default 5 min) and past a hard cap, so memory
    stays bounded.
  - `Topology()` aggregates the whole window into a service+datastore graph.
- **`sequence.go`**: `SequenceFor(trace)` projects one assembled trace into ordered messages
  between participants. **`sequence-layout.go`**: `LayoutSequence` adds activations and N+1
  groups for the diagram.
- **`summary.go`**: `Summarize(trace)` returns the title, services, span count, duration and
  error flag. `Detail(trace)` returns the summary, sequence, waterfall span rows, critical path
  and Mermaid.
- **`metrics.go`**: per-service RED metrics (`RedMetrics`), alert evaluation (`EvaluateAlerts`)
  and latency buckets. **`flamegraph.go`**: `AggregateFlamegraph` and `CriticalPath`.
- **`export-mermaid.go`**: `ToMermaidSequence` / `ToMermaidFlow` render the models as Mermaid
  text (the snapshot/export path).

### `internal/server` + `cmd/liveprobe`: ingest + serve

- **Ingest** has two routes:
  - `POST /v1/traces` accepts OTLP/HTTP JSON and handles gzip. It runs `NormalizeOTLP`, then
    masking, then `TraceWindow.Add`.
  - `POST /v1/events` accepts native events (`[...]` or `{"events":[...]}`).
  - Bodies are capped at 8 MB, and at 32 MB after decompression.
- **REST (live)**:
  - `GET /api/traces?limit=`: recent trace summaries.
  - `GET /api/traces/:id`: one trace (summary, sequence model, span rows, critical path,
    Mermaid). It falls back to the history store once a trace has aged out of the live window.
  - `GET /api/topology`: the aggregate graph + Mermaid.
  - `GET /api/service/:name`: one service from the live window. Returns inbound/outbound deps,
    error rate, span count, and top operations.
- **REST (history)**:
  - `GET /api/days`: days that have data, with request/error counts.
  - `GET /api/day/:date/summary`: requests, error rate, p50/p95/p99 latency, per-minute
    throughput, top endpoints, and the slowest traces.
  - `GET /api/day/:date/traces?limit=`: that day's trace summaries.
  - `GET /api/day/:date/latency?endpoint=`: p50/p95/p99 per minute for one endpoint.
  - `GET /api/day/:date/flamegraph?endpoint=`: an aggregated flamegraph for one endpoint.
  - `GET /api/errors?limit=`: errored traces grouped by endpoint + error label, each with a
    sample trace.
  - `GET /api/search?q=&service=&attr=&error=&minMs=&maxMs=&minSpans=&sort=&traceId=&limit=`:
    searches all persisted traces. Filters are endpoint substring, service, attribute, error-only,
    latency range and min span count. `sort` picks recent or slowest; `traceId` finds an exact id.
- **WebSocket** (`/ws`, for API clients): on connect the server sends a `snapshot`. After that
  it pushes `traces` deltas (one per ingest) and `topology` deltas (throttled, ~750 ms, carrying
  `red` and `alerts`).
- **UI**: mounts the `internal/ui` handler for every other GET.
- **Alerts**: each topology tick evaluates alerts. When `ALERT_WEBHOOK_URL` is set,
  `webhook.go` POSTs once when an alert starts firing, with timeout, retries and a per-alert
  cooldown.
- `cmd/liveprobe/main.go` reads the environment (`PORT`, `HOST`, `DB_PATH`, `RETENTION_DAYS`,
  `LIVE_WINDOW_MINUTES`, `MAX_TRACES`, `CORS_ORIGINS`, `MASK_PII`, `ALERT_WEBHOOK_*`). The
  default bind is `127.0.0.1` because ingest and API have no auth.

### `internal/history`: persistence

On ingest, each affected trace (summary + detail) is staged and flushed into **SQLite**
(`modernc.org/sqlite`, pure Go, no cgo), partitioned by UTC day. Any `/api` read flushes first,
so reads always see prior writes. The store powers the daily view and lets trace detail survive
eviction from the live window. The schema is the same as the TS server's, so databases it wrote
open unchanged. Old days are pruned at startup and hourly (`RETENTION_DAYS`). The live window
stays in memory for speed; the store is the durable record.

### `internal/ui`: server-rendered dashboard

There is no SPA and no build step. Pages are [templ](https://templ.guide) components rendered
in Go from the same in-process state the API serves. The UI talks to the server through a
`ui.Backend` interface, so it never imports `server`. CSS and JS are embedded with `go:embed`,
so the whole UI ships inside the one binary.

- **Live updates**: live pages subscribe to `GET /ui/live` (Server-Sent Events). Each stream
  starts with a full `reset`, then pushes `traces` (new/updated rows) and `live` (header, RED
  tiles, flow graph or matrix). All of these are **pre-rendered HTML fragments** for that client's
  view. The ~400-line vanilla `static/live.js` applies them in a single `requestAnimationFrame`
  batch. If a client falls behind, its stream is dropped; `EventSource` reconnects and gets a
  fresh reset.
- **Geometry in Go**: flow layout, health colouring, the dependency matrix and every SVG are
  computed server-side (`graph.go`, `trace.go`, `charts.go`). The pure parts live in
  `internal/ui/view`.
- **View state lives in the URL**, so every view is shareable and changing one is a plain
  navigation: `/?service=&view=graph|matrix&metric=calls|errors|latency`,
  `/trace/{id}?tab=sequence&expand=…#spanId`. Only zoom/pan, pause, waterfall collapse and span
  selection happen client-side.
- **Routes**:
  - `/`: the **live page**, with a trace list, RED tiles, and the flow graph ⇄ dependency
    matrix, plus a service filter.
  - `/trace/{id}`: a **dedicated trace page** (deep-linkable). It has a summary and two tabs:
    a **span waterfall** (nested by depth, with self-time, critical path and collapse; click a
    span → its attributes) and the **sequence** diagram. An unknown trace answers 404.
  - `/service/{name}`: a **service page** (also reached by clicking a flow node) showing deps,
    error rate and top operations.
  - `/errors`: the **error explorer**, with errored traces grouped by endpoint + error label.
  - `/alerts`: currently firing alerts, with live "firing for" timers.
  - `/history`: days that have data.
  - `/day/{date}`: the **daily dashboard**. Shows rollup cards (requests, error rate,
    p50/p95/p99), throughput, top endpoints, the slowest traces, and per-endpoint latency,
    histogram, heatmap and flamegraph (`?endpoint=`).
  - `/search`: search all recorded traces. The header also has a search box.
  - `/compare`: two traces side by side.
  - Unknown paths redirect to `/`.
- **Pause**: the live list reorders as traces stream, which makes inspection hard. Pause
  freezes the visible list while new rows keep buffering and an "N new" counter ticks. Resume
  flushes the buffer. The graph keeps updating either way.
- **Flow view**: an SVG of the topology with a stable layout, laid out in Go. It fits to the
  viewport by default and supports wheel-zoom, drag-to-pan and +/−/Fit controls. Zoom survives
  live ticks. Datastore nodes are styled apart, ghost peers are dashed, edge width scales with
  call volume, and nodes/edges are coloured by health. You can export the graph as Mermaid, SVG
  or PNG.
- **Sequence view**: an SVG lifeline diagram for a trace (activations, dashed returns, async
  arrows, collapsible N+1 groups), used on the trace page.

## Message contracts

WebSocket frames on `/ws` (JSON, identical to the TS server):

```
{ type: "snapshot", traces: TraceSummary[], topology, mermaidFlow, red, alerts }
{ type: "traces",   traces: TraceSummary[] }                          // upsert by traceId
{ type: "topology", topology, mermaidFlow, red, alerts }              // replace
```

```
TraceSummary = { traceId, rootOperation, services: string[], spanCount, startTime,
                 durationMicros, hasError }
Topology     = { nodes: string[], edges: Edge[], externals: string[] }
Edge         = { from, to, calls, errors, avgDurationMicros }
TraceDetail  = { summary, sequence, mermaidSequence, spans: SpanRow[], criticalPath: string[] }
Sequence     = { participants: string[], messages: Message[], externals: string[] }
Message      = { from, to, spanId, label, startTime, durationMicros, status, async }
```

Times are integer microseconds. `/ui/live` is the UI's private stream. Its events (`reset`,
`traces`, `live`) carry HTML fragments rather than data and are not a public API.

## The testbed (Shopwave)

Eight Go services plus infrastructure, all in `testbed/docker-compose.yml` (compose project
`shopwave-go`). One distroless image holds them all; each service runs a different binary
from `testbed/cmd/`.

| Component | Role | Stores |
|-----------|------|--------|
| gateway | BFF / API entry, auth check, proxying | — |
| auth | signup/login, JWT, sessions | Postgres, Redis |
| catalog | products | Postgres, Redis (cache) |
| cart | cart state, price checks | Redis |
| order | orders, publishes order.created, saga | Postgres, RabbitMQ |
| payment-worker | consumes order.created, pays | Postgres, RabbitMQ |
| inventory-worker | consumes order.created, reserves stock | Postgres, RabbitMQ |
| notification-worker | consumes results, "sends" mail | Redis, RabbitMQ |
| loadgen | virtual shoppers (traced as `loadgen`) | — |
| postgres / redis / rabbitmq | infrastructure | — |
| otel-collector | receives OTLP, fans out to Jaeger + LiveProbe | — |
| jaeger | reference trace UI | — |

Go has no auto-instrumentation, so tracing is explicit. All of it lives in `testbed/shared`:

- `otelhttp` on every server and client,
- `otelpgx` with `pg.query:<VERB>` span names,
- `redisotel`,
- hand-written AMQP producer/consumer spans that carry W3C trace context in the message headers.

That last piece keeps a checkout one connected trace across every queue hop. See
[shopwave-events.md](shopwave-events.md) for the message payloads and per-action traces, and
[testbed/README.md](../testbed/README.md) for the instrumentation details.

## How LiveProbe connects to the testbed

`testbed/otel/collector-config.yaml` has an `otlphttp/liveprobe` exporter (JSON encoding)
pointing at `http://host.docker.internal:4319`, and the collector service has a
`host.docker.internal:host-gateway` mapping. That lets the collector, running in Docker, reach
the LiveProbe server on the host. The two stacks stay decoupled: LiveProbe runs on the host,
the testbed runs in Docker, and the only link between them is that one OTLP export line.

## Running it

- `./dev-start.sh` builds the binaries and runs the server on http://localhost:4319 with
  synthetic traffic. Flags:
  - `--watch` rebuilds and restarts on every change.
  - `--testbed` brings up the Go Shopwave testbed and its loadgen instead; add `--chaos` for
    the failure scenario.
  - `--ts-testbed` uses the original TS testbed.
  - `--no-load` skips traffic.
  - `--fresh` keeps history in memory.
- `./dev-stop.sh` tears it all down (`--wipe` also deletes local data and testbed volumes).
- `./serve.sh` runs just the server on :4319 for pointing a real app at LiveProbe.
  `./serve.sh --collector` also runs the `algo-instrumentation` collector on :4320
  (`COLLECTOR_PORT`).
- `docker compose up -d` at the repo root runs LiveProbe from a distroless image. Two profiles
  add services: `--profile collector` adds that collector, and `--profile otel` adds an OTel
  Collector that translates OTLP/protobuf to JSON.

See the top-level [README](../README.md) for details and ports.
