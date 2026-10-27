// UI view-logic differential, TS side: layout, topology health, dependency matrix and number
// formatting over random topologies (+ the real one from core-parity's ts-output.json, if present).
// internal/ui/view/parity_test.go writes view-go.json.
//   <ts-checkout>/node_modules/.bin/tsx tools/parity/view-parity.mts <dir> <ts-checkout> [count]
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { pathToFileURL } from "node:url";

const [dir, tsRoot, countArg] = process.argv.slice(2);
if (!dir || !tsRoot) throw new Error("usage: view-parity.mts <dir> <ts-checkout> [count]");
const lib = (f: string) => import(pathToFileURL(`${tsRoot}/packages/ui/src/lib/${f}.ts`).href);
const { computeLayout } = await lib("layout");
const { topologyHealth } = await lib("topology-health");
const { dependencyMatrix } = await lib("dependency-matrix");
const { formatMicros } = await lib("format");

let seed = 0xbeef;
const rand = () => {
  seed = (seed + 0x6d2b79f5) | 0;
  let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
  t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
  return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
};
const NAMES = ["gateway", "auth", "catalog", "cart", "order", "payment", "inventory", "notify", "postgresql", "redis",
  "rabbitmq", "orders-postgresql", "legacy-api", "hrms.roles", "client", "élan", "a", "b", "svc-9", "kafka"];

type Topo = { nodes: string[]; edges: { from: string; to: string; calls: number; errors: number; avgDurationMicros: number }[] };
const topologies: Topo[] = [];
const real = `${dir}/ts-output.json`;
if (existsSync(real)) topologies.push(JSON.parse(readFileSync(real, "utf8")).topology);
const count = Number(countArg ?? "500");
while (topologies.length < count) {
  const n = 1 + Math.floor(rand() * 12);
  const nodes = [...new Set(Array.from({ length: n }, () => NAMES[Math.floor(rand() * NAMES.length)]!))];
  const edges = [];
  const m = Math.floor(rand() * nodes.length * 2);
  for (let i = 0; i < m; i++) {
    const calls = Math.floor(rand() * 500);
    edges.push({
      from: nodes[Math.floor(rand() * nodes.length)]!,
      to: rand() < 0.05 ? "not-a-node" : nodes[Math.floor(rand() * nodes.length)]!, // dangling edge
      calls,
      errors: Math.floor(rand() * rand() * calls * 0.3),
      avgDurationMicros: Math.floor(rand() * rand() * 3_000_000),
    });
  }
  topologies.push({ nodes, edges });
}

const out = topologies.map((t) => {
  const m = dependencyMatrix(t);
  return {
    layout: computeLayout(t.nodes, t.edges),
    health: topologyHealth(t),
    matrix: { rows: m.rows, cols: m.cols, cells: m.cells, medianLatencyMicros: m.medianLatencyMicros },
  };
});
const micros = Array.from({ length: 3000 }, () => Math.floor(rand() * 10 ** (rand() * 8)) + (rand() < 0.3 ? 0.5 : 0));
writeFileSync(`${dir}/view-input.json`, JSON.stringify({ topologies, micros }));
writeFileSync(`${dir}/view-ts.json`, JSON.stringify({ topologies: out, micros: micros.map(formatMicros) }));
console.log(`ts: ${topologies.length} topologies, ${micros.length} durations -> ${dir}`);
