#!/usr/bin/env bash
# Rebuild and restart the dev server on a known port.
#
# Kills whatever holds the port by PID rather than by process name: a stray
# `go run` binary is called "main", so name-based pkill silently misses it and
# you end up testing against a stale server.
set -euo pipefail

PORT="${PORT:-8899}"
CASES="${CASES:-./cases}"
STATE="${STATE:-/tmp/deadlocker-dev/state.db}"
BIN=/tmp/deadlocker
LOG="${LOG:-/tmp/deadlocker.log}"

holders=$(lsof -nP -iTCP:"$PORT" -sTCP:LISTEN -t 2>/dev/null || true)
if [ -n "$holders" ]; then
  echo "killing pid(s) on :$PORT -> $holders"
  kill $holders 2>/dev/null || true
  # Wait for the process to actually exit, not just to release the port.
  # Shutdown removes containers first, and it holds the bbolt file lock the
  # whole time -- starting again too early fails with "state.db: timeout".
  for _ in $(seq 1 60); do
    still=""
    for pid in $holders; do
      kill -0 "$pid" 2>/dev/null && still="$still $pid"
    done
    [ -z "$still" ] && break
    sleep 0.5
  done
  for pid in $holders; do kill -9 "$pid" 2>/dev/null || true; done
  sleep 0.3
fi

GOTOOLCHAIN=auto go build -o "$BIN" ./cmd/deadlocker
"$BIN" -addr "127.0.0.1:$PORT" -cases "$CASES" -state "$STATE" > "$LOG" 2>&1 &

for _ in $(seq 1 40); do
  if grep -q 'listening' "$LOG" 2>/dev/null; then
    echo "serving http://127.0.0.1:$PORT  (cases: $CASES)"
    exit 0
  fi
  if grep -q 'deadlocker:' "$LOG" 2>/dev/null; then
    tail -3 "$LOG"; exit 1
  fi
  sleep 0.25
done
tail -5 "$LOG"; exit 1
