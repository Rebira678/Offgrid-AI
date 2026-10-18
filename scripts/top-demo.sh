#!/usr/bin/env bash
# Day 13 demo: watch the whole system on one screen while the network comes and goes.
#
#   scripts/top-demo.sh [SECONDS]      SECONDS = how long the screen stays up (default 26)
#
# In a terminal it shows the LIVE screen (`offgrid top`): record this for the post.
# Piped or in CI (or CHECK=1) it takes a few plain-text snapshots instead and
# FAILS (exit 1) if the screen does not tell the truth about what happened.
#
# What you will see, in order:
#   - requests saved while offline; jobs that need only this laptop (interpret) run at once,
#     jobs that need another machine (deep_think) wait: the queue depth shows them
#   - the (simulated) network comes back: the waiting jobs run
#   - it goes away again, more requests arrive and wait, it returns, they run
#
# THE NETWORK IS SIMULATED (a local 204 server behind a scripted outage) and the model is
# a STAND-IN with canned answers (STANDIN=1, the default). Nothing here is your real WiFi
# or a real language model. STANDIN=0 uses your real Ollama for the model only.
set -euo pipefail
cd "$(dirname "$0")/.."

SECS="${1:-26}"
STANDIN="${STANDIN:-1}"
BIN=./bin/offgrid
make build >/dev/null   # always rebuild: a stale binary would demo old behaviour

WORK="${TMPDIR:-/tmp}/offgrid-top-$$"
mkdir -p "$WORK"
PIDS=()
cleanup() {
  for p in "${PIDS[@]:-}"; do [ -z "$p" ] || kill "$p" 2>/dev/null || true; done
  wait 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT
export OFFGRID_DB="$WORK/tasks.db"
export OFFGRID_QUEUE="$WORK/queue.db"

# the "internet": a local server that answers 204 (nothing leaves this machine)
$BIN fault serve -for $((SECS + 60))s >"$WORK/serve.out" &
PIDS+=($!)
HOSTFLAG=()
if [ "$STANDIN" = "1" ]; then
  $BIN fault model -delay 1200ms -for $((SECS + 60))s >"$WORK/model.out" &
  PIDS+=($!)
fi
for _ in $(seq 1 50); do
  grep -q 'listening on' "$WORK/serve.out" 2>/dev/null && { [ "$STANDIN" != 1 ] || grep -q 'listening on' "$WORK/model.out" 2>/dev/null; } && break
  sleep 0.1
done
PROBE="$(sed -n 's/.*listening on \(http[^ ]*\)$/\1/p' "$WORK/serve.out" | head -1)"
[ -n "$PROBE" ] || { echo "probe server did not start"; exit 1; }
if [ "$STANDIN" = "1" ]; then
  MODEL="$(sed -n 's/.*listening on \(http[^ ]*\) .*/\1/p' "$WORK/model.out")"
  [ -n "$MODEL" ] || { echo "stand-in model did not start"; exit 1; }
  HOSTFLAG=(-host "$MODEL" -deep-host "$MODEL")
else
  HOSTFLAG=(-deep-host "${OLLAMA_HOST:-http://127.0.0.1:11434}")
fi

# Phase 1: offline, a few requests. Simple ones are answered, hard ones are saved.
$BIN agent -seed -no-model "list open tasks" >/dev/null
printf '%s\n' \
  "list open tasks" \
  "summarize this week's incidents and suggest priorities" \
  "what should I prioritize this week?" \
  "why does the fuel line keep cracking" \
  "please jot down that pump three smells of gas" \
  "compare site 2 and site 4 and recommend where to send the crew" \
  | $BIN agent -no-model -route -net offline >/dev/null

# the drain worker, behind a scripted outage: 5 s down, 7 s up, 6 s down, then up
$BIN drain -net auto -url "$PROBE" -probe-interval 300ms "${HOSTFLAG[@]}" \
  -fault-schedule partition:5s,ok:7s,partition:6s,ok:60s -fault-mode refuse \
  -concurrency 2 -lease 5s -poll 200ms -slog "$WORK/drain.log" -slog-level debug >"$WORK/drain.out" 2>&1 &
PIDS+=($!)

# Phase 2: while the network is down the second time, two more requests arrive
(
  sleep 14
  printf '%s\n' "which tasks are getting worse and what is the root cause" "analyze the open tasks and advise what to do first" \
    | $BIN agent -no-model -route -net offline >/dev/null
) &
PIDS+=($!)

LIVE=0
if [ -t 1 ] && [ "${CHECK:-0}" != "1" ]; then LIVE=1; fi

if [ "$LIVE" = "1" ]; then
  timeout "$SECS" $BIN top || true      # timeout ends the screen; top restores the terminal itself
  echo
  $BIN top -once
  exit 0
fi

# ---- plain mode: snapshots and a verdict
snap() { printf '\n--- screen at +%ss ---\n' "$1"; $BIN top -once -color never; }
sleep 3;  snap 3  | tee "$WORK/s3"   >/dev/null
sleep 5;  snap 8  | tee "$WORK/s8"   >/dev/null
sleep 8;  snap 16 | tee "$WORK/s16"  >/dev/null
sleep $((SECS - 16)); snap "$SECS" | tee "$WORK/sEnd" >/dev/null
cat "$WORK/s3" "$WORK/s16" "$WORK/sEnd"

fail=0
check() { # NAME FILE PATTERN
  if grep -Eq "$3" "$2"; then printf '  ok   %s\n' "$1"; else printf '  FAIL %s (no match for: %s)\n' "$1" "$3"; fail=1; fi
}
printf '\n== verdict\n'
check "offline at the start"                     "$WORK/s3"   'OFFLINE'
check "deep_think jobs are waiting for the network" "$WORK/s3" 'queued [1-9]'
check "the interpret jobs ran while offline"     "$WORK/s3"   'interpret +[0-9]+ +[1-9]'
check "the outage is in the connection history"  "$WORK/sEnd" 'DEGRADED → OFFLINE|ONLINE → DEGRADED'
check "the recovery is in the connection history" "$WORK/sEnd" 'OFFLINE → ONLINE|DEGRADED → ONLINE'
check "tiers show deferred requests"             "$WORK/sEnd" 'deferred +[0-9]+'
check "nothing is left in the queue"             "$WORK/sEnd" 'queued 0 +running 0'
check "nothing died"                             "$WORK/sEnd" 'dead 0'
check "the drain shows as running at the end"    "$WORK/sEnd" '● drain'
[ "$fail" = 0 ] && echo "PASS" || { echo "FAIL"; exit 1; }
