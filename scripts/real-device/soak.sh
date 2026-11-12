#!/usr/bin/env bash
# Checkpoint 1 soak: two real devices, signed ops, memory and goroutines watched.
#
#   On the node (the Pi, or the second machine):
#       ./soak.sh node   node.db key [DURATION]
#   On the laptop:
#       ./soak.sh laptop laptop.db key http://NODE:7878 [DURATION]
#
#   DURATION is a Go duration, default 1h. Start the node first, then the laptop,
#   and give them the same DURATION. Each side makes its own device key if it has
#   none ("offgrid identity init"), writes a task or a note every few seconds,
#   syncs, samples its own memory and goroutines every 10 s into a file, and at
#   the end checks that every op is signed and judges the samples.
#
#   OFFGRID=./offgrid-arm64   which binary to run (default: offgrid on PATH)
#   MIN_DURATION=10m          shortest run that gets a verdict (rehearse with MIN_DURATION=10s)
#   WRITE_EVERY=3             seconds between writes
#
# The key file is the SAME shared key on both machines (openssl rand -hex 32).
# Sync is signed, not encrypted: use a network you trust.
set -euo pipefail

OFFGRID="${OFFGRID:-offgrid}"
MIN_DURATION="${MIN_DURATION:-10m}"
WRITE_EVERY="${WRITE_EVERY:-3}"

role="${1:-}"
case "$role" in
node)   db="${2:?usage: soak.sh node DB KEYFILE [DURATION]}";           key="${3:?need KEYFILE}"; dur="${4:-1h}" ;;
laptop) db="${2:?usage: soak.sh laptop DB KEYFILE PEER_URL [DURATION]}"; key="${3:?need KEYFILE}"; peer="${4:?need PEER_URL}"; dur="${5:-1h}" ;;
*) sed -n '2,20p' "$0"; exit 2 ;;
esac

# Go durations ("1h", "90m", "30s") to seconds, for the writer loop.
to_seconds() {
  local d="$1" total=0 n u
  while [[ "$d" =~ ^([0-9]+)([hms])(.*)$ ]]; do
    n="${BASH_REMATCH[1]}"; u="${BASH_REMATCH[2]}"; d="${BASH_REMATCH[3]}"
    case "$u" in h) total=$((total + n * 3600)) ;; m) total=$((total + n * 60)) ;; s) total=$((total + n)) ;; esac
  done
  [ -z "$d" ] && [ "$total" -gt 0 ] || { echo "soak.sh: cannot read the duration" >&2; exit 2; }
  echo "$total"
}
secs="$(to_seconds "$dur")"

stamp="$(date +%Y%m%d-%H%M%S)"
stats="soak-$role-$stamp.stats"
log="soak-$role-$stamp.log"
echo "== Checkpoint 1 soak: $role, $dur. Samples: $stats. Log: $log"
"$OFFGRID" version | head -1 | tee -a "$log"
uname -srm | tee -a "$log"
grep -m1 'model name\|Model' /proc/cpuinfo 2>/dev/null | tee -a "$log" || true
free -m 2>/dev/null | head -2 | tee -a "$log" || true

# Every device needs a key of its own, or its ops are unsigned.
if ! "$OFFGRID" identity show >/dev/null 2>&1; then
  "$OFFGRID" identity init | tee -a "$log"
fi
"$OFFGRID" identity show | tee -a "$log"

writer() {  # every WRITE_EVERY seconds: a task, and a note on the previous one
  local end=$((SECONDS + secs)) i=0
  while [ "$SECONDS" -lt "$end" ]; do
    i=$((i + 1))
    "$OFFGRID" exec -db "$db" "{\"intent\":\"create_task\",\"title\":\"$role soak $i\",\"priority\":\"medium\"}" >/dev/null || echo "write $i failed" >>"$log"
    sleep "$WRITE_EVERY"
  done
}

pids=()
cleanup() { for p in "${pids[@]:-}"; do kill "$p" 2>/dev/null || true; done; }
trap cleanup EXIT

if [ "$role" = node ]; then
  # The node serves; its own writer runs beside it. -create: the database is new.
  "$OFFGRID" exec -db "$db" '{"intent":"create_task","title":"node soak start"}' >/dev/null
  "$OFFGRID" sync serve -db "$db" -addr 0.0.0.0:7878 -key-file "$key" -stats "$stats" -stats-every 10s >>"$log" 2>&1 &
  pids+=($!)
  sleep 2
  writer
  # Leave the server up a little longer so the laptop can finish its last round.
  sleep 20
  kill -INT "${pids[0]}"; wait "${pids[0]}" 2>/dev/null || true
  pids=()
else
  "$OFFGRID" exec -db "$db" '{"intent":"create_task","title":"laptop soak start"}' >/dev/null
  "$OFFGRID" sync run -db "$db" -peer "$peer" -key-file "$key" -every 2s -for "$dur" -stats "$stats" -stats-every 10s >>"$log" 2>&1 &
  runpid=$!
  writer
  wait "$runpid" || echo "sync run exited non-zero (see $log)" | tee -a "$log"
  # One quiet round so nothing written in the last moment is left behind.
  "$OFFGRID" sync once -db "$db" -peer "$peer" -key-file "$key" 2>&1 | tee -a "$log"
fi

echo | tee -a "$log"
echo "== Results ($role)" | tee -a "$log"
"$OFFGRID" sync summary -db "$db" 2>&1 | tee -a "$log"
echo "(compare the state hash line with the other machine's: they must be identical once both are quiet)" | tee -a "$log"
"$OFFGRID" store -db "$db" verify 2>&1 | tee -a "$log"
rc=0
"$OFFGRID" sync stats -min-duration "$MIN_DURATION" "$stats" 2>&1 | tee -a "$log" || rc=$?
echo "Paste the lines above into docs/checkpoint1-report.md. Keep $stats and $log."
exit "$rc"
