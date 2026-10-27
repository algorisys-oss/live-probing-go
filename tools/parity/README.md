# Differential parity harness

Runs real recorded traces through the **original TS core** and the **Go port** and diffs every
projection (coerce, OTLP normalize, mask, window order/eviction, assembly, sequence, layout incl.
N+1 groups, critical path, topology, RED, alerts, flamegraphs, Mermaid, latency buckets).

```bash
P=$(mktemp -d)
npx -y tsx@4.19.2 tools/parity/core-parity.ts "$P" ../live-probing/packages/server/data/liveprobe.db
LIVEPROBE_PARITY_DIR="$P" go test -count=1 -run Parity ./internal/core/
node tools/parity/compare.mjs "$P"     # → PARITY OK
```

Optional third arg to `core-parity.ts` is the sampling modulus (default 211 → ~1.5k traces).
Requires Node ≥ 22 (`node:sqlite`) and a sibling `../live-probing` checkout. The server
differential below reuses the `input.json` / `otlp.json` this step writes.

## Server differential

Runs the TS server and the Go server side by side on identical traffic and diffs every API route
plus the `/ws` snapshot. The TS server needs its deps; install them in an isolated copy rather than
in `../live-probing`:

```bash
C=$(mktemp -d) && (cd ../live-probing && tar --exclude=node_modules --exclude=dist --exclude=public --exclude=data \
  -cf - package.json package-lock.json packages) | tar -xf - -C "$C"
(cd "$C" && PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1 npm ci --ignore-scripts)
(cd "$C" && PORT=4441 DB_PATH=:memory: npx tsx packages/server/src/index.ts &)
go build -o liveprobe ./cmd/liveprobe && PORT=4442 DB_PATH=:memory: ./liveprobe &
node tools/parity/server-parity.mjs "$P" http://localhost:4441 http://localhost:4442   # → SERVER PARITY OK
```

- `NO_HOSTILE=1` drops the far-future event so the live window stays populated (otherwise it
  correctly evicts all older traces in both servers).
- `SKIP_FEED=1` compares two servers opened on **copies** of an existing history DB (start both
  with `RETENTION_DAYS=0`, or startup retention prunes old days).

## Adapter fuzz differential

A seeded corpus (the TS test fixtures + mutations: dropped keys, type-swapped values, odd number and
date strings) through every collector adapter on both sides. Uses the scratch TS install above.

```bash
"$C"/node_modules/.bin/tsx tools/parity/adapter-parity.mts "$P" "$C" 20000
LIVEPROBE_PARITY_DIR="$P" go test -count=1 -run AdapterParity ./internal/collector/
node tools/parity/compare.mjs "$P" adapter-ts.json adapter-go.json
```

Expected: `adapter-id-1=ok algo-instrumentation=ok`; `liveprobe-native` shows only the documented
deltas #1 (fractional micros truncate) and #4 (non-scalar attributes dropped). `compare.mjs` compares
`startTime`/`duration` clamped to [0, 8.64e15] — the value ingest will actually use.
