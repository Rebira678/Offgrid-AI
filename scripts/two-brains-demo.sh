#!/usr/bin/env bash
# Day 22 demo: two brains, one agent.
#
#   scripts/two-brains-demo.sh [PAUSE]
#
# A hard question is saved while offline. The slow brain (a big local model)
# answers only when the laptop is quiet, writes the answer into the task
# database as an op, and a second device receives it by syncing.
#
# STANDIN=1 (default)  a stand-in model server gives canned answers: the wiring is
#                      real, the words are NOT from a language model.
# STANDIN=0            your real Ollama (OLLAMA_HOST) with DEEP_MODEL (default qwen2.5:7b).
# BURN=1 (default)     starts CPU burners for a few seconds, so you SEE the idle gate wait.
#
# It FAILS (exit 1) if the answer is missing on either device, or if the two
# devices disagree afterwards. Everything lives in a throwaway folder.
set -euo pipefail
cd "$(dirname "$0")/.."
PAUSE="${1:-0}"; STANDIN="${STANDIN:-1}"; BURN="${BURN:-1}"
DEEP_MODEL="${DEEP_MODEL:-qwen2.5:7b}"
BIN=./bin/offgrid
make build >/dev/null
WORK="${TMPDIR:-/tmp}/offgrid-brains-$$"; mkdir -p "$WORK"
PIDS=()
cleanup() { for p in "${PIDS[@]:-}"; do [ -z "$p" ] || kill "$p" 2>/dev/null || true; done; rm -rf "$WORK"; }
trap cleanup EXIT
say() { printf '\n\033[1m== %s\033[0m\n' "$*"; sleep "$PAUSE"; }
export OFFGRID_QUEUE="$WORK/queue.db"
LAPTOP="$WORK/laptop.db"; PHONE="$WORK/phone.db"
head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$WORK/key"

HOSTFLAG=()
if [ "$STANDIN" = "1" ]; then
  $BIN fault model -delay 800ms -for 120s >"$WORK/model.out" &
  PIDS+=($!)
  for _ in $(seq 1 50); do grep -q 'listening on' "$WORK/model.out" 2>/dev/null && break; sleep 0.1; done
  URL="$(sed -n 's/.*listening on \(http[^ ]*\) .*/\1/p' "$WORK/model.out")"
  [ -n "$URL" ] || { echo "stand-in model did not start"; exit 1; }
  HOSTFLAG=(-host "$URL")
  echo "(STAND-IN model at $URL: canned answers, not a real language model)"
fi

say "1. Offline. A hard question is saved for later, not guessed at"
OFFGRID_NODE=laptop $BIN agent -seed -no-model -db "$LAPTOP" "list open tasks" >/dev/null
echo "summarize this week's incidents and suggest priorities" | OFFGRID_NODE=laptop $BIN agent -no-model -route -net offline -db "$LAPTOP" | grep -E 'deferred' | cut -c1-110

say "2. The slow brain waits while the laptop is busy, then answers"
if [ "$BURN" = "1" ]; then
  for _ in $(seq "$(nproc)"); do ( timeout 6 sh -c 'while :; do :; done' ) & PIDS+=($!); done
fi
OFFGRID_NODE=laptop $BIN drain -db "$LAPTOP" -net offline -once "${HOSTFLAG[@]}" -deep-model "$DEEP_MODEL" -poll 300ms -idle-max-busy 0.5 | grep -vE '^$'

say "3. The answer is an op in the task database"
OFFGRID_NODE=laptop $BIN answers -db "$LAPTOP"
OFFGRID_NODE=laptop $BIN store -db "$LAPTOP" -n 2 ops | tail -3 | cut -c1-120

say "4. Sync to the phone: the answer travels like a task edit"
$BIN sync serve -db "$PHONE" -addr 127.0.0.1:0 -key-file "$WORK/key" >"$WORK/srv.out" 2>"$WORK/srv.err" &
PIDS+=($!)
for _ in $(seq 50); do grep -q 'serving on' "$WORK/srv.out" 2>/dev/null && break; sleep 0.1; done
SYNCURL=$(sed -n 's/.*serving on \(http[^ ]*\).*/\1/p' "$WORK/srv.out")
OFFGRID_NODE=laptop $BIN sync once -db "$LAPTOP" -peer "$SYNCURL" -key-file "$WORK/key" | tail -2
echo "--- on the phone:"
$BIN answers -db "$PHONE"

say "5. Verdict"
FAILED=0
[ "$($BIN answers -db "$LAPTOP" -json | grep -c '"key"')" = "1" ] || { echo "FAIL: laptop should hold exactly one answer"; FAILED=1; }
[ "$($BIN answers -db "$PHONE" -json | grep -c '"key"')" = "1" ] || { echo "FAIL: phone did not receive the answer"; FAILED=1; }
HA=$($BIN sync summary -db "$LAPTOP" | grep 'state hash'); HB=$($BIN sync summary -db "$PHONE" | grep 'state hash')
echo "laptop $HA"; echo "phone  $HB"
[ "$HA" = "$HB" ] || { echo "FAIL: the devices disagree"; FAILED=1; }
$BIN store -db "$LAPTOP" verify | tail -1
[ "$FAILED" = "0" ] || exit 1
printf '\n\033[1mOK: one question, one answer, on both devices, identical state.\033[0m\n'
