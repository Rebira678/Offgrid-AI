#!/usr/bin/env bash
# Day 12 demo: kill -9 in the middle of a drain, restart, nothing lost.
#
#   scripts/drain-demo.sh [PAUSE]       PAUSE = seconds between steps (default 0)
#
# STANDIN=1 (default)  a stand-in model server gives canned answers, so this
#                      runs anywhere. The answers are NOT from a real model.
# STANDIN=0            use your real Ollama (OLLAMA_HOST, model $OFFGRID_MODEL).
#                      Jobs then take as long as the model takes.
#
# It FAILS (exit 1) if any job is lost, duplicated, or left unfinished.
# Everything lives in a throwaway folder that is deleted at the end.
set -euo pipefail
cd "$(dirname "$0")/.."

PAUSE="${1:-0}"
STANDIN="${STANDIN:-1}"
JOBS=6
BIN=./bin/offgrid
make build >/dev/null   # always rebuild: a stale binary would demo old behaviour

WORK="${TMPDIR:-/tmp}/offgrid-drain-$$"
mkdir -p "$WORK"
MODEL_PID=""; DRAIN_PID=""
cleanup() {
  [ -z "$DRAIN_PID" ] || kill -9 "$DRAIN_PID" 2>/dev/null || true
  [ -z "$MODEL_PID" ] || kill "$MODEL_PID" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT
export OFFGRID_DB="$WORK/tasks.db"
export OFFGRID_QUEUE="$WORK/queue.db"

say() { printf '\n\033[1m== %s\033[0m\n' "$*"; sleep "$PAUSE"; }

HOSTFLAG=()
if [ "$STANDIN" = "1" ]; then
  $BIN fault model -delay 1200ms -for 120s >"$WORK/model.out" &
  MODEL_PID=$!
  for _ in $(seq 1 50); do grep -q 'listening on' "$WORK/model.out" 2>/dev/null && break; sleep 0.1; done
  URL="$(sed -n 's/.*listening on \(http[^ ]*\) .*/\1/p' "$WORK/model.out")"
  [ -n "$URL" ] || { echo "stand-in model did not start"; exit 1; }
  HOSTFLAG=(-host "$URL")
  echo "(STAND-IN model at $URL: canned answers, not a real language model)"
fi

say "1. WiFi off. Hard requests are saved, not guessed at"
$BIN agent -seed -no-model "list open tasks" >/dev/null     # three starting tasks
QUESTIONS=(
  "summarize this week's incidents and suggest priorities"
  "what should I prioritize this week?"
  "why does the fuel line keep cracking"
  "compare site 2 and site 4 and recommend where to send the crew"
  "analyze the open tasks and advise what to do first"
  "which tasks are getting worse and what is the root cause"
)
printf '%s\n' "${QUESTIONS[@]}" | $BIN agent -no-model -route -net offline | grep -E 'deferred|ROUTES' | cut -c1-110
$BIN queue stats

say "2. The network is back. Start draining (2 at a time, in order)"
$BIN drain -net online -once=false "${HOSTFLAG[@]}" -concurrency 2 -lease 3s -poll 200ms >"$WORK/drain1.out" 2>&1 &
DRAIN_PID=$!
# wait until the first job has finished and others are in flight
for _ in $(seq 1 200); do
  [ "$($BIN queue stats | awk '/^done/{print $2}')" -ge 1 ] && \
  [ "$($BIN queue stats | awk '/^running/{print $2}')" -ge 1 ] && break
  sleep 0.1
done
cat "$WORK/drain1.out"

say "3. kill -9, with jobs in flight"
echo "pid $DRAIN_PID: kill -9"
kill -9 "$DRAIN_PID"; wait "$DRAIN_PID" 2>/dev/null || true; DRAIN_PID=""
$BIN queue list | cut -c1-100
$BIN queue stats

say "4. Restart. The dead worker's leases expire and the jobs are picked up"
$BIN drain -net online -once "${HOSTFLAG[@]}" -concurrency 2 -lease 3s -poll 200ms

say "5. Verdict"
FAILED=0
STATS="$($BIN queue stats)"; echo "$STATS"
for st in queued running dead; do
  n="$(echo "$STATS" | awk -v s="$st" '$1==s{print $2}')"
  [ "$n" = "0" ] || { echo "FAIL: $n job(s) $st"; FAILED=1; }
done
[ "$(echo "$STATS" | awk '/^done/{print $2}')" = "$JOBS" ] || { echo "FAIL: not all $JOBS jobs are done"; FAILED=1; }
for i in $(seq 1 "$JOBS"); do
  if ! $BIN queue result "$i" >"$WORK/r$i" 2>/dev/null || [ ! -s "$WORK/r$i" ]; then echo "FAIL: job $i has no result"; FAILED=1; fi
done
printf '%s\n' "$($BIN queue list | awk 'NR>1{print $3}' | sort | uniq -c | sed 's/^ *//')" | sed 's/^/tries per job: /'
echo "(a job that shows 2/5 tries was the one running when the process was killed)"
[ "$FAILED" = "0" ] || exit 1
printf '\n\033[1mOK: %s jobs saved, one process killed with -9, all %s finished exactly once. Nothing lost, nothing duplicated.\033[0m\n' "$JOBS" "$JOBS"
