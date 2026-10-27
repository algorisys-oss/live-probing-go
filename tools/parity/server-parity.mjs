// Server-level differential: feed the TS server and the Go server identical traffic (the
// input.json / otlp.json written by core-parity.ts), then diff every API route and the /ws
// snapshot. Both servers must already be running.
//   node tools/parity/server-parity.mjs <parity-dir> <ts-base-url> <go-base-url>
import { readFileSync } from "node:fs";

const [dir, tsBase, goBase] = process.argv.slice(2);
// NO_HOSTILE=1 drops the far-future event, which (correctly) becomes the live window's clock and
// evicts everything older — run both ways to cover the hostile path and a populated live window.
const input = JSON.parse(readFileSync(`${dir}/input.json`, "utf8")).filter(
  (e) => !(process.env.NO_HOSTILE && e.traceId === "hostile"),
);
const otlp = JSON.parse(readFileSync(`${dir}/otlp.json`, "utf8"));
if (process.env.NO_HOSTILE) {
  for (const rs of otlp.resourceSpans) for (const sc of rs.scopeSpans) sc.spans = sc.spans.filter((s) => s.traceId !== "hostile");
}

const post = (base, path, body, headers = {}) =>
  fetch(base + path, { method: "POST", headers: { "content-type": "application/json", ...headers }, body });

// --- feed identical traffic, in the same order, to both -------------------------------------
// SKIP_FEED=1 compares two servers opened on copies of an existing (TS-written) history DB.
for (const base of process.env.SKIP_FEED ? [] : [tsBase, goBase]) {
  for (let i = 0; i < input.length; i += 400) {
    const r = await post(base, "/v1/events", JSON.stringify({ events: input.slice(i, i + 400) }));
    if (r.status !== 200) throw new Error(`${base} events ${r.status}`);
  }
  for (const rs of otlp.resourceSpans) {
    const r = await post(base, "/v1/traces", JSON.stringify({ resourceSpans: [rs] }));
    if (r.status !== 200) throw new Error(`${base} traces ${r.status}`);
  }
}

// --- discover routes from the TS side -------------------------------------------------------
const tsGet = async (p) => (await fetch(tsBase + p)).json();
const { days } = await tsGet("/api/days");
const { traces: live } = await tsGet("/api/traces?limit=2000");
const { topology } = await tsGet("/api/topology");
const { groups } = await tsGet("/api/errors");
const { traces: hist } = await tsGet("/api/search?limit=40&sort=slowest");

const routes = [
  "/healthz", "/api/traces", "/api/traces?limit=7", "/api/traces?limit=-3", "/api/traces?limit=abc", "/api/traces?limit=",
  "/api/topology", "/api/errors", "/api/errors?limit=3", "/api/days", "/api/traces/does-not-exist",
  "/api/search", "/api/search?q=GET", "/api/search?error=true", "/api/search?error=false&limit=5",
  "/api/search?minMs=50&sort=slowest", "/api/search?maxMs=2&limit=20", "/api/search?minSpans=10",
  "/api/search?attr=http.method%3DGET&limit=10", "/api/search?attr=redacted", "/api/search?service=hrms&limit=15",
  "/api/day/1970-01-01/latency", "/api/day/nope/summary",
  ...live.slice(0, 25).map((t) => `/api/traces/${encodeURIComponent(t.traceId)}`),
  ...hist.map((t) => `/api/traces/${encodeURIComponent(t.traceId)}`),
  ...topology.nodes.map((n) => `/api/service/${encodeURIComponent(n)}`),
  ...groups.slice(0, 5).map((g) => `/api/search?traceId=${encodeURIComponent(g.sampleTraceId)}`),
];
for (const d of days.slice(0, 6)) {
  routes.push(`/api/day/${d.day}/summary`, `/api/day/${d.day}/traces`, `/api/day/${d.day}/traces?limit=5`);
  const sum = await tsGet(`/api/day/${d.day}/summary`);
  for (const ep of sum.topEndpoints.slice(0, 4)) {
    const e = encodeURIComponent(ep.operation);
    routes.push(`/api/day/${d.day}/latency?endpoint=${e}`, `/api/day/${d.day}/flamegraph?endpoint=${e}`);
  }
}

// --- compare ---------------------------------------------------------------------------------
const diffs = [];
function cmp(a, b, path) {
  if (diffs.length > 60) return;
  if (typeof a === "number" && typeof b === "number") {
    if (a === b || Math.abs(a - b) <= 1e-9 * Math.max(1, Math.abs(a))) return;
    return diffs.push(`${path}: ts=${a} go=${b}`);
  }
  if (a === null || b === null || typeof a !== "object" || typeof b !== "object") {
    if (a !== b) diffs.push(`${path}: ts=${JSON.stringify(a)?.slice(0, 160)} go=${JSON.stringify(b)?.slice(0, 160)}`);
    return;
  }
  if (Array.isArray(a)) {
    if (!Array.isArray(b)) return diffs.push(`${path}: array vs object`);
    if (a.length !== b.length) diffs.push(`${path}: length ts=${a.length} go=${b.length}`);
    for (let i = 0; i < Math.min(a.length, b.length); i++) cmp(a[i], b[i], `${path}[${i}]`);
    return;
  }
  for (const k of new Set([...Object.keys(a), ...Object.keys(b)])) {
    if (!(k in a)) diffs.push(`${path}.${k}: missing in ts`);
    else if (!(k in b)) diffs.push(`${path}.${k}: missing in go`);
    else cmp(a[k], b[k], `${path}.${k}`);
  }
}

for (const r of routes) {
  const [ta, ga] = await Promise.all([fetch(tsBase + r), fetch(goBase + r)]);
  if (ta.status !== ga.status) diffs.push(`${r}: status ts=${ta.status} go=${ga.status}`);
  cmp(await ta.json(), await ga.json(), r);
}

// Error paths, CORS, preflight.
for (const [label, req] of [
  ["bad json", (b) => post(b, "/v1/events", "{nope")],
  ["empty body", (b) => post(b, "/v1/events", "")],
  ["unknown post", (b) => post(b, "/nope", "{}")],
  ["preflight", (b) => fetch(b + "/v1/traces", { method: "OPTIONS", headers: { origin: "http://localhost:5173" } })],
  ["cors ok", (b) => fetch(b + "/api/days", { headers: { origin: "http://127.0.0.1:3000" } })],
  ["cors evil", (b) => fetch(b + "/api/days", { headers: { origin: "https://evil.example" } })],
]) {
  const [ta, ga] = await Promise.all([req(tsBase), req(goBase)]);
  if (ta.status !== ga.status) diffs.push(`${label}: status ts=${ta.status} go=${ga.status}`);
  const h = "access-control-allow-origin";
  if (ta.headers.get(h) !== ga.headers.get(h)) diffs.push(`${label}: ${h} ts=${ta.headers.get(h)} go=${ga.headers.get(h)}`);
}

// WebSocket snapshot. Alert `since` is wall-clock (first seen by a tick during the feed), and
// alerts are ordered by it, so `since` is dropped and same-severity alerts compared as a set.
const snapshot = (base) =>
  new Promise((resolve, reject) => {
    const ws = new WebSocket(base.replace(/^http/, "ws") + "/ws");
    ws.onmessage = (m) => (ws.close(), resolve(JSON.parse(m.data)));
    ws.onerror = reject;
  });
const [ts, go] = await Promise.all([snapshot(tsBase), snapshot(goBase)]);
for (const s of [ts, go]) {
  for (const a of s.alerts) delete a.since;
  s.alerts.sort((a, b) => a.severity.localeCompare(b.severity) || a.id.localeCompare(b.id));
}
cmp(ts, go, "ws.snapshot");

console.log(`routes compared: ${routes.length + 6} + ws snapshot (${ts.traces.length} traces, ${ts.alerts.length} alerts)`);
if (diffs.length) {
  console.log(diffs.join("\n"));
  process.exit(1);
}
console.log("SERVER PARITY OK");
