# Integrating LiveProbe with client apps (adapters)

**Status: built and verified.** The pieces are:

- native `/v1/events` ingest
- a collector with stdin / rabbitmq / http sources
- the `adapter-id-1` and `algo-instrumentation` adapters

All of it was ported from TS. Under a 20k-input seeded fuzz, the adapters produce output
identical to the TS adapters (`tools/parity/`). See "Running the collector" below.

## Problem

LiveProbe runs on the client side. Client apps are polyglot (Node, Ruby on Rails,
Elixir/Phoenix, Go, Python) and emit instrumentation in a **client-specific structured format**
(not OTLP). The first client's format spec is [adapters/adapter-id-1.md](adapters/adapter-id-1.md).
Different clients use different formats **and** different transports: client-1 emits to
**RabbitMQ**, another client emits to **stdout**. We need one way to bring all of them into
LiveProbe.

## Principle: the normalized Event is the narrow waist

LiveProbe's engine only ever works with the normalized `Event` model
(`internal/core/event.go`). Trace assembly, sequence diagrams, topology, history, and search are
all projections of `[]Event`. The OTLP ingest (`core.NormalizeOTLP`) is just the first adapter:
`OTLP JSON → []Event`.

So integration reduces to this: **for each input format, write `raw → []Event`.** The core never
changes. Many input formats feed one `Event` model, which feeds many views (an hourglass).

## Two orthogonal concerns

- **Transport (source)**: *where* events arrive (RabbitMQ, stdout, HTTP).
- **Format (adapter)**: *how* to map them (`adapter-id-1`, `algo-instrumentation`, …).

Any source pairs with any adapter. **Polyglot does not multiply adapters**: a client's five
languages all emit the *same* structured format, so one adapter covers all of them. The
language is irrelevant once events reach the transport.

## The collector

```
 SOURCES (transport)                 ADAPTERS (per client format)          SINK
 ──────────────────                  ────────────────────────────          ────
 rabbitmq  ─ consume queue ─┐
 stdin     ─ ndjson lines  ─┼─ raw event ─► adapter(raw) => []Event ─► POST /v1/events ─► LiveProbe
 http      ─ receive POSTs ─┘         (adapter-id-1, algo-instrumentation…)  └► window/SQLite/UI
```

`cmd/liveprobe-collector` is configured entirely by environment. Each process runs **one
source + one adapter**:

| Variable | Default | Meaning |
| --- | --- | --- |
| `SOURCE` | `stdin` | `stdin`, `http` or `rabbitmq` |
| `ADAPTER` | `adapter-id-1` | `adapter-id-1`, `algo-instrumentation`, `liveprobe-native` |
| `LIVEPROBE_URL` | `http://localhost:4319` | where batches are POSTed (`/v1/events`) |
| `HTTP_PORT` | `4320` | `http` source port |
| `RABBITMQ_URL` / `QUEUE` | `amqp://localhost:5672` / `instrumentation.events` | broker and queue |
| `EXCHANGE` / `EXCHANGE_TYPE` / `ROUTING_KEY` | — / — / `#` | tap an exchange (see the runbook) |
| `QUEUE_MAX_LENGTH` / `QUEUE_MESSAGE_TTL_MS` | — | bound the tap queue |

Logically, wiring two clients looks like this:

```yaml
sources:
  - id: client-1            # emits to RabbitMQ
    type: rabbitmq          # SOURCE=rabbitmq RABBITMQ_URL=amqp://… QUEUE=instrumentation.events
    adapter: adapter-id-1   # ADAPTER=adapter-id-1

  - id: client-2            # emits to stdout
    type: stdin             # newline-delimited JSON on stdin / a tailed file / a log-shipper POST
    adapter: algo-instrumentation
```

Each entry is one collector process with those env vars. There is no config-file loader, in
this port or in the TS one.

Source notes:

- **rabbitmq**: asserts a durable queue and consumes it with prefetch 64. Each message is acked
  once the adapter has run and its events are buffered for send. Delivery is **best-effort, not
  durable** (see the delivery semantics under the runbook below).
  - Set `EXCHANGE` to bind the queue to an existing exchange for a **non-intrusive fanout tap**.
    `EXCHANGE_TYPE` declares the exchange; without it, the exchange is only verified passively.
    `ROUTING_KEY` sets the binding key(s), comma-separated, default `#`.
  - Optionally bound the queue with `QUEUE_MAX_LENGTH` (drops the oldest past the cap) and/or
    `QUEUE_MESSAGE_TTL_MS`, so a live tap can't back up unboundedly while the collector is down.
  - Implementation: `internal/collector/rabbitmq.go` (`rabbitmq/amqp091-go`).
- **stdin** ("stdout" from the app's point of view): newline-delimited JSON from stdin or a
  tailed file. In container/k8s land, apps write JSON lines to stdout. A log shipper
  (fluentbit/vector) can POST them to the collector's `http` source, or the collector can read
  the file directly. The adapter is the same either way.
- **http**: a plain receiver for log shippers and direct pushers.
  - It accepts POSTed JSON (one raw event or an array) on **any path**. Emitters that push to
    per-type endpoints (e.g. `POST /v1/event/instrumentation` / `/v1/event/log` /
    `/v1/event/audit`) can therefore be repointed at it unchanged. The event type must live in
    the payload, not the URL.
  - `HTTP_PORT` (default 4320), `GET /healthz`, 5 MB body cap.
  - Implementation: `internal/collector/sources.go`.

## PII and sensitive data (mask in the adapter)

The adapter is the **single chokepoint** every client's raw data passes through before it is
buffered, POSTed to `/v1/events`, persisted to SQLite, and shown in the UI. That makes it the
place to **mask PII and secrets**: do it in `raw → []Event`, before the event leaves the adapter.

- **`attributes` is the highest-risk field.** It holds free-form key/values and is often the
  widest copy of the source payload (e.g. audit `before`/`after` flattening, per-type payload
  fields). Prefer an **allow-list** of known-safe keys over copying everything and denying a few.
  When a value may hold PII (email, phone, name, government id, card/PAN, token, password, auth
  header), **redact, hash, or drop it**. Don't forward it raw.
- **Watch the other free-text fields too.** `operation` can leak identifiers (routes and messages
  can embed ids or emails, e.g. `GET /users/jane@acme.com`), and so can `peer` and
  `participant`.
- **When unsure, drop it.** LiveProbe is a live-diagnostics view, not a system of record. Losing
  an attribute costs nothing; leaking one into stored/rendered traces is a data-handling incident.
- Keep the masking rules **in the adapter (or a shared helper)** so every source/transport that
  uses that adapter inherits them, and cover them in the adapter's tests
  (`internal/collector/adapters_test.go`).
- A shared backstop ships in core: **`core.MaskPII(event, opts)`** (and `MaskEvents`,
  `MaskString`) in `internal/core/mask.go`. It redacts sensitive attribute keys by name and
  scrubs embedded emails, bearer tokens and Luhn-valid card numbers from string values and the
  identity fields. The server already runs it on **every** ingest (`/v1/events` included)
  unless `MASK_PII=off`, so collector output is always masked once. An adapter that needs a
  custom placeholder or extra `RedactKeys` can call `core.MaskEvents` itself as well. Full
  reference (how the two layers work, patterns, limitations): [pii-masking.md](pii-masking.md).

## Deployment modes

1. **Sidecar per client (recommended default for isolation).** Run one collector process per
   client, with one source + one adapter, deployed next to (or as a sidecar of) that client's
   system. This gives you:
   - fault and resource isolation (one client's spike or bad message can't affect another),
   - independent scaling and upgrades,
   - per-client credentials and config kept separate,
   - a blast radius of one client.

   A client's config is a handful of env vars, and its collector can be deployed or rolled back
   on its own. The collector is a single static binary, so it needs no runtime on the host.
2. **Several clients on one box.** Run several collector processes side by side, one per
   source. A single process reading several sources at once is *not* implemented (it wasn't in
   TS either), and it would couple the clients' fate anyway.

Start client onboarding with a sidecar.

## adapter-id-1 field mapping (their format → `Event`)

| LiveProbe `Event` | ← adapter-id-1 field |
|---|---|
| `traceId` / `spanId` / `parentSpanId` | `trace_id` / `span_id` / `parent_span_id` |
| `participant` | `service_category` (or `component`) |
| `operation` | `service:event_type` (their `span_name` = sc+component+service) |
| `startTime` | `span_start_timestamp` / `timestamp` (→ micros) |
| `status` | `level`: ERROR/CRITICAL → `error`, else `ok`/`unset` |
| `attributes` | `details` + `event_value`, `correlation_id`, `reference_id`, `reference_type`, `ou_id`, `event_source` |
| `kind` | *(not in their format)* → default `internal` |
| `peer` / `duration` | *(not present)* → optional (see gaps) |

**Works immediately** with the mapping above: trace assembly, **sequence diagrams**, **service
topology**, history, and search. LiveProbe derives service→service edges from parent/child
`participant` transitions, so it does not need OTel span kinds.

**Gaps to decide later:**
- **Latency/duration**: their format is event-centric, so a per-span duration needs a span-end
  event. Their `event_value` sometimes carries a TAT we could use.
- **Datastore nodes**: these need a `peer` / `db.system` convention. Without it, datastores
  don't appear as nodes (service→service topology still works).
- **Span kind**: defaults to `internal`. It could be inferred from their context parser
  (http / rabbitmq / socket) if we want producer/consumer arrows.

## algo-instrumentation field mapping (instrumentation-service format → `Event`)

This is the format of an internal instrumentation service whose apps POST three JSON event
types: `instrumentation`, `log` and `audit`. Events arrive either singly or wrapped in a
`{ events: [...] }` batch envelope, sent to `POST /v1/event/<type>` (the http source ignores the
path). Each event carries `application { name, module, environment }` and
`request { requestId }`. Older emitters send **no span ids**, so for them the adapter
synthesizes trace structure. Newer ones send `request.traceId` / `request.spanId` and a `spans[]`
array, which the adapter prefers. Spec and samples are in
[adapters/algo-instrumentation.md](adapters/algo-instrumentation.md).

| LiveProbe `Event` | ← algo-instrumentation field |
|---|---|
| — (envelope) | `{ events: [...] }` is unwrapped; each element maps independently |
| `traceId` | `request.traceId`, else `request.requestId`, else `eventId`. All three event types of one request share a trace |
| `spanId` | `request.spanId`, else `eventId` |
| `parentSpanId` | `request.requestId` → a **synthetic `client` root span** (spanId = requestId; every event re-emits it, and the window dedupes) |
| `participant` | `application.name`.`application.module` (e.g. `hrms.employee`). **App + module are the lifeline** |
| `operation` | instrumentation: `METHOD endpoint` · log: `log LEVEL: message` · audit: `audit ACTION entityType entityId` |
| `kind` | instrumentation → `server`; log/audit → `internal`; the synthetic root → `client`; `spans[]` children by their `kind` (db/redis/cache/http → `client`, function → `internal`) |
| `startTime` | `timestamp` (ISO-8601 → micros) |
| `duration` | instrumentation: `payload.durationMs` → micros; others 0 |
| `status` | instrumentation: `success === false` or `statusCode`/`status_code` `>= 400` → `error`; log: level ERROR/FATAL/CRITICAL → `error` |
| `attributes` | `application`, `module`, `service`, `version`, `environment`, `eventType`, top-level `severity`/`message`, `tags.*` + per-type payload fields (instrumentation adds `status_code`, `memory_usage_mb`; audit `before`/`after` flattened to `before.x`/`after.x`, `changedFields` joined) |

**Works immediately**:

- sequence diagrams with `app.module` lifelines (endpoint calls with latency + status, error
  logs in red, audits as labeled arrows),
- flow topology and history,
- attribute search (`module=invoice`, `application=hrms`, `environment=prod`).

**Limitation for emitters without `spans[]`:** these events have no causality between them
(no parent span ids). Every arrow therefore originates at the synthetic `client` lifeline, and
service→service call nesting (`propeak → hrms`) cannot be reconstructed. Closing that gap
requires the emitting library to propagate a parent/span id. Retries within one request stay
distinct (`spanId = eventId`).

## What was built

- [x] **`POST /v1/events`**: native ingest on the server, accepting `{ events: Event[] }` (the
  target all adapters produce). It shares the ingest path with OTLP via `core.CoerceEvents`.
- [x] **A collector** (`internal/collector`, `cmd/liveprobe-collector`):
  - sources `stdin` (ndjson), `rabbitmq` and `http` (path-agnostic JSON receiver),
  - an adapter registry (`GetAdapter`),
  - a batching sink that POSTs to `/v1/events` (batches of 50 or every second).
- [x] **`adapter-id-1`**: a reference `func(raw any) []core.Event` mapping for the
  structured-event format (field mapping above). One client event → one span.
- [x] **`algo-instrumentation`**: the instrumentation-service mapping above.
- [x] **Verified**:
  - The TS adapter and source test suites are ported.
  - A real-broker integration test (opt-in, `LIVEPROBE_AMQP_URL`) covers declare/bind/bounded
    queue args, poison drops, and reconnect after a broker-forced disconnect.
  - The adapters match TS under 20k fuzzed inputs.

A new client after this is **a new adapter function + one registry entry**. The transport is
just a source.

## Running the collector

```bash
go build -o liveprobe-collector ./cmd/liveprobe-collector

# ndjson on stdin (one client event per line)
cat events.ndjson | SOURCE=stdin ADAPTER=adapter-id-1 LIVEPROBE_URL=http://localhost:4319 \
  ./liveprobe-collector

# consume a RabbitMQ queue directly (one collector per client; run as a sidecar)
SOURCE=rabbitmq ADAPTER=adapter-id-1 RABBITMQ_URL=amqp://… QUEUE=instrumentation.events \
  LIVEPROBE_URL=http://localhost:4319 ./liveprobe-collector

# non-intrusive fanout tap: bind a dedicated queue to an existing exchange (see the runbook)
SOURCE=rabbitmq ADAPTER=adapter-id-1 RABBITMQ_URL=amqp://… \
  EXCHANGE=instrumentation ROUTING_KEY="#" QUEUE=liveprobe.tap \
  LIVEPROBE_URL=http://localhost:4319 ./liveprobe-collector

# http receiver: point emitters that POST JSON events (e.g. /v1/event/instrumentation) here
SOURCE=http ADAPTER=algo-instrumentation HTTP_PORT=4320 \
  LIVEPROBE_URL=http://localhost:4319 ./liveprobe-collector
```

The last one is also what `./serve.sh --collector` and `docker compose --profile collector`
run next to the server.

Adapters live in `internal/collector/adapters.go` (`adapter-id-1` and `algo-instrumentation`,
plus the `liveprobe-native` passthrough). Per-client format specs live in
[`docs/adapters/`](adapters/).

## Rolling out to an existing production estate (non-intrusive runbook)

This is the common real-world case. A client runs **many apps** (say 10):

- across **mixed environments** (vanilla VPS, GCP, …),
- in **several languages** (React + Node + Postgres via an ORM, Elixir/Phoenix + Ecto, …),
- and they **already emit** `adapter-id-1`-format instrumentation through an internal library.

That library publishes to a **shared RabbitMQ**. From there the stream is written to stdout and
fanned out to the client's own aggregators for storage and analysis.

This is the easiest possible integration, because the client already did the hard part
(normalizing every app into one format on one transport). The whole job is to add a *reader*.

### The one principle: tap the stream, don't divert it

You do **not** touch the 10 apps, and you do **not** take over the existing consumers. You add
a parallel reader that gets a **copy** of the same stream and forwards it to LiveProbe. The
client's existing storage/analysis pipeline keeps working, unchanged and unaware.

### You deploy ONE collector for the client, not one per app

Every app already funnels into a single normalized stream, so:

- **Language / framework / ORM is irrelevant.** React, Node, Elixir, Ecto, Postgres: by the
  time data reaches RabbitMQ it is uniform `adapter-id-1` JSON. LiveProbe never sees app code.
- **Hosting diversity is irrelevant.** VPS, GCP or anything else, they all publish to the one
  shared RabbitMQ. The integration point is that single broker, not the 10 apps.

So you need **1 collector instance + 1 LiveProbe server per client**, regardless of app count,
language mix, or where the apps run. (Run one collector *per client*, not per app; see
Deployment modes.)

### Step 1: pick a tap point

**Option A: RabbitMQ fanout copy (recommended).** Bind a *new, dedicated* queue (e.g.
`liveprobe.tap`) to the same exchange the internal library publishes to. RabbitMQ then delivers
every message to both the existing aggregator queue **and** the LiveProbe queue. Point the
collector's `rabbitmq` source at `liveprobe.tap`.

> ⚠️ **Never point the collector at the aggregators' existing queue.** Two consumers on one
> queue are *competing* consumers, so LiveProbe would *steal* half the messages from the
> client's pipeline. A tap must be its own queue bound to the exchange, so each side gets its own
> copy.

> The binding is built in. Set `EXCHANGE` (plus optional `ROUTING_KEY`, default `#`), and on
> startup the source declares or verifies the exchange, asserts your `QUEUE`, and binds it. No
> manual RabbitMQ admin step is needed. Use `EXCHANGE_TYPE` (e.g. `topic`, `fanout`) to
> *declare* the exchange. Omit it to bind to an exchange someone else already owns; that
> exchange is verified passively and never redeclared. For a `direct` exchange, set
> `ROUTING_KEY` to the exact key(s); fanout ignores it.

**Option B: stdout tee (zero RabbitMQ changes).** The client already writes the stream to
stdout. `tee` that stream (or point a log shipper at the same file) into the collector's `stdin`
source. Nothing about RabbitMQ changes at all, which makes this the most non-intrusive option
when you can't add a queue binding.

### Step 2: run LiveProbe

LiveProbe needs **no infrastructure of its own**: an in-memory window plus a local SQLite file,
with no database or broker to provision. It is one static binary. Run it anywhere that can reach
the tap, and keep it private:

```bash
PORT=4319 HOST=0.0.0.0 ./liveprobe     # or ./serve.sh, or `docker compose up -d`
```

### Step 3: run the collector (one instance)

```bash
# Option A — RabbitMQ fanout tap: binds a dedicated queue to the client's exchange
SOURCE=rabbitmq ADAPTER=adapter-id-1 \
  RABBITMQ_URL=amqp://<shared-rabbit-host> \
  EXCHANGE=<their-exchange> ROUTING_KEY="#" \
  QUEUE=liveprobe.tap QUEUE_MAX_LENGTH=100000 \
  LIVEPROBE_URL=http://<liveprobe-host>:4319 \
  ./liveprobe-collector
#   add EXCHANGE_TYPE=topic (or fanout) to declare the exchange if it doesn't exist yet;
#   omit EXCHANGE to consume a queue directly instead of tapping an exchange.

# Option B — stdout tap
SOURCE=stdin ADAPTER=adapter-id-1 LIVEPROBE_URL=http://<liveprobe-host>:4319 \
  ./liveprobe-collector < your-ndjson-stream
```

The collector consumes each message, runs `AdapterID1(raw)` on it, then batches the normalized
events and POSTs them to `/v1/events`. A message that is not valid JSON, or that makes the
adapter fail, is dropped (nack, no requeue), so one poison message can't wedge the queue.

### Step 4: open the UI

Traces assemble from the `trace_id` / `span_id` / `parent_span_id` already present in the
client's events. Sequence and topology render immediately.

### Where to run it (mixed VPS / GCP)

The collector must reach **both** the shared RabbitMQ and the LiveProbe server, and LiveProbe
must be reachable from the collector. Put LiveProbe and the collector on one small box near the
broker (a VPS, a GCP VM, Cloud Run, or a GKE pod) with network access to RabbitMQ (same VPC,
peered, or a firewall rule). The 10 apps' own locations don't matter, because they only talk to
RabbitMQ.

### What you get, and what's limited for this format

Works out of the box: **trace assembly, sequence diagrams, service topology** (derived from
parent/child `participant` transitions, so no OTel span kinds are needed), history, and search.

Some things are limited because `adapter-id-1` is event-centric. The adapter maps each event to
one span with `kind: "internal"` and `duration: 0` (`AdapterID1` in
`internal/collector/adapters.go`). So for this client there are **no latency bars and no
precise client→server request/response arrows**. Closing that gap needs the upstream library to
emit a span `kind` and a per-span duration (or span-end events). That *is* intrusive to the apps,
so treat it as an opt-in phase 2, not part of the minimal rollout. The full field mapping and
gaps are in the two sections above.

### Production caveats (state these to the client)

- **Best-effort delivery, by design.** The collector acks a RabbitMQ message once its events are
  buffered for send. If the `POST /v1/events` fails, the sink logs it and drops the batch
  without retrying (`Sink` in `internal/collector/sources.go`). LiveProbe is a
  **live-diagnostics view, not a system of record**. A dropped batch means a momentary gap in the
  live diagram, never data loss for the client, whose own aggregators still receive everything.
  Don't position it as durable storage.
- **Lost broker connections reconnect automatically.** The `rabbitmq` source watches for the
  connection closing and reconnects with exponential backoff (1 s base, doubling up to 30 s).
  Each attempt re-runs the full exchange/queue/bind/consume setup. Only the *initial* connect
  fails fast, because misconfiguration should be loud. Still run the collector under a supervisor
  (systemd `Restart=always`, docker `restart: unless-stopped`) to cover process-level crashes.
- **Bound the tap queue.** Because this is a live view, set `QUEUE_MAX_LENGTH` (and/or
  `QUEUE_MESSAGE_TTL_MS`) so the tap queue can't accumulate a huge backlog while the collector is
  down. Overflow drops the *oldest* messages (drop-head) and keeps the newest. These settings
  apply to queues the collector owns (the tap queue); don't set them on a pre-existing queue that
  has different arguments.
- **Exactly one consumer per stream.** LiveProbe assembles by trace/span id, so a second
  collector reading the *same* messages would double-count spans. Scale by giving each
  collector its *own* tap, never two collectors on one queue.
- **One collector per client.** For multiple clients, run a separate collector (and ideally a
  separate LiveProbe) per client, for isolation, per-client RabbitMQ credentials, and a blast
  radius of one. See Deployment modes above.

## Open questions (answer before building further)

1. Message granularity: on RabbitMQ, is each message **one** instrumentation event (JSON of
   the adapter-id-1 fields), or a batch/envelope? The same question applies to the stdout stream
   (one event per line?).
2. Deployment: the default is a **sidecar per client** (documented above). Confirm whether any
   clients need several sources handled together.
3. Do we want per-span **duration** (requires span-end events) for adapter-id-1, or stay
   structure-only (sequence + topology) and add latency later?
