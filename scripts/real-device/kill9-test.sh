#!/usr/bin/env bash
# Day 32. A REHEARSAL of the power-pull test that needs no second machine: kill -9 the
# writer at random moments, then run the same checker. It proves the kit works and that
# offgrid survives a killed process. It does NOT prove the disk survives a power cut:
# kill -9 leaves the operating system running, so the OS still flushes its caches.
# Only pulling the plug on real hardware tests the disk and the SD card or SSD.
#
#   scripts/real-device/kill9-test.sh [ROUNDS]        (default 10; uses ./bin/offgrid, or OFFGRID_BIN)
set -u
ROUNDS="${1:-10}"
here="$(cd "$(dirname "$0")" && pwd)"
export OFFGRID_BIN="${OFFGRID_BIN:-$here/../../bin/offgrid}"
[ -x "$OFFGRID_BIN" ] || { echo "no binary at $OFFGRID_BIN (run: make build)"; exit 2; }

tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
DB="$tmp/tasks.db"; ACKS="$tmp/acks.log"; : > "$ACKS"

for r in $(seq 1 "$ROUNDS"); do
  # setsid: its own process group, so kill -9 takes the writer AND any offgrid in flight.
  setsid "$here/powerpull-writer.sh" "$DB" 2 >> "$ACKS" 2>/dev/null &
  pgid=$!
  sleep "0.$((RANDOM % 9 + 3))"            # 0.3 to 1.1 s: a random moment
  kill -9 -- "-$pgid" 2>/dev/null
  wait "$pgid" 2>/dev/null
  echo "round $r: killed; $(grep -c '^ACK ' "$ACKS") ACKs so far"
done
echo
"$here/powerpull-check.sh" "$DB" "$ACKS"
