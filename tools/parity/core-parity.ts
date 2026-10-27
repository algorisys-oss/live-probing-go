// Differential parity harness, TS side. Samples real traces from a LiveProbe history DB, builds
// native + OTLP inputs (plus hostile and PII cases), runs them through the ORIGINAL TS core and
// writes ts-output.json. The Go side (internal/core/parity_test.go) runs the same inputs through
// the port; compare.mjs diffs the two. See tools/parity/README.md.
//
//   npx tsx tools/parity/core-parity.ts <out-dir> <history.db> [sample-mod]
import { DatabaseSync } from "node:sqlite";
import { mkdirSync, writeFileSync } from "node:fs";
import * as core from "../../../live-probing/packages/core/src/index.ts";

const [outDir, dbPath, modArg] = process.argv.slice(2);
if (!outDir || !dbPath) throw new Error("usage: core-parity.ts <out-dir> <history.db> [sample-mod]");
const mod = Number(modArg ?? "211");
mkdirSync(outDir, { recursive: true });

const db = new DatabaseSync(dbPath, { readOnly: true });
const rows = db
  .prepare(
    `SELECT trace_id, detail_json FROM traces WHERE rowid % ? = 0
     UNION SELECT trace_id, detail_json FROM (SELECT * FROM traces WHERE span_count > 20 LIMIT 150)`,
  )
  .all(mod) as Array<{ trace_id: string; detail_json: string }>;

// --- inputs ---------------------------------------------------------------------------------
const input: Record<string, unknown>[] = [];
for (const r of rows) {
  const d = JSON.parse(r.detail_json) as { spans: Array<Record<string, unknown>> };
  for (const s of d.spans) {
    const { depth: _depth, ...span } = s;
    input.push({ traceId: r.trace_id, ...span });
  }
}
input.reverse(); // children before parents: exercises out-of-order assembly

// PII on a few real spans, and hostile/sloppy events.
for (let i = 0; i < input.length; i += 97) {
  const e = input[i]!;
  e["attributes"] = {
    ...(e["attributes"] as object),
    "user.email": "jane@acme.com",
    note: "card 4242 4242 4242 4242 for bob@shop.io",
    authorization: "Bearer abc.def",
    customer_pin: 1234,
  };
  e["operation"] = `${String(e["operation"])} for jane.doe@acme.com`;
}
input.push(
  { traceId: "hostile", spanId: "future", participant: "svc", operation: "x", startTime: 1e30, duration: 1 },
  { traceId: "hostile", spanId: "neg", parentSpanId: "future", participant: "svc", startTime: -5, duration: "12", kind: "bogus", status: "nope" },
  { traceId: "hostile", spanId: "strnum", participant: "", peer: "redis", kind: "client", startTime: "1783347950269000", duration: 7 },
  { spanId: "no-trace" },
);

// OTLP rendering of the same spans, grouped per service as an exporter would send them.
const KIND_NUM: Record<string, number> = { internal: 1, server: 2, client: 3, producer: 4, consumer: 5 };
const STATUS_NUM: Record<string, number> = { unset: 0, ok: 1, error: 2 };
const byService = new Map<string, unknown[]>();
for (const e of core.coerceEvents(input)) {
  const attrs = Object.entries(e.attributes).map(([key, v]) => ({
    key,
    value:
      typeof v === "string"
        ? { stringValue: v }
        : typeof v === "boolean"
          ? { boolValue: v }
          : Number.isInteger(v)
            ? { intValue: String(v) }
            : { doubleValue: v },
  }));
  if (e.peer) attrs.push({ key: "peer.service", value: { stringValue: e.peer } });
  const list = byService.get(e.participant) ?? byService.set(e.participant, []).get(e.participant)!;
  list.push({
    traceId: e.traceId,
    spanId: e.spanId,
    parentSpanId: e.parentSpanId ?? "",
    name: e.operation,
    kind: KIND_NUM[e.kind],
    startTimeUnixNano: String(BigInt(Math.trunc(e.startTime)) * 1000n + 123n),
    endTimeUnixNano: String(BigInt(Math.trunc(e.startTime + e.duration)) * 1000n + 456n),
    attributes: attrs,
    status: { code: STATUS_NUM[e.status] },
  });
}
const otlp = {
  resourceSpans: [...byService.entries()].map(([svc, spans]) => ({
    resource: { attributes: [{ key: "service.name", value: { stringValue: svc } }] },
    scopeSpans: [{ spans }],
  })),
};

writeFileSync(`${outDir}/input.json`, JSON.stringify(input));
writeFileSync(`${outDir}/otlp.json`, JSON.stringify(otlp));

// --- run the TS core ------------------------------------------------------------------------
const coerced = core.coerceEvents(input);
// Clock-relative clamping differs between runs; normalise it so outputs are comparable.
for (const e of coerced) if (e.traceId === "hostile" && e.spanId === "future") e.startTime = 0;
const otlpEvents = core.normalizeOtlp(otlp as core.OtlpPayload);
const masked = core.maskEvents(coerced);

const w = new core.TraceWindow({ maxTraces: 1e9, horizonMicros: 1e15 });
w.add(masked);
const traceIds = w.traceIds();
const traces = traceIds.map((id) => w.assemble(id)!);
const perTrace = traces.map((t) => {
  const sequence = core.sequenceFor(t);
  const layout = core.sequenceLayout(sequence);
  const expanded = core.sequenceLayout(sequence, new Set(layout.groups.map((g) => g.id)));
  return {
    assembled: t,
    sequence,
    layout,
    expanded,
    criticalPath: core.criticalPath(t),
    mermaid: core.toMermaidSequence(sequence),
  };
});
const topology = w.topology();
const red = core.redMetrics(traces);
const alerts = core.evaluateAlerts({ services: red, edges: topology.edges });

const byRoot = new Map<string, core.FlameSpan[][]>();
for (const t of traces) {
  const op = t.roots[0]?.event.operation ?? "";
  (byRoot.get(op) ?? byRoot.set(op, []).get(op)!).push(t.events);
}
const flamegraphs = [...byRoot.entries()].map(([op, spans]) => ({ op, root: core.aggregateFlamegraph(spans) }));

const small = new core.TraceWindow({ maxTraces: 300, horizonMicros: 120_000_000 });
for (let i = 0; i < coerced.length; i += 500) small.add(coerced.slice(i, i + 500));

writeFileSync(
  `${outDir}/ts-output.json`,
  JSON.stringify({
    coerced,
    otlpEvents,
    masked,
    traceIds,
    perTrace,
    topology,
    red,
    alerts,
    mermaidFlow: core.toMermaidFlow(topology),
    flamegraphs,
    buckets: coerced.map((e) => core.latencyBucketIndex(e.duration)),
    smallWindowIds: small.traceIds(),
  }),
);
console.log(`ts: ${rows.length} traces, ${input.length} raw events, ${otlpEvents.length} otlp events -> ${outDir}`);
