#!/usr/bin/env bash
#
# Stop everything ./dev-start.sh started: the LiveProbe server, the synthetic loadgen, and (if it was
# started with --testbed) the Shopwave Docker testbed.
#
# Usage:
#   ./dev-stop.sh           stop, keep data (.dev/data and the testbed's Postgres volume)
#   ./dev-stop.sh --wipe    also delete .dev/data and the testbed's Docker volumes

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"
DEV="$ROOT/.dev"

WIPE=0
for arg in "$@"; do
  case "$arg" in
    --wipe) WIPE=1 ;;
    -h | --help) sed -n '2,9p' "$0"; exit 0 ;;
    *) echo "unknown option: $arg" >&2; exit 1 ;;
  esac
done

# Only processes we started (recorded pid files) — never a pattern match.
stop_pid() {
  local name="$1" file="$DEV/$1.pid"
  if [[ -f "$file" ]] && kill -0 "$(cat "$file")" 2>/dev/null; then
    kill "$(cat "$file")" 2>/dev/null || true
    echo "    $name stopped"
  else
    echo "    $name not running"
  fi
  rm -f "$file"
}

echo "==> Stopping LiveProbe processes"
stop_pid runner # dev-start.sh itself (watch loop); its trap stops server + loadgen
for _ in $(seq 1 30); do [[ -f "$DEV/server.pid" ]] || break; sleep 0.1; done
stop_pid loadgen
stop_pid server

if [[ -f "$DEV/testbed" ]]; then
  TESTBED_DIR="$(cat "$DEV/testbed")"
  echo "==> Tearing down the Docker testbed ($TESTBED_DIR)"
  if [[ "$WIPE" == 1 ]]; then
    ( cd "$TESTBED_DIR" && docker compose --profile load down -v )
  else
    ( cd "$TESTBED_DIR" && docker compose --profile load down )
  fi
  rm -f "$DEV/testbed"
fi

if [[ "$WIPE" == 1 ]]; then
  rm -rf "$DEV/data"
  echo "==> Wiped .dev/data"
fi
echo "==> Done."
