#!/usr/bin/env bash
# Day 20 / Checkpoint 2: two replicas, the network goes away, both edit, it comes
# back, and their state hashes are compared. Exits non-zero if they differ or if
# the merged state is not what the rules say it must be.
#
#   replica A  = `offgrid sync serve`            (database a.db)
#   replica B  = `offgrid sync run -peer A`      (database b.db), behind the chaos
#                switch: `offgrid fault set partition` is its "WiFi off".
#
# Everything runs on localhost, in a temp folder that is deleted at the end. The
# partition is SIMULATED by the fault-injecting transport, not a real WiFi drop.
set -euo pipefail
cd "$(dirname "$0")/.."
BIN=${BIN:-bin/offgrid}
[ -x "$BIN" ] || { echo "build first: make build"; exit 1; }
PAUSE=${PAUSE:-1}          # seconds between narrated steps; PAUSE=0 for tests
D=$(mktemp -d)
KEY="$D/key"; FAULT="$D/fault"; A="$D/a.db"; B="$D/b.db"
SRV=""; RUNNER=""
cleanup() { [ -n "$RUNNER" ] && kill "$RUNNER" 2>/dev/null || true; [ -n "$SRV" ] && kill "$SRV" 2>/dev/null || true; rm -rf "$D"; }
trap cleanup EXIT

a() { OFFGRID_NODE=node-a "$BIN" "$@"; }
b() { OFFGRID_NODE=node-b "$BIN" "$@"; }
say() { printf '\n\033[1m== %s\033[0m\n' "$*"; sleep "$PAUSE"; }
hash_of() { "$BIN" sync summary -db "$1" | sed -n 's/^state hash: *//p'; }
list_of() { OFFGRID_NODE=$2 "$BIN" task -db "$1" list | grep -v '^state hash'; }

head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$KEY"
echo ok > "$FAULT"

say "A has three tasks (one with a note). B is empty."
a exec -db "$A" '{"intent":"create_task","title":"Leaking pump","site":"4","priority":"high"}' >/dev/null
a exec -db "$A" '{"intent":"create_task","title":"Replace filter","priority":"low"}' >/dev/null
a exec -db "$A" '{"intent":"create_task","title":"Check valve","priority":"medium"}' >/dev/null
a exec -db "$A" '{"intent":"add_note","task_ref":"T1","note":"seal cracked"}' >/dev/null

say "A serves; B syncs with A every 300 ms through the chaos switch"
a sync serve -db "$A" -addr 127.0.0.1:0 -key-file "$KEY" > "$D/serve.out" 2> "$D/serve.err" &
SRV=$!
for _ in $(seq 100); do grep -q 'serving on' "$D/serve.out" 2>/dev/null && break; sleep 0.1; done
URL=$(sed -n 's/.*serving on \(http[^ ]*\).*/\1/p' "$D/serve.out")
[ -n "$URL" ] || { echo "the server did not start"; cat "$D/serve.err"; exit 1; }
b sync run -db "$B" -peer "$URL" -key-file "$KEY" -every 300ms -name B -fault-file "$FAULT" > "$D/b.log" 2>&1 &
RUNNER=$!

waitfor() {  # waitfor "description" command... : poll up to 30 s
  local what=$1; shift
  for _ in $(seq 150); do "$@" && return 0; sleep 0.2; done
  echo "timed out waiting for: $what"; cat "$D/b.log"; return 1
}
same() { [ "$(hash_of "$A")" = "$(hash_of "$B")" ]; }
waitfor "B to catch up" same
echo "A and B hold the same state: $(hash_of "$A" | cut -c1-12)"

say "WiFi OFF (offgrid fault set partition)"
"$BIN" fault set -file "$FAULT" partition
waitfor "B to notice it is offline" grep -q OFFLINE "$D/b.log"

say "While offline, A edits"
a exec -db "$A" '{"intent":"update_task","task_ref":"T1","status":"done"}' >/dev/null
a exec -db "$A" '{"intent":"add_note","task_ref":"T1","note":"replaced the seal"}' >/dev/null
a task -db "$A" delete T2
a task -db "$A" delete T3
a exec -db "$A" '{"intent":"create_task","title":"Fuel line","priority":"medium"}' >/dev/null
sleep 1.1   # B's edits are later than A's in time

say "While offline, B edits the SAME task and more"
b exec -db "$B" '{"intent":"update_task","task_ref":"T1","status":"in_progress","priority":"low"}' >/dev/null
b exec -db "$B" '{"intent":"add_note","task_ref":"T1","note":"waiting for parts"}' >/dev/null
b task -db "$B" remove-note T1 1
b exec -db "$B" '{"intent":"update_task","task_ref":"T2","priority":"high"}' >/dev/null
b exec -db "$B" '{"intent":"create_task","title":"Order gasket","priority":"high"}' >/dev/null

echo; echo "--- A (offline edits) ---";  list_of "$A" node-a
echo; echo "--- B (offline edits) ---";  list_of "$B" node-b
echo; echo "state hash A: $(hash_of "$A")"; echo "state hash B: $(hash_of "$B")"
if same; then echo "BUG: the replicas should differ here"; exit 1; fi
echo "(they differ, as they should)"

say "WiFi ON (offgrid fault set ok)"
"$BIN" fault set -file "$FAULT" ok
waitfor "the replicas to converge" same
sleep 0.5

echo; echo "--- A after reconnecting ---"; list_of "$A" node-a
echo; echo "--- B after reconnecting ---"; list_of "$B" node-b
HA=$(hash_of "$A"); HB=$(hash_of "$B")
echo; echo "state hash A: $HA"; echo "state hash B: $HB"
[ "$HA" = "$HB" ] || { echo "FAIL: the state hashes differ"; exit 1; }
[ "$(list_of "$A" node-a)" = "$(list_of "$B" node-b)" ] || { echo "FAIL: the lists differ"; exit 1; }

say "Checking the merge did what the rules say"
L=$(list_of "$A" node-a)
expect() { echo "$L" | grep -q -- "$1" || { echo "FAIL: expected $2"; echo "$L"; exit 1; }; echo "ok: $2"; }
refute() { ! echo "$L" | grep -q -- "$1" || { echo "FAIL: did not expect $2"; echo "$L"; exit 1; }; echo "ok: $2"; }
expect 'T1 .*in_progress *low *Leaking pump' "T1: B's later status and priority both applied (per-field last writer wins)"
expect 'replaced the seal'  "A's note on T1 is there"
expect 'waiting for parts'  "B's note on T1 is there"
refute 'seal cracked'       "the note B removed is gone"
expect 'T2 .*high *Replace filter' "T2: A deleted it, B edited it without knowing: it stays, with B's edit (add-wins)"
refute 'Check valve'        "T3: deleted by A, nobody else touched it: gone"
expect 'Fuel line'          "the task A created offline is there"
expect 'Order gasket'       "the task B created offline is there"
for db in "$A" "$B"; do "$BIN" store -db "$db" verify >/dev/null || { echo "FAIL: verify failed on $db"; exit 1; }; done
echo "ok: both logs verify against their views"

say "Result"
echo "Both replicas: state hash ${HA:0:16}...  IDENTICAL."
echo "(simulated partition on localhost; not real WiFi)"
