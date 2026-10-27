#!/usr/bin/env bash
#
# LiveProbe (Go) dev runner.
#
# Modes:
#   ./dev-start.sh             Generate templates, build, and run the server on :4319 with synthetic
#                              traffic from liveprobe-loadgen (no Docker, no Node). Open the UI at
#                              http://localhost:4319.
#   ./dev-start.sh --watch     Same, but rebuild + restart the server on any .go / .templ / static change.
#   ./dev-start.sh --testbed   Real traffic instead: start the Shopwave Docker testbed from
#                              ../live-probing/testbed (its OTel collector exports to host:4319, so
#                              it feeds this server unchanged). Override the path with LIVE_PROBING_TS.
#   ./dev-start.sh --chaos     (with --testbed) inject failure modes (slow catalog, flaky order).
#   ./dev-start.sh --no-load   No traffic generator (point your own app at :4319).
#   ./dev-start.sh --fresh     Keep history in memory (DB_PATH=:memory:) instead of .dev/data.
#
# The server runs in the foreground; Ctrl-C stops it and the synthetic loadgen. The Docker testbed
# keeps running — stop everything with ./dev-stop.sh.
#
# Env: PORT (4319), TRACES_PER_SEC (6), ERROR_RATE (0.12), plus every server env var
# (RETENTION_DAYS, LIVE_WINDOW_MINUTES, MAX_TRACES, MASK_PII, ALERT_WEBHOOK_URL, …).

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

PORT="${PORT:-4319}"
WATCH=0
TESTBED=0
CHAOS=0
LOAD=1
FRESH=0
for arg in "$@"; do
  case "$arg" in
    --watch) WATCH=1 ;;
    --testbed) TESTBED=1 ;;
    --chaos) CHAOS=1 ;;
    --no-load) LOAD=0 ;;
    --fresh) FRESH=1 ;;
    -h | --help) sed -n '2,25p' "$0"; exit 0 ;;
    *) echo "unknown option: $arg (see --help)" >&2; exit 1 ;;
  esac
done
if [[ "$CHAOS" == 1 && "$TESTBED" == 0 ]]; then
  echo "--chaos needs --testbed (the synthetic loadgen has its own ERROR_RATE)" >&2
  exit 1
fi

DEV="$ROOT/.dev"
mkdir -p "$DEV/bin" "$DEV/data" "$DEV/logs"

if curl -fsS -m 1 "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1; then
  echo "Something is already serving :${PORT} (a LiveProbe server?). Stop it first (./dev-stop.sh)." >&2
  exit 1
fi

build() {
  echo "==> Generating templates and building"
  go generate ./internal/ui && go build -o "$DEV/bin/" ./cmd/...
}

# --- server ----------------------------------------------------------------------------------
export PORT
# The dockerized testbed reaches us via host.docker.internal, so bind all interfaces for it;
# otherwise stay on loopback (the ingest/API have no auth).
if [[ "$TESTBED" == 1 ]]; then export HOST="${HOST:-0.0.0.0}"; fi
if [[ "$FRESH" == 1 ]]; then export DB_PATH=":memory:"; else export DB_PATH="${DB_PATH:-$DEV/data/liveprobe.db}"; fi

SERVER_PID=""
LOAD_PID=""
start_server() {
  "$DEV/bin/liveprobe" > >(tee -a "$DEV/logs/server.log") 2>&1 &
  SERVER_PID=$!
  echo "$SERVER_PID" > "$DEV/server.pid"
}
stop_server() {
  if [[ -n "$SERVER_PID" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  rm -f "$DEV/server.pid"
}
cleanup() {
  echo
  echo "==> Stopping"
  stop_server
  if [[ -n "$LOAD_PID" ]]; then kill "$LOAD_PID" 2>/dev/null || true; rm -f "$DEV/loadgen.pid"; fi
  rm -f "$DEV/runner.pid"
  [[ "$TESTBED" == 1 ]] && echo "    (the Docker testbed is still running — ./dev-stop.sh to tear it down)"
  exit 0
}
trap cleanup INT TERM
echo $$ > "$DEV/runner.pid" # so ./dev-stop.sh can stop this loop (its trap then stops the children)

build

# --- traffic ---------------------------------------------------------------------------------
if [[ "$TESTBED" == 1 ]]; then
  TS_ROOT="${LIVE_PROBING_TS:-$ROOT/../live-probing}"
  if [[ ! -f "$TS_ROOT/testbed/docker-compose.yml" ]]; then
    echo "No testbed at $TS_ROOT/testbed (set LIVE_PROBING_TS to the live-probing checkout)." >&2
    exit 1
  fi
  CHAOS_FILE=()
  [[ "$CHAOS" == 1 ]] && CHAOS_FILE=(-f docker-compose.chaos.yml)
  echo "==> Starting the Shopwave testbed (Docker; first run builds images)"
  ( cd "$TS_ROOT/testbed" && docker compose -f docker-compose.yml "${CHAOS_FILE[@]}" up -d --build )
  if [[ "$LOAD" == 1 ]]; then
    ( cd "$TS_ROOT/testbed" && docker compose -f docker-compose.yml "${CHAOS_FILE[@]}" --profile load up -d loadgen )
  fi
  echo "$TS_ROOT" > "$DEV/testbed"
fi

start_server
for _ in $(seq 1 50); do
  curl -fsS -m 1 "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1 && break
  sleep 0.1
done

if [[ "$LOAD" == 1 && "$TESTBED" == 0 ]]; then
  LIVEPROBE_URL="http://127.0.0.1:${PORT}" "$DEV/bin/liveprobe-loadgen" >>"$DEV/logs/loadgen.log" 2>&1 &
  LOAD_PID=$!
  echo "$LOAD_PID" > "$DEV/loadgen.pid"
fi

cat <<EOF

  LiveProbe UI      http://localhost:${PORT}
  OTLP ingest       POST http://localhost:${PORT}/v1/traces   native: /v1/events
  History DB        ${DB_PATH}
  Traffic           $([[ "$TESTBED" == 1 ]] && echo "Shopwave testbed (Docker)$([[ "$CHAOS" == 1 ]] && echo ", chaos on")" || ([[ "$LOAD" == 1 ]] && echo "synthetic loadgen (.dev/logs/loadgen.log)" || echo "none"))
  Logs              .dev/logs/
  Mode              $([[ "$WATCH" == 1 ]] && echo "watch: rebuild + restart on change" || echo "static build")

  Ctrl-C to stop.   ./dev-stop.sh tears everything down.

EOF

if [[ "$WATCH" == 0 ]]; then
  wait "$SERVER_PID"
  exit 0
fi

# --- watch loop: poll source fingerprints (no extra tools needed) ------------------------------
fingerprint() {
  find cmd internal -type f \( -name '*.go' -o -name '*.templ' -o -path '*/static/*' \) ! -name '*_templ.go' \
    -printf '%T@ %p\n' 2>/dev/null | sort | md5sum
}
last="$(fingerprint)"
while true; do
  sleep 1
  now="$(fingerprint)"
  [[ "$now" == "$last" ]] && continue
  last="$now"
  echo "==> Change detected — rebuilding"
  if build; then
    stop_server
    start_server
    echo "==> Restarted (pid $SERVER_PID)"
  else
    echo "==> Build failed — keeping the previous server running"
  fi
done
