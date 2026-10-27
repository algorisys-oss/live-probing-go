# Integrating your app with LiveProbe

This guide shows how to feed **any** running system into LiveProbe so it draws live sequence and
flow diagrams of what your system actually does. The system can be dockerized or not, Node or
polyglot, greenfield or already instrumented.

This is the general guide. For worked, copy-paste references see:

- [shopwave-events.md](shopwave-events.md): the in-repo testbed, a dockerized 8-service Go
  e-commerce app instrumented end to end with the OTel Go SDK (the pattern in "Recipe A" below).
- [`../live-probing/examples/otel-react-go/`](https://github.com/algorisys-oss/live-probing/tree/main/examples/otel-react-go):
  a React frontend + Go backend that shows the "browser direct" and "non-Node service via
  collector" patterns. It lives in the TS repo and was not copied here; it is about *your* app,
  so it applies unchanged.
- [integration-adapters.md](integration-adapters.md): for apps that already emit a
  **custom, non-OTLP** instrumentation format (the native `/v1/events` + collector-adapter path).

## The mental model: one normalized event, two front doors

LiveProbe's engine only ever works with one normalized `Event` (see
[event-model.md](event-model.md)). Both diagram types, the per-trace **sequence** view and the
aggregate **flow/topology** view, are projections of the same `[]Event`. So integration just
means *getting your telemetry into that event stream.* The LiveProbe server (default `:4319`)
has two ingest endpoints:

| Endpoint | Body | Who uses it |
|----------|------|-------------|
| `POST /v1/traces` | **OTLP/JSON** spans | Anything OpenTelemetry-instrumented (the main path) |
| `POST /v1/events` | Pre-normalized `{ "events": [...] }` | Custom formats via the collector adapter; hand-rolled tests |

Both accept plain or gzipped JSON. Server-to-server posts (collectors, SDKs, curl) need no
special handling. For a **browser** posting directly, CORS is reflected only for loopback
origins by default. Allow your app's origin with `CORS_ORIGINS` (see the configuration table in
the [README](../README.md#configuration)).

## The one fact that shapes every integration

**LiveProbe's OTLP ingest reads OTLP/JSON, not protobuf.** The server decodes the request body
as JSON (`core.DecodeOTLP` behind `/v1/traces` in
[internal/server/server.go](../internal/server/server.go)). That single fact decides whether you
need a collector:

```
Emitter that speaks OTLP/JSON  ─────────────────────────────►  LiveProbe :4319/v1/traces   ✅ direct
Emitter that speaks OTLP/protobuf ─► OTel Collector (encoding: json) ─► LiveProbe :4319     (translated)
```

- **Browsers** (OTel-web) and a few SDKs configured for `http/json` emit JSON. They can post
  straight to LiveProbe, no collector needed.
- **Most server SDKs** (Node, Go, Python, Java, …) default to OTLP/**protobuf**. Send them
  through a lightweight **OpenTelemetry Collector** that re-exports with `encoding: json`.
  The collector is also where you batch, retry, and add resource attributes.

> Traces, not logs. LiveProbe draws diagrams from causal span structure (`traceId` /
> `parentSpanId` / `kind` / timing). Use OpenTelemetry's **tracing** signal and set the metrics
> and logs exporters to `none`. A span with `status=error` is your red arrow, and detail rides
> along as span **attributes**.

## Running LiveProbe itself

Every recipe below needs a running LiveProbe server. Pick one:

```bash
./serve.sh                       # build (if missing) + run the server on :4319; --build forces a rebuild
./serve.sh --collector           # + the algo-instrumentation collector on :4320 (COLLECTOR_PORT; Recipe D)

go build -o liveprobe ./cmd/liveprobe && PORT=4319 ./liveprobe   # by hand

docker compose up -d                     # distroless image on :4319, history in a volume
docker compose --profile otel up -d      # + an OTel Collector on :4317/:4318 → LiveProbe (protobuf→JSON)
docker compose --profile collector up -d # + the algo-instrumentation collector on :4320
```

The server binds `127.0.0.1` by default because ingest and the API have no auth. Set
`HOST=0.0.0.0` only on a private network. The compose file sets it for the container.

## Pick your path

| Your app | Path |
|----------|------|
| Node service, want zero code changes | **Recipe A**: `--require` auto-instrumentation + collector |
| Any OTel-SDK service (Go/Python/Java/…), dockerized | **Recipe A**: set OTel env, add a collector service |
| Same, **not** dockerized (bare process / VM / systemd) | **Recipe B**: same env, run one collector next to it |
| Browser / SPA frontend | **Recipe C**: OTel-web, post direct to `:4319` |
| App already emitting a **custom** (non-OTLP) format | **Recipe D**: native `/v1/events` via a collector adapter |
| **VPS deploy, no Docker**: React + Node / Go / Elixir | **Recipe E**: one collector per box (systemd), browser through it |

Whatever the language, once spans reach the wire the language is irrelevant, because LiveProbe
only sees OTLP/JSON. Polyglot systems do **not** need one integration per language.

---

## Recipe A: Dockerized app via OpenTelemetry (the Shopwave pattern)

This is exactly how the repo's own testbed works. There are two moving parts: OTel env on each
service, and one collector container that fans spans out to LiveProbe.

**1. Instrument each service via env.** For Node, zero-code auto-instrumentation with a
`--require` preload covers `http`/`fetch`, `pg`, `ioredis`, `amqplib`, etc. Define the shared
OTel env once and give each service only its own name (a YAML anchor keeps it DRY):

```yaml
# docker-compose.yml
x-otel-env: &otel-env
  NODE_OPTIONS: --require @opentelemetry/auto-instrumentations-node/register
  OTEL_EXPORTER_OTLP_ENDPOINT: http://otel-collector:4318   # OTLP/HTTP into the collector
  OTEL_EXPORTER_OTLP_PROTOCOL: http/protobuf
  OTEL_TRACES_EXPORTER: otlp
  OTEL_METRICS_EXPORTER: none
  OTEL_LOGS_EXPORTER: none

services:
  orders:
    environment:
      <<: *otel-env
      OTEL_SERVICE_NAME: orders     # ← the ONE per-service line; becomes the diagram's node/lifeline label
```

`OTEL_SERVICE_NAME` is stamped onto every span as the `service.name` resource attribute.
LiveProbe maps that to `Event.participant`, which is the box in the flow view and the lifeline
in the sequence view. Rename it and the diagram renames.

For non-Node services, drop the `NODE_OPTIONS` line and instrument with that language's OTel
SDK, but keep the same `OTEL_EXPORTER_OTLP_ENDPOINT` / `OTEL_SERVICE_NAME` env. For Go, the
testbed's [`testbed/shared/`](../testbed/shared/) is a working reference:

- `otel.go`: the SDK setup, an OTLP/HTTP exporter, and W3C propagation.
- `http.go`: `otelhttp` servers and clients with `http.route` stamped.
- `stores.go`: `otelpgx` and `redisotel`.
- `amqp.go`: RabbitMQ producer/consumer spans by hand.

**2. Add a collector that re-exports JSON to LiveProbe on the host:**

```yaml
  otel-collector:
    image: otel/opentelemetry-collector:0.115.1
    command: ["--config=/etc/otel/config.yaml"]
    volumes:
      - ./otel/collector-config.yaml:/etc/otel/config.yaml:ro
    extra_hosts:
      - "host.docker.internal:host-gateway"   # lets the container reach LiveProbe on the host
```

```yaml
# otel/collector-config.yaml
receivers:
  otlp:
    protocols: { grpc: { endpoint: 0.0.0.0:4317 }, http: { endpoint: 0.0.0.0:4318 } }
processors:
  batch: { timeout: 1s }
exporters:
  otlphttp/liveprobe:
    endpoint: http://host.docker.internal:4319   # LiveProbe's OTLP/JSON ingest
    encoding: json                               # ← the protobuf→JSON translation
    tls: { insecure: true }
service:
  pipelines:
    traces: { receivers: [otlp], processors: [batch], exporters: [otlphttp/liveprobe] }
```

`endpoint: http://host.docker.internal:4319` is correct because the OTLP/HTTP exporter appends
`/v1/traces` itself. Add `jaeger`/`debug` exporters alongside if you want a second sink.

Start LiveProbe on the host (`HOST=0.0.0.0 ./serve.sh`: server only, no testbed; the bind
address matters, see the gotchas), bring the stack up, and drive traffic. The diagram redraws live. If you don't want to run a collector of your own,
`docker compose --profile otel up -d` in this repo runs LiveProbe together with a pre-configured
collector on `:4317`/`:4318`; point your services' `OTEL_EXPORTER_OTLP_ENDPOINT` at that.

## Recipe B: Non-dockerized app (bare process, VM, systemd, k8s pod)

Nothing here needs Docker. There are two options:

**B1: Run one collector next to the app (recommended; Recipe A without compose).**
Install the collector binary (`otelcol`) and point it at LiveProbe:

```bash
# 1. LiveProbe running somewhere reachable (host, another VM, a cluster service)
# 2. Collector: same collector-config.yaml as above, but the endpoint is LiveProbe's real address:
#    exporters.otlphttp/liveprobe.endpoint: http://<liveprobe-host>:4319
otelcol --config ./collector-config.yaml       # listens OTLP on :4317/:4318

# 3. Your app: export env before launching (works for a plain process or a systemd unit)
export OTEL_SERVICE_NAME=orders
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
export OTEL_TRACES_EXPORTER=otlp OTEL_METRICS_EXPORTER=none OTEL_LOGS_EXPORTER=none
export NODE_OPTIONS="--require @opentelemetry/auto-instrumentations-node/register"  # Node only
node ./server.js
```

In a systemd unit these go in `[Service]` as `Environment=` lines. In k8s they're container
`env:` entries, and the collector is a sidecar or a DaemonSet.

**B2: Skip the collector (only if your SDK can emit OTLP/JSON).** Some SDKs support
`OTEL_EXPORTER_OTLP_PROTOCOL=http/json`. If yours does, point the app straight at LiveProbe
and drop the collector entirely:

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT=http://<liveprobe-host>:4319
export OTEL_EXPORTER_OTLP_PROTOCOL=http/json      # ← must be JSON; protobuf won't parse
```

This is the leanest path for a single service. Most server SDKs default to protobuf, though, so
**B1 is the safe default.** The browser is the one emitter that always speaks JSON (see
Recipe C).

## Recipe C: Browser / SPA frontend (direct, no collector)

The OTel-web OTLP/HTTP exporter emits JSON, so the browser can post straight to LiveProbe. Point
its exporter at `http://<liveprobe-host>:4319/v1/traces`, instrument fetch/XHR, and add your
app's origin to `CORS_ORIGINS`. To stitch frontend spans and backend spans into **one** trace,
enable W3C context propagation so the `traceparent` header rides the API call:
`propagateTraceHeaderCorsUrls` on the web side, and the `TraceContext` propagator on the server.
Full snippet:
[../live-probing/examples/otel-react-go/react-frontend/tracing.ts](https://github.com/algorisys-oss/live-probing/blob/main/examples/otel-react-go/react-frontend/tracing.ts).

## Recipe D: App already emitting a custom (non-OTLP) format

Some apps emit a proprietary structured format over RabbitMQ, stdout, or HTTP rather than OTLP.
For these, use the native path. The repo's **collector** (`cmd/liveprobe-collector`, code in
`internal/collector`) consumes from a source, runs a per-format **adapter**
(`raw → []Event`), and POSTs the result to `/v1/events`. One adapter covers all of a client's
languages. Design and config are in [integration-adapters.md](integration-adapters.md). That doc
includes a step-by-step
[non-intrusive runbook](integration-adapters.md#rolling-out-to-an-existing-production-estate-non-intrusive-runbook)
for adding LiveProbe to an existing production estate that already emits to a shared broker,
without touching the apps.

In the HTTP-source case, apps POST the format straight to the collector.
`./serve.sh --collector` (or `docker compose --profile collector up -d`) runs the server and the
`algo-instrumentation` collector together. The collector listens on `:4320` (env `COLLECTOR_PORT`) and
forwards to the server, so point your `/v1/event/*` emitter at it.

The OTLP and native/collector streams don't merge because they use different trace-id spaces.
A request instrumented both ways shows up as two traces, so pick one instrumentation path per
request to avoid duplicates.

## Recipe E: VPS deploy, no Docker (React + Node / Go / Elixir)

This is the common "app on a DigitalOcean droplet, no Docker, no k8s" case. It is Recipe B
applied to a polyglot stack, with one addition: **route the browser through the collector too**,
so LiveProbe never has to be exposed to the internet.

### The shape

Run **one OpenTelemetry Collector per VPS as a systemd service**. Every backend on the box
exports to it locally, and the React frontend posts to it over the network. The collector
translates protobuf→JSON and forwards to LiveProbe, which stays private.

```
React (browser, OTel-web, JSON) ─────────────────┐
                                                  ▼
Node / Go / Elixir  ──OTLP/protobuf──►  otel Collector (systemd, :4318, TLS+CORS)
  (localhost:4318)                                │  encoding: json
                                                  ▼
                                            LiveProbe :4319   (private / localhost)
```

Why go through the collector instead of straight to LiveProbe (Recipe C)? On a VPS the browser
needs a **public** endpoint, and **LiveProbe's ingest has no auth**, so exposing it means open
ingest. The collector's OTLP/HTTP receiver also accepts JSON. Pointing React at it gives you a
single internet-facing door you can put TLS and a CORS allow-list on, while LiveProbe stays
bound to localhost. **Go and Elixir have no choice anyway**: their OTLP exporters speak only gRPC
or HTTP/**protobuf**, never JSON, so they need a translating collector regardless.

LiveProbe itself is one static binary here: copy `liveprobe` to the box and run it under systemd
with `PORT=4319` and a `DB_PATH` on persistent disk. It needs no other runtime.

### Backends

Each backend exports to the local collector at `http://localhost:4318`.

**Node.js**: zero-code auto-instrumentation. Put the env in the systemd unit's `[Service]`:

```ini
Environment=OTEL_SERVICE_NAME=orders-api
Environment=OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
Environment=OTEL_TRACES_EXPORTER=otlp
Environment=OTEL_METRICS_EXPORTER=none
Environment=OTEL_LOGS_EXPORTER=none
Environment=NODE_OPTIONS=--require @opentelemetry/auto-instrumentations-node/register
```

**Go**: the OTel Go SDK with `otelhttp` (server) + `otelpgx` (DB), exporting to `localhost:4318`.
There are two references:

- [../live-probing/examples/otel-react-go/go-backend/](https://github.com/algorisys-oss/live-probing/blob/main/examples/otel-react-go/go-backend/telemetry.go)
  (`telemetry.go` + `main.go`) is exactly this case.
- [`testbed/shared/`](../testbed/shared/) also covers Redis and RabbitMQ.

Recent `otelpgx` versions emit `db.system.name` rather than `db.system`. LiveProbe reads both
(see [event-model.md](event-model.md#peer-resolution-outbound-spans-only)), so Postgres renders
as a `postgresql` datastore node.

**Elixir / Phoenix**: add the OTel libs and turn on the auto-instrumentation:

```elixir
# mix.exs deps
{:opentelemetry, "~> 1.5"}, {:opentelemetry_api, "~> 1.4"},
{:opentelemetry_exporter, "~> 1.8"},
{:opentelemetry_phoenix, "~> 2.0"}, {:opentelemetry_ecto, "~> 1.2"},
{:opentelemetry_bandit, "~> 0.2"}   # or {:opentelemetry_cowboy, "~> 1.0"} if not on Bandit

# application.ex — in start/2, before the supervisor children
OpentelemetryPhoenix.setup(adapter: :bandit)
OpentelemetryEcto.setup([:my_app, :repo])
OpentelemetryBandit.setup()

# config/runtime.exs
config :opentelemetry, :resource, service: %{name: "my-phoenix-app"}
config :opentelemetry, traces_exporter: :otlp
config :opentelemetry_exporter,
  otlp_protocol: :http_protobuf,
  otlp_endpoint: "http://localhost:4318"
```

Elixir defaults to the W3C `tracecontext` propagator, so it stitches to the React `traceparent`
automatically, with no extra config.

### Database tracing (and its intrusion floor)

A DB span is one span per query, nested under the request, with timing and the service that ran
it. To produce it you need the **active trace context** (`traceId`/`spanId`/`parentSpanId`), and
that context only exists **inside the process making the query**. Postgres emits no
OpenTelemetry spans and has no idea which trace a query belongs to. So "tracing the DB" always
means instrumenting the DB *client* (`pg`, Ecto, pgx) in the app. **Postgres then renders in
LiveProbe as a datastore *peer* node**, derived from the `db.system` / `db.system.name`
attribute on the client span, not as an instrumented participant. Nothing is installed on the
database and no `traceparent` travels into it.

That puts a floor on "without any intrusion": something in or around the app process must
capture the query. How much it touches the app varies by tier (least intrusive first):

| Tier | Node (pg + ORM) | Elixir/Phoenix (Ecto) | Touches |
|---|---|---|---|
| **1. Preload agent** | ✅ `NODE_OPTIONS=--require @opentelemetry/auto-instrumentations-node/register` | ❌ no BEAM equivalent | start command only |
| **2. In-process SDK** | (same as tier 1) | ✅ dep + `OpentelemetryEcto.setup(...)` | a few lines at boot |
| **3. eBPF agent** | ✅ | ✅ | host only, zero app change |

- **Node is effectively non-intrusive today.** The `--require` preload (already set above)
  monkey-patches `pg` at load time. That gives a span per query nested under the request with
  no source changes; you only change *how the process starts*. Because it instruments the
  underlying `pg` driver, most ORMs (Prisma, Sequelize, TypeORM, Knex) come along for free.
- **Elixir needs a minimal but nonzero change.** The BEAM has no preload/agent injection, so DB
  tracing here cannot be truly zero-code. Add `{:opentelemetry_ecto, …}` and the one
  `OpentelemetryEcto.setup([:my_app, :repo])` call from the Elixir backend block above. The
  footprint is tiny: Ecto already emits `:telemetry` events, and this just *attaches a handler*
  to events that already fire.
- **eBPF is genuinely zero app change, on both stacks.** An eBPF agent on the host (Grafana
  Beyla or the OpenTelemetry eBPF profiler) captures DB client calls at the syscall level, with
  no code and no preload. It exports OTLP → the collector → LiveProbe. The trade-offs: it needs
  a recent kernel and root/`CAP_BPF`, and its span quality and cross-service context propagation
  are lower fidelity than in-process SDK instrumentation. Reach for it when you truly cannot
  touch the app or its launch.

**What does *not* work for LiveProbe:** Postgres-side visibility (`log_statement`,
`pg_stat_statements`, `auto_explain`, a `pgbouncer`/wire proxy) yields queries and durations but
**no trace/span ids**. Without them, the queries can't be nested into a trace or drawn in the
sequence/flow views. Bridging that gap needs SQL-comment context injection (à la
*sqlcommenter*), which is itself an app change.

> The same floor applies on the **adapter path** (a custom format over a broker, Recipe D), from
> the other side. Postgres appears only if the app's own instrumentation already emits a DB event
> carrying a `db.system`/`peer` field. `adapter-id-1` currently drops `peer` and `duration`, so
> DB nodes and query latency don't render there yet. See
> [integration-adapters.md](integration-adapters.md).

### React (same for all three stacks)

Use OTel-web, but point the exporter at the **collector's public URL** (not at LiveProbe). Also
propagate `traceparent` to your API origin so the frontend and backend become one trace:

```ts
// OTLPTraceExporter url: https://<vps-collector-host>/v1/traces   (JSON; collector receives it)
// FetchInstrumentation propagateTraceHeaderCorsUrls: [/https:\/\/api\.myapp\.com/]
```

Full snippet: [../live-probing/examples/otel-react-go/react-frontend/tracing.ts](https://github.com/algorisys-oss/live-probing/blob/main/examples/otel-react-go/react-frontend/tracing.ts).

### The collector (systemd, no Docker)

Install the `otelcol` binary, drop in a config, and run it as a unit:

```yaml
# /etc/otelcol/config.yaml
receivers:
  otlp:
    protocols:
      grpc: { endpoint: 0.0.0.0:4317 }
      http:
        endpoint: 0.0.0.0:4318
        cors: { allowed_origins: ["https://myapp.com"] }   # so the browser can post
processors:
  batch: { timeout: 1s }
exporters:
  otlphttp/liveprobe:
    endpoint: http://<liveprobe-host>:4319   # localhost if LiveProbe is on the same box
    encoding: json
    tls: { insecure: true }
service:
  pipelines:
    traces: { receivers: [otlp], processors: [batch], exporters: [otlphttp/liveprobe] }
```

```ini
# /etc/systemd/system/otelcol.service
[Service]
ExecStart=/usr/bin/otelcol --config /etc/otelcol/config.yaml
Restart=always
```

### Two decisions to make up front

- **Public exposure.** Put the collector's `:4318` behind TLS (a Caddy/nginx reverse proxy) and
  keep the CORS allow-list tight, because it's your one internet-facing telemetry port. Keep
  LiveProbe's `:4319` on localhost or a private network, never public.
- **One collector or many.** For a handful of droplets, one collector per box (above) is the
  clean default, because a bad box can't affect the others. A single central collector that all
  boxes ship to also works; each box's exporter just points at its address instead of
  `localhost`.

---

## Field mapping (what shows up in the diagram)

LiveProbe's `NormalizeOTLP` derives the normalized `Event` from OTLP spans
([internal/core/otlp.go](../internal/core/otlp.go)):

| Diagram element | Comes from |
|-----------------|-----------|
| Participant (node / lifeline) | `service.name` resource attribute (= `OTEL_SERVICE_NAME`) |
| Peer (the other side of an edge) | `db.system` / `db.system.name` / `messaging.system`, else `peer.service` / `server.address` / … on client spans |
| Operation (arrow / span label) | span name (route, method, query); bare-method HTTP spans become `METHOD route` |
| Arrow direction | span `kind` (`client`/`server`/`producer`/`consumer`/`internal`); OTel is the oracle |
| Red / error edge | span `status = error` |
| Timing | span start + duration (microseconds) |

Practical consequences:

- Set a clear `OTEL_SERVICE_NAME` per service.
- Keep standard OTel semantic attributes (`db.system` or `db.system.name`, `messaging.system`,
  `http.route`), so peers and infra (Postgres, Redis, RabbitMQ) render correctly without being
  instrumented themselves.
- Propagate `traceparent` across every hop, including message-queue headers, or traces
  fragment.

## Verify the pipe before touching any SDK

Prove that LiveProbe is reachable and drawing by sending a hand-rolled native event. This
bypasses OTel entirely; `startTime` and `duration` are epoch **microseconds**:

```bash
curl -X POST http://localhost:4319/v1/events -H 'content-type: application/json' -d '{
  "events": [
    {"traceId":"t1","spanId":"a","participant":"web","peer":"orders-api","operation":"GET /orders","kind":"client","startTime":1700000000000000,"duration":8000,"status":"ok","attributes":{}},
    {"traceId":"t1","spanId":"b","parentSpanId":"a","participant":"orders-api","operation":"GET /orders","kind":"server","startTime":1700000000001000,"duration":6000,"status":"ok","attributes":{}}
  ]
}'
```

The response is `{"accepted":2}`. Open the LiveProbe UI and you should see a `web → orders-api`
trace. The live window's clock is the newest start time it has seen, so this fixed 2023
timestamp only stays live on an idle server. If other traffic is flowing, the trace is evicted
at once (it is still in History, under 2023-11-14). Use the current time (`$(date +%s%6N)`, as
in the [README](../README.md#quick-start) quick start) to keep it on the Live page. Then wire your real SDK via the recipe above
and watch real traffic replace the fixture.

## Common gotchas

- **Spans arrive at Jaeger but not LiveProbe**: the collector exporter is missing
  `encoding: json`, or is sending protobuf. LiveProbe only parses JSON.
- **Nothing arrives from a container**: the collector can't reach the host. Add the
  `host.docker.internal:host-gateway` `extra_hosts` entry (or use LiveProbe's real address).
  Also check the bind address. `host.docker.internal` reaches the host over the Docker bridge,
  not over loopback, so a LiveProbe on the default `HOST=127.0.0.1` is unreachable from
  containers. Run it with `HOST=0.0.0.0` on a trusted network (`./dev-start.sh --testbed` does
  this for you).
- **Frontend and backend show as two separate traces**: context propagation isn't wired. The
  `traceparent` header (and the CORS allow-list for it) must be enabled on both sides.
- **Node spans missing for `amqplib` / `ioredis`**: load those libs via CJS `require`, not
  ESM `import`. OTel's Node instrumentation only hooks the require path (the TS testbed's
  [`amqp.ts`](https://github.com/algorisys-oss/live-probing/blob/main/testbed/packages/shared/src/amqp.ts) shows the workaround).
- **Go spans missing for RabbitMQ / other libraries**: Go has no auto-instrumentation, so
  every library needs an explicit instrumentation package. AMQP has no maintained one at all.
  [`testbed/shared/amqp.go`](../testbed/shared/amqp.go) shows hand-written producer/consumer
  spans that carry the trace through message headers.
- **Postgres shows as a dashed ghost `postgres` host instead of a `postgresql` datastore**:
  your SDK emits neither `db.system` nor `db.system.name`, so the peer fell through to
  `server.address`. Upgrade the instrumentation, or set the attribute yourself.
- **Everything is one giant `internal` blob**: you're emitting the wrong signal or missing
  `kind`. Make sure you export **traces** with proper client/server span kinds, not logs.
