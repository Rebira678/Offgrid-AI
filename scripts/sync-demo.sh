#!/usr/bin/env bash
# Day 19 demo: two real offgrid processes, two databases, one localhost HTTP link.
# Everything lives in a temp folder that is deleted at the end.
set -euo pipefail
cd "$(dirname "$0")/.."
BIN=${BIN:-bin/offgrid}
[ -x "$BIN" ] || { echo "build first: make build"; exit 1; }
D=$(mktemp -d)
trap 'kill $SRV 2>/dev/null || true; rm -rf "$D"' EXIT
head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$D/key"

echo "== Lidia has two tasks; Beka is empty"
$BIN exec -db "$D/a.db" '{"intent":"create_task","title":"Leaking pump","site":"4","priority":"high"}' >/dev/null
$BIN exec -db "$D/a.db" '{"intent":"create_task","title":"Replace filter","priority":"low"}' >/dev/null

echo "== Beka starts serving on a free localhost port"
$BIN sync serve -db "$D/b.db" -addr 127.0.0.1:0 -key-file "$D/key" > "$D/srv.out" 2> "$D/srv.err" &
SRV=$!
for _ in $(seq 50); do grep -q 'serving on' "$D/srv.out" 2>/dev/null && break; sleep 0.1; done
URL=$(sed -n 's/.*serving on \(http[^ ]*\).*/\1/p' "$D/srv.out")
echo "   $URL"

echo "== round 1: Lidia syncs with Beka"
$BIN sync once -db "$D/a.db" -peer "$URL" -key-file "$D/key"

echo "== both go offline and edit (Lidia marks T1 done; Beka adds a note and a task)"
$BIN exec -db "$D/a.db" '{"intent":"update_task","task_ref":"T1","status":"done"}' >/dev/null
$BIN exec -db "$D/b.db" '{"intent":"add_note","task_ref":"T2","note":"waiting for parts"}' >/dev/null
$BIN exec -db "$D/b.db" '{"intent":"create_task","title":"Check valve","priority":"high"}' >/dev/null
echo "   Lidia: $($BIN sync summary -db "$D/a.db" | grep 'state hash')"
echo "   Beka:  $($BIN sync summary -db "$D/b.db" | grep 'state hash')"

echo "== reconnect: round 2"
$BIN sync once -db "$D/a.db" -peer "$URL" -key-file "$D/key"
echo "   Lidia: $($BIN sync summary -db "$D/a.db" | grep 'state hash')"
echo "   Beka:  $($BIN sync summary -db "$D/b.db" | grep 'state hash')"
echo "== a wrong key is refused"
head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$D/bad"
$BIN sync once -db "$D/a.db" -peer "$URL" -key-file "$D/bad" || echo "   (refused, as it should be)"
