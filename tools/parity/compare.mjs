// Deep-compare ts-output.json and go-output.json. Numbers compare with a tiny relative
// tolerance (float formatting differs between runtimes). Prints the first diffs by path.
//   node tools/parity/compare.mjs <dir> [ts-file go-file]   (default ts-output.json go-output.json)
import { readFileSync } from "node:fs";

const [dir, tsFile = "ts-output.json", goFile = "go-output.json"] = process.argv.slice(2);
const ts = JSON.parse(readFileSync(`${dir}/${tsFile}`, "utf8"));
const go = JSON.parse(readFileSync(`${dir}/${goFile}`, "utf8"));

const diffs = [];
const counts = {};
// startTime/duration are compared as ingest will see them: clamped to [0, MAX_EPOCH_MICROS].
// (Go saturates int64 where JS keeps e.g. 1e24; V8's legacy Date.parse turns "200" into year 200
// where Go yields 0 — both clamp to the same value, so the difference is unobservable.)
const MAX_EPOCH_MICROS = 8.64e15;
const clampMicros = (n) => (!(n > 0) ? 0 : Math.min(n, MAX_EPOCH_MICROS));
const isTimeField = (path) => /\.(startTime|duration)$/.test(path);
let total = 0; // every diff is counted; only the first 40 are kept for printing
const push = (d) => (total++, diffs.length < 40 && diffs.push(d));
function cmp(a, b, path) {
  if (typeof a === "number" && typeof b === "number" && isTimeField(path)) {
    a = clampMicros(a);
    b = clampMicros(b);
  }
  if (typeof a === "number" && typeof b === "number") {
    if (a === b || Math.abs(a - b) <= 1e-9 * Math.max(1, Math.abs(a))) return;
    return push(`${path}: ts=${a} go=${b}`);
  }
  if (a === null || b === null || typeof a !== "object" || typeof b !== "object") {
    if (a !== b) push(`${path}: ts=${JSON.stringify(a)?.slice(0, 200)} go=${JSON.stringify(b)?.slice(0, 200)}`);
    return;
  }
  if (Array.isArray(a) !== Array.isArray(b)) return push(`${path}: array vs object`);
  if (Array.isArray(a)) {
    if (a.length !== b.length) push(`${path}: length ts=${a.length} go=${b.length}`);
    for (let i = 0; i < Math.min(a.length, b.length); i++) cmp(a[i], b[i], `${path}[${i}]`);
    return;
  }
  for (const k of new Set([...Object.keys(a), ...Object.keys(b)])) {
    if (!(k in a)) push(`${path}.${k}: missing in ts (go=${JSON.stringify(b[k])?.slice(0, 120)})`);
    else if (!(k in b)) push(`${path}.${k}: missing in go (ts=${JSON.stringify(a[k])?.slice(0, 120)})`);
    else cmp(a[k], b[k], `${path}.${k}`);
  }
}

for (const key of Object.keys(ts)) {
  const before = total;
  cmp(ts[key], go[key], key);
  counts[key] = total - before;
}
console.log("sections:", Object.entries(counts).map(([k, n]) => `${k}=${n === 0 ? "ok" : n + " diff"}`).join(" "));
if (ts.traceIds) console.log("sizes: traces", ts.traceIds.length, "events", ts.coerced.length, "otlp", ts.otlpEvents.length);
if (total) {
  console.log(`${total} diffs; first ${diffs.length}:\n` + diffs.join("\n"));
  process.exit(1);
}
console.log("PARITY OK");
