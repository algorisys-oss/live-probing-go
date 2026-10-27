# The live views

The LiveProbe homepage (`/`, the **Live** view) shows two things side by side, and they answer
different questions. Both are projections of the same event stream (see
[event-model.md](event-model.md)). They differ only in how they slice it.

- **Live Traces list** (left): individual requests. Each row is one trace, and opening it
  shows a *sequence* diagram (who called whom, in what order, for one request).
- **Flow graph** (center): the whole system, aggregated. Every trace currently in the rolling
  window is collapsed into one topology (who calls whom in general, with volume and latency).
  The **Matrix** toggle shows the same topology as a dependency matrix.

RED tiles (rate, error rate, p95 per service) sit above the graph. All of these regions are
rendered on the server and pushed as HTML fragments over `/ui/live` (see
[architecture.md](architecture.md#internalui-server-rendered-dashboard)).

This doc is mostly about the flow graph, because "what does the changing graph represent?"
is the common question.

## The flow graph: what it represents at any instant

> **An aggregate graph of the whole system, computed fresh from the in-memory rolling
> window. It is not one trace.**

It says: *across all the traffic I've seen recently, here's who called whom, how many times,
and how fast.* It is built by [`TraceWindow.Topology()`](../internal/core/trace-window.go),
which collapses every trace in the window into `participant → peer` edges and overlays them.
The graph is laid out and coloured in Go (`internal/ui/graph.go`, `internal/ui/view`) and
re-sent on each topology tick (~750 ms).

### The window *is* the "point in time"

The graph is live and moving because its source is a **rolling window**, not a cumulative
total:

- The window holds recent traces only: a **5-minute horizon** by default
  (`LIVE_WINDOW_MINUTES`), capped at `MAX_TRACES` traces (default 2000).
- Traces older than the horizon are **evicted**, and excess traces past the cap are dropped.
- As new traces stream in and old ones age out, edge counts and latencies shift.

So the graph is a **live moving average of your topology over the last few minutes**, not an
all-time count. If the system is idle for longer than the horizon, its edges fade out.

## Reading the elements

| Element | Meaning |
|---|---|
| **Node** | A service / participant (`client`, `hrms.dashboard`, `hrms.login`, …). Click a service node → its service page. |
| **Edge** | An aggregated call relationship. Stroke width scales with `log(calls)`, so thicker means busier. |
| **Edge top number** | **Call count** in the window (e.g. `client → hrms.login` = `3`), plus `· N err` when some calls failed. |
| **Edge bottom number** | **Average duration** across those calls (e.g. `1.40s`). |
| **Colour** | Health. Nodes and edges turn amber/red past the error-rate or latency thresholds (the same ones the alerts use). |
| **Datastore node** | e.g. `redis`. Styled distinctly from services. |
| **Dashed / "ghost" node** | e.g. `database`. This is an **external/uninstrumented peer**: instrumented services call it, but it never reported spans of its own. It marks a partial-rollout blind spot: you see calls *into* it, not what it does (`externals` in `Topology()`). |

The service filter (`?service=`) narrows the graph and the trace list to one service and its
direct neighbours. The matrix metric (`?view=matrix&metric=calls|errors|latency`) is also in
the URL, so a filtered view can be shared as a link. Zoom and pan are client-side and survive
live ticks.

## The mental model

```
Live Traces list  ──►  one traceId  ──►  sequence view (per-request order + timing)
Flow graph        ──►  whole window ──►  topology  (system shape + volume + latency)
```

Both come from the same normalized `Event` stream. The sequence view filters to a single
`traceId`, and the flow view aggregates the window.

## The sequence view (trace page)

Opening a trace (`/trace/{id}?tab=sequence`) renders a UML-style sequence diagram from
`LayoutSequence()` (pure, in
[`internal/core/sequence-layout.go`](../internal/core/sequence-layout.go)). The SVG is
drawn server-side. Reading it:

- **Lifeline**: one per participant (service / datastore / ghost peer).
- **Activation bar**: the rectangle on a lifeline. The participant is "active" from the call
  that entered it down to its return. Nested bars are nested calls (a parent's bar encloses its
  children's). Overlapping calls on one lifeline step right into depth lanes.
- **Solid arrow, filled head**: a synchronous **call**.
- **Dashed arrow, open head**: the **return** (reply), labelled with the call's duration.
- **Solid arrow, open/stick head**: an **async** message (producer/consumer). It is
  fire-and-forget, so it has no return arrow and no activation bar.
- **×N group**: a run of three or more identical calls (an N+1, e.g. cart → catalog once per
  cart line) folds into one row. Expanding it adds the group's id to `?expand=`, so the expanded
  state is a link too.

The rule: **dashing distinguishes reply from call, and the arrowhead distinguishes sync from
async.** Nesting is inferred from time containment (a sync parent is active while its children
run), not from parent ids, so it works on any stream that carries per-span start + duration.
Clicking a bar or arrow selects that span in the shared detail panel (the same selection as the
waterfall tab).

## The waterfall (trace page)

The other trace-page tab (the default) is a Gantt waterfall of the spans. Beyond the bars
(positioned by start + duration, coloured per service), it surfaces three things:

- **Critical path**: spans marked ◆ with an outlined bar are on the *critical path*, the chain
  that determines the trace's end time. It is computed by `CriticalPath()`
  ([`internal/core/flamegraph.go`](../internal/core/flamegraph.go)) with a backward
  last-finisher sweep. Work that ran concurrently in another span's shadow is deliberately *not*
  on the path, because shortening it wouldn't shorten the trace.
- **Self-time shading**: the hatched region of a bar is time covered by its children, and the
  solid remainder is the span's own **self-time**. A leaf span is fully solid.
- **Collapse / expand**: the chevron on a span with children folds its subtree and shows a "+N"
  hidden count. This is useful for deep traces.

Selecting a span (here or in the sequence tab) writes `#<spanId>` to the URL and fetches that
span's detail panel from the server. A trace link can therefore deep-link straight to one span,
and switching tabs keeps the selection.

## Caveats

- **Edge latency is a plain average** (`totalDuration / calls`), so a single slow outlier
  skews an edge. The graph has no percentiles. The RED tiles show a p95 per service, computed
  from the live window.
- **Day-level percentiles (p50/p95/p99), throughput and latency charts live on the History
  side.** They are computed from the persisted SQLite history store (per UTC day), not from the
  live window. See [architecture.md](architecture.md) for the live-vs-history split.
- **The live window is in memory and not durable.** If you restart the server, the flow graph
  starts empty. Only the SQLite history survives (per-trace rows, kept for a rolling
  `RETENTION_DAYS` days).
