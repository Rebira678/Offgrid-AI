#!/usr/bin/env bash
# Day 23 demo: the AI never overwrites a human.
#
#   scripts/ai-suggests-demo.sh [PAUSE]
#
# Two devices, two databases, one localhost sync link (real offgrid processes).
# While they are apart, a PERSON edits a task on the phone, and an AI finishes a
# job on the laptop that proposes a different change to the same task.
#
# The AI's command is typed by this script with `exec -as-ai`, which runs a
# command exactly the way a model's commands are run (a stand-in for the model:
# no language model is involved). What is under test is the rule, not the model.
#
# It FAILS (exit 1) if the AI ever changes the person's edit, if a stale
# suggestion can be accepted, or if the two devices end up different.
set -euo pipefail
cd "$(dirname "$0")/.."
PAUSE="${1:-0}"
BIN=./bin/offgrid
make build >/dev/null
WORK="${TMPDIR:-/tmp}/offgrid-suggest-$$"; mkdir -p "$WORK"
SRV=""
cleanup() {
  trap - EXIT INT TERM
  [ -z "$SRV" ] || kill "$SRV" 2>/dev/null || true
  wait 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM
LAPTOP="$WORK/laptop.db"; PHONE="$WORK/phone.db"
head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$WORK/key"
say() { printf '\n\033[1m== %s\033[0m\n' "$*"; sleep "$PAUSE"; }
on_laptop() { OFFGRID_NODE=laptop "$@"; }
on_phone()  { OFFGRID_NODE=phone "$@"; }
show() { # DB LABEL: the line for one task
  $BIN exec -db "$1" '{"intent":"list_tasks"}' | grep -E "$3" | sed "s/^ */   $2: /"
}
FAILED=0
fail() { echo "FAIL: $*"; FAILED=1; }

on_phone $BIN sync serve -db "$PHONE" -addr 127.0.0.1:0 -key-file "$WORK/key" >"$WORK/srv.out" 2>"$WORK/srv.err" &
SRV=$!
for _ in $(seq 50); do grep -q 'serving on' "$WORK/srv.out" 2>/dev/null && break; sleep 0.1; done
URL=$(sed -n 's/.*serving on \(http[^ ]*\).*/\1/p' "$WORK/srv.out")
sync_now() { on_laptop $BIN sync once -db "$LAPTOP" -peer "$URL" -key-file "$WORK/key" >/dev/null; }

say "1. Both devices start from the same three tasks"
on_laptop $BIN exec -seed -db "$LAPTOP" '{"intent":"list_tasks"}' >/dev/null
sync_now
show "$LAPTOP" laptop 'Leaking pump'
show "$PHONE" phone 'Leaking pump'

say "2. Apart. The PERSON changes the pump from high to medium on the phone"
on_phone $BIN exec -db "$PHONE" '{"intent":"update_task","task_ref":"pump","priority":"medium"}' | grep -E "ok:"
sleep 1
say "3. Meanwhile the AI (laptop) finishes a job: 'pump can wait' -> low"
on_laptop $BIN exec -as-ai -by qwen2.5:7b -db "$LAPTOP" '{"intent":"update_task","task_ref":"pump","priority":"low"}' | grep -E "suggested|decides"

say "4. Reconnect. The person's edit is still there on both devices"
sync_now
show "$LAPTOP" laptop 'Leaking pump'
show "$PHONE" phone 'Leaking pump'
for db in "$LAPTOP" "$PHONE"; do
  $BIN exec -db "$db" '{"intent":"list_tasks"}' | grep -q 'Leaking pump' && \
  $BIN exec -db "$db" '{"intent":"list_tasks","priority":"medium"}' | grep -q 'Leaking pump' || fail "the AI overwrote the person's edit on $db"
done

say "5. The AI's suggestion is waiting, marked STALE, and cannot be accepted"
$BIN suggestions -db "$PHONE"
ID=$($BIN suggestions -db "$PHONE" | awk 'NR==1{print $1}')
if $BIN suggestions -db "$PHONE" accept "$ID" 2>"$WORK/accept.err"; then fail "a stale suggestion was accepted"; else sed 's/^/   /' "$WORK/accept.err" | head -3; fi

say "6. The person dismisses it; both devices agree"
$BIN suggestions -db "$PHONE" dismiss "$ID"
sync_now
echo "   pending on laptop: $($BIN suggestions -db "$LAPTOP" | head -1)"

say "7. A fresh suggestion that nobody has contradicted CAN be accepted"
on_laptop $BIN exec -as-ai -by qwen2.5:7b -db "$LAPTOP" '{"intent":"update_task","task_ref":"filter","status":"in_progress"}' | grep -E "suggested"
sync_now
ID2=$($BIN suggestions -db "$PHONE" | awk 'NR==1{print $1}')
$BIN suggestions -db "$PHONE" accept "$ID2"
sync_now
show "$LAPTOP" laptop 'Replace filter'

say "8. Verdict"
HA=$($BIN sync summary -db "$LAPTOP" | grep 'state hash'); HB=$($BIN sync summary -db "$PHONE" | grep 'state hash')
echo "laptop $HA"; echo "phone  $HB"
[ "$HA" = "$HB" ] || fail "the devices disagree"
$BIN exec -db "$LAPTOP" '{"intent":"list_tasks","status":"in_progress"}' | grep -q 'Replace filter' || fail "the accepted change did not reach the laptop"
$BIN store -db "$LAPTOP" verify | tail -1
[ "$FAILED" = "0" ] || exit 1
printf '\n\033[1mOK: the AI proposed twice and changed nothing itself. A person edit survived; a person accepted the other.\033[0m\n'
