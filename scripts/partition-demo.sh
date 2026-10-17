#!/usr/bin/env bash
# Day 11 demo: a switch to break your own network on purpose.
#
#   scripts/partition-demo.sh [UP DOWN]     seconds of network up / partitioned (default 4 4)
#
# A tiny local server plays "the internet". offgrid watches it. A timer flips a
# partition on and off by rewriting a control file, and you watch ONLINE ->
# OFFLINE -> ONLINE. Nothing touches your real WiFi or the real internet, so
# it is safe to record, and it behaves the same way every time.
#
# The script FAILS (exit 1) if offgrid does not notice both flips, so it is
# also a check, not only a show.
set -euo pipefail
cd "$(dirname "$0")/.."

UP="${1:-4}"
DOWN="${2:-4}"
BIN=./bin/offgrid
make build >/dev/null   # always rebuild: a stale binary would demo old behaviour

WORK="${TMPDIR:-/tmp}/offgrid-partition-$$"
mkdir -p "$WORK"
SERVER_PID=""
cleanup() { [ -z "$SERVER_PID" ] || kill "$SERVER_PID" 2>/dev/null || true; rm -rf "$WORK"; }
trap cleanup EXIT
export OFFGRID_FAULT="$WORK/fault"

TOTAL=$((UP + DOWN + UP + 3))

printf '\033[1m== the "internet": a local server that answers 204\033[0m\n'
$BIN fault serve -for "$((TOTAL + 10))s" >"$WORK/server.out" &
SERVER_PID=$!
for _ in $(seq 1 50); do grep -q 'listening on' "$WORK/server.out" 2>/dev/null && break; sleep 0.1; done
URL="$(sed -n 's/^listening on //p' "$WORK/server.out")"
[ -n "$URL" ] || { echo "server did not start"; exit 1; }
echo "$URL"

printf '\n\033[1m== the switch: flip the partition on a timer (up %ss, down %ss, up again)\033[0m\n' "$UP" "$DOWN"
(
  sleep "$UP";   $BIN fault set partition
  sleep "$DOWN"; $BIN fault set ok
) &
FLIPPER=$!

printf '\n\033[1m== offgrid watches the network (probes every 300 ms)\033[0m\n'
$BIN net -url "$URL" -interval 300ms -timeout 1s -slow 500ms -for "${TOTAL}s" \
  -fault-file "$OFFGRID_FAULT" | tee "$WORK/net.out" | grep --line-buffered -E 'FAULT|==>|stopped|fault injection'
wait "$FLIPPER"

printf '\n'
if grep -q -- '-> OFFLINE' "$WORK/net.out" && grep -q 'OFFLINE -> ONLINE' "$WORK/net.out"; then
  printf '\033[1mOK: offgrid saw the partition start and end.\033[0m\n'
else
  echo "FAIL: offgrid did not notice both flips" >&2
  exit 1
fi
