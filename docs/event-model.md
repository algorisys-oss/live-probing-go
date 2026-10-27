# The event model

Everything LiveProbe draws is a projection of a stream of one type: the normalized `Event`.
One `Event` corresponds to exactly one span. Defined in
[`internal/core/event.go`](../internal/core/event.go).

## The `Event` shape

```go
type Event struct {
	TraceID      string     `json:"traceId"`                // groups spans into one trace
	SpanID       string     `json:"spanId"`                 // unique within the trace
	ParentSpanID string     `json:"parentSpanId,omitempty"` // empty => this span is a trace root
	Participant  string     `json:"participant"`            // the service doing this span's work (resource service.name)
	Peer         string     `json:"peer,omitempty"`         // the other side for outbound spans (service or datastore)
	Operation    string     `json:"operation"`              // human label: "POST /api/checkout", "pg.query:SELECT", ...
	Kind         SpanKind   `json:"kind"`                   // client | server | producer | consumer | internal
	StartTime    int64      `json:"startTime"`              // epoch MICROSECONDS
	Duration     int64      `json:"duration"`               // MICROSECONDS
	Status       SpanStatus `json:"status"`                 // ok | error | unset
	Attributes   Attributes `json:"attributes"`             // raw span attributes, scalars only
}
```

```go
type SpanKind   string // "client" | "server" | "producer" | "consumer" | "internal"
type SpanStatus string // "ok" | "error" | "unset"
type Attributes map[string]any // string, float64 or bool values
```

The JSON field names are the wire format of `POST /v1/events`, and they are identical to the TS
implementation's.

`DatastoreSystems` is a set of infrastructure peers (`postgresql`, `mysql`, `redis`,
`rabbitmq`, `amqp`, `kafka`, `mongodb`, `memcached`, `elasticsearch`). It decides when a
`peer` is drawn as a datastore node rather than treated as a service.

### Field notes

- **participant vs peer.** `participant` is *who ran this span*, which is always the emitting
  service. `peer` is *who they talked to*, and it is only set for outbound spans. A Postgres
  query span has `participant: "catalog"`, `peer: "postgresql"`.
- **operation** is the display label. HTTP spans get `"METHOD /path"` (see below). DB spans
  keep their instrumentation name (`"pg.query:SELECT"`), and messaging spans keep theirs. The
  Go testbed names them `"publish order.created"` and `"process order.created"`, while Node's
  amqplib instrumentation used names like `"publish shopwave.orders"`.
- **Times are integer microseconds.** OTLP sends nanosecond strings, which are divided by 1000.
  Fractional micros are truncated, both here and on native ingest. Absurd values are clamped:
  a start time more than 60 s in the future is pulled back to now + 60 s, and anything
  non-finite or negative becomes 0.
- **Attributes are scalars only.** Native ingest drops nested object/array values. The TS
  implementation passed them through even though its own type said otherwise; see
  [migration-plan.md, delta 4](migration-plan.md#known-semantic-deltas-deliberate-documented).

## OTLP → Event mapping

`NormalizeOTLP(payload)` in [`otlp.go`](../internal/core/otlp.go) walks OTLP/HTTP JSON:
`resourceSpans[] → scopeSpans[] → spans[]`. The legacy `instrumentationLibrarySpans` key is
also accepted.

| Event field | Source | Rule |
|-------------|--------|------|
| `participant` | `resource.attributes["service.name"]` | falls back to `"unknown"` |
| `traceId` / `spanId` / `parentSpanId` | span fields | spans missing a trace or span id are skipped; an empty `parentSpanId` means a root |
| `operation` | `span.name`, enriched | HTTP: `METHOD path` (below); otherwise the span name |
| `kind` | `span.kind` | int or string enum → our kinds (below) |
| `startTime` / `duration` | `startTimeUnixNano` / `endTimeUnixNano` | nanos (string or number, parsed without float loss) / 1000 → micros |
| `status` | `span.status.code` | 2/ERROR → `error`, 1/OK → `ok`, else `unset` |
| `peer` | attributes | resolved by priority (below) |
| `attributes` | `span.attributes` | flattened to a plain map |

### Kind mapping

OTLP kind is `2 SERVER, 3 CLIENT, 4 PRODUCER, 5 CONSUMER`, and anything else is `internal`.
Values may arrive as numbers, numeric strings, or `"SPAN_KIND_SERVER"` etc.; all are handled.
Kind is the reference oracle for arrow direction in both diagrams.

### Peer resolution (outbound spans only)

For `client` / `producer` / `consumer` spans, the first present attribute wins:

1. `db.system`, then `db.system.name` (e.g. `postgresql`, `redis`): a datastore peer
2. `messaging.system` (e.g. `rabbitmq`): a broker peer
3. `peer.service` → `server.address` → `net.peer.name` → `network.peer.address`: a service/host
4. `messaging.destination.name`: a queue/exchange

`server` and `internal` spans have no peer.

`db.system.name` is the OTel semantic-conventions ≥ 1.33 name for `db.system`, and modern SDKs
such as `otelpgx` emit only that one. The TS implementation read only `db.system`, so a modern
SDK's Postgres spans fell through to `server.address` and showed up as a ghost `postgres` host.
This is a deliberate enhancement
([delta 15](migration-plan.md#known-semantic-deltas-deliberate-documented)).

### HTTP name enrichment

HTTP instrumentation frequently names a span by its bare method (`"GET"`, `"POST"`), which makes
a useless trace title. When a span carries an HTTP method and a path, `operation` becomes
`"METHOD path"`:

- method from `http.request.method`, or `http.method`
- path from `http.route` (unless it is a catch-all `"*"`), else `url.path`, else
  `http.target`, with the query string stripped

So `POST` + `http.target=/api/cart/items?x=1` → `operation = "POST /api/cart/items"`.

### Attribute-value decoding

OTLP attribute values are wrapped: `{ stringValue }`, `{ boolValue }`, `{ intValue }`
(string or number), `{ doubleValue }`. Each one is unwrapped to a scalar. Unknown shapes are
dropped.

## From events to diagrams

### Trace assembly (`TraceWindow.Assemble`)

Spans are bucketed by `traceId`. A tree is built by linking each span to its
`parentSpanId`. Spans whose parent is not in the window become **roots**. Children are sorted
by `startTime`, with ties broken by `spanId`. Assembly is order-independent: a child can arrive
before its parent and still links correctly once both are present. Go maps don't keep insertion
order, so the window records insertion sequence numbers. That keeps every listing in the same
order the TS version produced.

### Topology (`TraceWindow.Topology`)

The whole window is aggregated into `{ nodes, edges, externals }`. Edges come from two sources:

1. **service → service**: for any span whose parent (same trace) has a *different*
   `participant`, add an edge `parent.participant → span.participant`. A full Shopwave trace is
   connected across RabbitMQ, so this captures async hops too (order → payment-worker).
2. **service → peer**: for any span with a `peer` whose callee did **not** report a span of its
   own (no cross-participant child), add an edge `participant → peer`. This covers datastores
   (`peer` in `DatastoreSystems`) *and* **uninstrumented services**. Uninstrumented services are
   listed in `externals` and render as dashed "ghost" nodes, so a partially onboarded system
   still shows every observed call. When the callee did report (a cross-participant child
   exists), the peer edge is suppressed so the call isn't drawn twice.

Each edge accumulates `calls`, `errors` (spans with status `error`), and an average duration.
`externals` never includes datastores or names that also appear as participants.

### Sequence (`SequenceFor`)

One assembled trace → `{ participants, messages, externals }`, walking spans in `startTime`
order:

- A span whose parent is a *different* participant emits a message `parent → span`, labelled
  with `operation`.
- A span calling a peer (datastore or uninstrumented service) emits `participant → peer`,
  unless the callee reported its own span (then the parent/child arrow already covers the
  call). Uninstrumented peers are listed in `externals` and render as dashed ghost lifelines.
- Same-participant internal work is not drawn.
- `async: true` when the span is a `consumer` or its parent is a `producer` (the RabbitMQ
  boundary). The UI renders this as an async arrow.

Each message carries the `spanId` it represents, which links it to the waterfall row.
`participants` are listed in order of first appearance.

## Why one model

Sequence and flow are two projections of the same `[]Event`. Spans are interpreted in exactly
one place (`NormalizeOTLP`, or `CoerceEvents` for native input), and trace structure is derived
in exactly one place (`TraceWindow`). Bugs have one place to hide, and a new view is just
another projection.
