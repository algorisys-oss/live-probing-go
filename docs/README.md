# LiveProbe docs

Detailed docs for the two halves of this repo: LiveProbe itself (`cmd/`, `internal/`) and the
Shopwave testbed (`testbed/`).

- [architecture.md](architecture.md): how the whole system fits together. Covers the LiveProbe
  pipeline (ingest → window → projections → UI), the testbed, and how they connect.
- [event-model.md](event-model.md): the normalized `Event` structure field by field, how
  OTLP spans map onto it, and how the topology and sequence diagrams are derived from it.
- [live-views.md](live-views.md): what the homepage **Live** view shows. The flow/topology
  graph is a rolling-window aggregate (nodes, edges, ghost peers, the two edge numbers); the
  trace list is per trace. Also covers how live differs from the persisted History view.
- [pii-masking.md](pii-masking.md): the masking helper (`core.MaskPII`) that scrubs PII and
  secrets at ingest. Covers the key-based and value-based layers and their limits.
- [shopwave-events.md](shopwave-events.md): the e-commerce testbed. Lists every HTTP action, the
  RabbitMQ messages published on each action, their exact payload structure, and the trace
  each user action produces.
- [integrating-your-app.md](integrating-your-app.md): **start here to wire your own system in.**
  How any app (dockerized or not, any language, greenfield or already-instrumented) emits
  into LiveProbe. Covers the OTLP/JSON fact, when you need a collector, and copy-paste recipes.
- [integration-adapters.md](integration-adapters.md): how to feed client apps that use a custom
  format into LiveProbe. Covers the adapter layer (client format → `[]Event`) and the collector
  (rabbitmq / stdin / http sources).
- [adapters/adapter-id-1.md](adapters/adapter-id-1.md): the client spec for the
  `adapter-id-1` structured-event format.
- [adapters/algo-instrumentation.md](adapters/algo-instrumentation.md): the
  `algo-instrumentation` format (instrumentation / log / audit events) and how the adapter
  maps it.

Contract:

- [migration-plan.md](migration-plan.md): the TypeScript → Go migration plan, with each
  phase's testable "done" checklist. Its
  [Known semantic deltas](migration-plan.md#known-semantic-deltas-deliberate-documented) section
  lists every deliberate behavioural difference from the TS implementation.

Not ported: the TS-era planning and history docs (`plan.md`, `plan-testbed.md`,
`feature-scope.md`, `implementation-review.md`, `adapters/skillzengine-adapter.md`) are kept in
the TS repo at [`../live-probing/docs/`](https://github.com/algorisys-oss/live-probing/tree/main/docs). They record how the TS
version was planned and reviewed, not how the Go version works, so they were deliberately not
ported.

The top-level [README](../README.md) is the short overview (quick start, configuration, API).
[CLAUDE.md](../CLAUDE.md) holds the conventions, and [HANDOFF.md](../HANDOFF.md) holds the
current state. These docs go deeper.
