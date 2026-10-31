#!/usr/bin/env bash
# Day 26 demo: the dashboard, on real data.
#
#   scripts/web-demo.sh [SECONDS]      SECONDS = how long it stays up (default: until Ctrl-C)
#   CHECK=1 scripts/web-demo.sh        no browser needed: fetch the page's data and FAIL (exit 1) if it is wrong
#
# It builds a laptop and a phone that really sync (two databases, a localhost HMAC link), lets
# a person edit one task on the phone while an AI suggests a different change to it from the
# laptop, queues requests and runs the real drain worker behind a scripted outage, then
# starts `offgrid web`. Open the address it prints.
#
# THE NETWORK IS SIMULATED (a local 204 server behind a scripted outage) and the model is a
# STAND-IN with canned answers. The tasks, ops, sync, queue, suggestions and dashboard are real.
set -euo pipefail
cd "$(dirname "$0")/.."
SECS="${1:-0}"
BIN=./bin/offgrid
make build >/dev/null
WORK="${TMPDIR:-/tmp}/offgrid-web-$$"; mkdir -p "$WORK"
PIDS=()
cleanup() { for p in "${PIDS[@]:-}"; do [ -z "$p" ] || kill "$p" 2>/dev/null || true; done; wait 2>/dev/null || true; rm -rf "$WORK"; }
trap cleanup EXIT
LAPTOP="$WORK/tasks.db"; PHONE="$WORK/phone.db"
export OFFGRID_DB="$LAPTOP" OFFGRID_QUEUE="$WORK/queue.db"
head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$WORK/key"
on_laptop() { OFFGRID_NODE=laptop "$@"; }
on_phone()  { OFFGRID_NODE=phone "$@"; }

# the phone, as a sync server on a free port
on_phone $BIN sync serve -db "$PHONE" -addr 127.0.0.1:0 -key-file "$WORK/key" >"$WORK/srv.out" 2>"$WORK/srv.err" &
PIDS+=($!)
for _ in $(seq 50); do grep -q 'serving on' "$WORK/srv.out" 2>/dev/null && break; sleep 0.1; done
PEER=$(sed -n 's/.*serving on \(http[^ ]*\).*/\1/p' "$WORK/srv.out")
[ -n "$PEER" ] || { echo "phone did not start"; exit 1; }
sync_now() { on_laptop $BIN sync once -db "$LAPTOP" -peer "$PEER" -key-file "$WORK/key" >/dev/null; }

# same three tasks on both; then apart
on_laptop $BIN exec -seed -db "$LAPTOP" '{"intent":"list_tasks"}' >/dev/null
sync_now
on_phone $BIN exec -db "$PHONE" '{"intent":"update_task","task_ref":"pump","priority":"medium"}' >/dev/null
on_phone $BIN exec -db "$PHONE" '{"intent":"create_task","title":"Order spare seals","site":"4","priority":"low"}' >/dev/null
sleep 1
# the AI, on the laptop: one suggestion the person will contradict, one nobody will
on_laptop $BIN exec -as-ai -by qwen2.5:7b -db "$LAPTOP" '{"intent":"update_task","task_ref":"pump","priority":"low"}' >/dev/null
on_laptop $BIN exec -as-ai -by qwen2.5:7b -db "$LAPTOP" '{"intent":"update_task","task_ref":"filter","status":"in_progress"}' >/dev/null
sync_now

# the "internet" and the model stand-in
$BIN fault serve -for 600s >"$WORK/serve.out" &
PIDS+=($!)
$BIN fault model -delay 800ms -for 600s >"$WORK/model.out" &
PIDS+=($!)
for _ in $(seq 50); do grep -q 'listening on' "$WORK/serve.out" 2>/dev/null && grep -q 'listening on' "$WORK/model.out" 2>/dev/null && break; sleep 0.1; done
PROBE="$(sed -n 's/.*listening on \(http[^ ]*\)$/\1/p' "$WORK/serve.out" | head -1)"
MODEL="$(sed -n 's/.*listening on \(http[^ ]*\) .*/\1/p' "$WORK/model.out")"
[ -n "$PROBE" ] && [ -n "$MODEL" ] || { echo "stand-ins did not start"; exit 1; }

printf '%s\n' "list open tasks" "what should I prioritize this week?" "why does the fuel line keep cracking" \
  "please jot down that pump three smells of gas" "compare site 2 and site 4 and recommend where to send the crew" \
  | on_laptop $BIN agent -no-model -route -net offline >/dev/null
on_laptop $BIN drain -net auto -url "$PROBE" -probe-interval 300ms -host "$MODEL" -deep-host "$MODEL" \
  -fault-schedule partition:4s,ok:6s,partition:5s,ok:60s -fault-mode refuse \
  -concurrency 2 -lease 5s -poll 200ms >"$WORK/drain.out" 2>&1 &
PIDS+=($!)

on_laptop $BIN web -db "$LAPTOP" -listen 127.0.0.1:${PORT:-8787} >"$WORK/web.out" 2>"$WORK/web.err" &
PIDS+=($!)
for _ in $(seq 50); do grep -q 'dashboard on' "$WORK/web.out" 2>/dev/null && break; sleep 0.1; done
URL=$(sed -n 's/.*dashboard on \(http[^ ]*\).*/\1/p' "$WORK/web.out")
[ -n "$URL" ] || { echo "dashboard did not start:"; cat "$WORK/web.err"; exit 1; }

if [ "${CHECK:-0}" = "1" ]; then
  sleep 3
  S=$(curl -fsS "$URL/api/state")
  fail=0
  check() { if printf '%s' "$S" | grep -Eq "$2"; then printf '  ok   %s\n' "$1"; else printf '  FAIL %s\n' "$1"; fail=1; fi; }
  check "both replicas are listed"          '"node":"phone"'
  check "the phone work arrived as merges"  '"merges":\[\{[^]]*"node":"phone"'
  check "two AI suggestions are waiting"    '"suggestions":\[\{.*\},\{.*\}\]'
  check "the person-contradicted one is stale" '"stale":true'
  check "the other one is not stale"        '"stale":false'
  check "the queue is read"                 '"queue":\{'
  check "the connection is known"           '"known":true'
  curl -fsS "$URL/" | grep -q '<title>Offgrid</title>' && echo "  ok   the page is served" || { echo "  FAIL page"; fail=1; }
  [ "$fail" = 0 ] && echo "PASS" || { echo "FAIL"; exit 1; }
  exit 0
fi

echo "dashboard: $URL"
if [ "$SECS" != "0" ]; then sleep "$SECS"; else echo "(Ctrl-C to stop)"; wait; fi
