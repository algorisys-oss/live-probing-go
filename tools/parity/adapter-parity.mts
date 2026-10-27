// Adapter differential, TS side. Builds a seeded fuzz corpus (the TS test fixtures plus random
// mutations: dropped keys and type-swapped values), runs every collector adapter over it, and
// writes adapter-corpus.json + adapter-ts.json. internal/collector/parity_test.go writes
// adapter-go.json; compare with:  node tools/parity/compare.mjs <dir> adapter-ts.json adapter-go.json
//
//   <ts-checkout>/node_modules/.bin/tsx tools/parity/adapter-parity.mts <out-dir> <ts-checkout> [count]
import { mkdirSync, writeFileSync } from "node:fs";
import { pathToFileURL } from "node:url";

const [outDir, tsRoot, countArg] = process.argv.slice(2);
if (!outDir || !tsRoot) throw new Error("usage: adapter-parity.ts <out-dir> <ts-checkout> [count]");
const count = Number(countArg ?? "4000");
mkdirSync(outDir, { recursive: true });
const { adapters } = await import(pathToFileURL(`${tsRoot}/packages/collector/src/adapters/index.ts`).href);

const TS = "2026-05-28T10:15:00Z";
const templates: unknown[] = [
  { eventId: "e1", eventType: "instrumentation", timestamp: TS, application: { name: "propeak", module: "invoice", environment: "prod", service: "api", version: "1" },
    request: { requestId: "req-1", traceId: "tr-1", spanId: "rs-1" }, severity: "INFO", message: "m", tags: { a: "b", n: 2 },
    payload: { endpoint: "/x", method: "POST", statusCode: 200, status_code: 201, durationMs: 135.5, success: true, memory_usage_mb: 1.5,
      spans: [{ spanId: "c0", kind: "redis", name: "get", startTime: TS, durationMs: 0.4 }] },
    spans: [
      { spanId: "c1", parentSpanId: "rs-1", traceId: "tr-1", kind: "db", name: "q", startTime: TS, endTime: "2026-05-28T10:15:00.005Z", durationMs: 5.39, success: true },
      { spanId: "c2", kind: "Function", name: "f", startTime: TS, endTime: "2026-05-28T10:15:00.002Z", success: false },
      { spanId: "c3", kind: "http", startTime: "2026-05-28T10:15:00.001", durationMs: "2" },
    ] },
  { eventId: "l1", eventType: "log", timestamp: TS, application: { name: "hrms", module: "employee" }, request: { requestId: "rq" },
    payload: { level: "fatal", message: "boom", file: "f.go", line: 3 } },
  { eventId: "a1", eventType: "audit", timestamp: "2026-05-28", application: { name: "hrms" },
    payload: { entityType: "employee", entityId: "E1", action: "UPDATE", before: { s: 1, x: { y: 1 } }, after: { s: 2 }, changedFields: ["s", "", 3], reason: "r", createdBy: "c", userId: "u" } },
  { eventId: "u1", eventType: "custom", timestamp: TS, application: { name: "x", module: "" }, payload: { a: 1, b: "s", c: true, d: null, e: [1] } },
  { events: [{ eventId: "b1", eventType: "instrumentation", timestamp: TS, application: { name: "h" }, request: { requestId: "r1" }, payload: { status_code: 503 } }, "junk", null] },
  { trace_id: "t1", span_id: "s2", parent_span_id: "s1", service_category: "orders-api", component: "OrdersModule", service: "OrderState",
    event_type: "Created", event_value: "42", correlation_id: "ORD-123", reference_id: "R1", reference_type: "OrderID", level: "critical",
    ou_id: "OU9", timestamp: 1_700_000_000_000.4, span_start_timestamp: "1700000000123", details: { exception_name: "IOException", retries: 3, nested: { a: 1 } } },
  { trace_id: 12, span_id: true, span_name: "sn", event_type: "E", timestamp: "2026-05-28T10:15:00+05:30", details: ["x", 2] },
  { traceId: "n1", spanId: "s", participant: "p", operation: "o", kind: "server", startTime: 1_700_000_000_000_000, duration: 5, status: "ok", attributes: { a: 1 } },
];

// mulberry32: tiny deterministic PRNG so both sides see the same corpus.
let seed = 0x5eed;
const rand = () => {
  seed = (seed + 0x6d2b79f5) | 0;
  let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
  t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
  return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
};
const pick = <T>(xs: T[]): T => xs[Math.floor(rand() * xs.length)]!;
const VALUES: unknown[] = [null, "", 0, -5, 1.5, 999.5, 2.5, "abc", " 200 ", "0x10", "inf", "Infinity", "1e3", true, false, [], {}, [1, "x"],
  "2026-05-28T10:15:00", "2026-05-28", "2026-05-28T10:15:00.123456Z", "1700000000000", 1e21, "ERROR", "db", "instrumentation", "log", "audit",
  // human/legacy date formats real emitters send (V8 Date.parse is the oracle)
  "2026/05/28 10:15", "05/28/2026", "May 28 2026", "May 28, 2026 10:15:00", "28 May 2026 10:15", "2026-5-8",
  "Thu, 28 May 2026 10:15:00 GMT", "Thu May 28 2026 10:15:00 GMT+0530 (India Standard Time)",
  "2026-05-28T10:15:00.123+0000", "2026-05-28 10:15:00", "2026-05-28T10:15Z", "20260528T101500Z"];

function mutate(v: unknown, depth = 0): unknown {
  if (Array.isArray(v)) return v.map((x) => (rand() < 0.3 ? mutate(x, depth + 1) : x));
  if (v && typeof v === "object") {
    const out: Record<string, unknown> = {};
    for (const [k, x] of Object.entries(v)) {
      const r = rand();
      if (r < 0.12) continue; // drop the key
      out[k] = r < 0.3 ? pick(VALUES) : r < 0.55 && depth < 4 ? mutate(x, depth + 1) : x;
    }
    return out;
  }
  return rand() < 0.5 ? pick(VALUES) : v;
}

const corpus: unknown[] = [...templates, null, "str", 5, [1, 2]];
while (corpus.length < count) corpus.push(mutate(pick(templates)));

const out: Record<string, unknown[]> = {};
for (const [name, adapt] of Object.entries(adapters) as [string, (raw: unknown) => unknown[]][]) {
  out[name] = corpus.map((raw) => {
    try {
      return adapt(raw);
    } catch (err) {
      return [{ thrown: String(err) }];
    }
  });
}
// coerceEvents clamps against the wall clock; the native adapter's far-future values are compared
// via their clamp, so keep corpus timestamps in the past (they are: 2023-2026).
writeFileSync(`${outDir}/adapter-corpus.json`, JSON.stringify(corpus));
writeFileSync(`${outDir}/adapter-ts.json`, JSON.stringify(out));
console.log(`ts: ${corpus.length} raw events x ${Object.keys(out).length} adapters -> ${outDir}`);
