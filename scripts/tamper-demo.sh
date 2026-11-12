#!/usr/bin/env bash
# Day 27 demo: I edited my own log and the system caught it.
#
#   scripts/tamper-demo.sh [PAUSE]
#
# Three tamperings, each done the way someone with the database file would do it
# (switch off the append-only triggers, then change the rows directly):
#
#   1. a careless edit: change a word in an old op. `store verify` names the op.
#   2. a careful edit: change the op AND recompute every hash after it AND rebuild
#      the view. The file now checks out on its own (this is the limit, shown, not
#      hidden). Two things still catch it: a head saved earlier (-anchor), and the
#      next time the edited replica syncs with a replica that kept the real history.
#
# It FAILS (exit 1) if any of the three is NOT caught, or if a clean log is accused.
# Needs python3 (its sqlite3 module plays the person with a file editor).
set -euo pipefail
cd "$(dirname "$0")/.."
PAUSE="${1:-0}"
BIN=./bin/offgrid
command -v python3 >/dev/null || { echo "this demo needs python3"; exit 1; }
make build >/dev/null
D="$(mktemp -d)"; SRV=""
export XDG_CONFIG_HOME="$D/config"
cleanup() { [ -z "$SRV" ] || kill "$SRV" 2>/dev/null || true; rm -rf "$D"; }
trap cleanup EXIT
FAILED=0
say() { printf '\n\033[1m== %s\033[0m\n' "$*"; sleep "$PAUSE"; }
fail() { echo "FAIL: $*"; FAILED=1; }
laptop() { OFFGRID_NODE=laptop "$BIN" "$@"; }
phone()  { OFFGRID_NODE=phone  "$BIN" "$@"; }
head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$D/key"
chmod 600 "$D/key"

# python plays the editor: tamper DB 'rewrite' also recomputes the chain (the careful attacker)
editor() { python3 - "$@" <<'PY'
import sqlite3, sys, hashlib, struct
db, mode = sys.argv[1], sys.argv[2]
c = sqlite3.connect(db)
for t in ("ops_append_only_update","ops_append_only_delete","op_chain_append_only_update","op_chain_append_only_delete"):
    c.execute("DROP TRIGGER IF EXISTS " + t)
c.execute("UPDATE ops SET payload = replace(payload, 'Leaking pump', 'Leaking pumpkin') WHERE node_id='laptop' AND seq=1")
if mode == "rewrite":
    def uv(n):
        out = bytearray()
        while n >= 0x80: out.append(n & 0x7f | 0x80); n >>= 7
        out.append(n); return bytes(out)
    def put(h, b):
        if isinstance(b, str): b = b.encode()
        h.update(uv(len(b))); h.update(b)
    prev = "0" * 64
    for node, seq, ts, counter, kind, payload in c.execute("SELECT node_id, seq, ts, counter, kind, payload FROM ops WHERE node_id='laptop' ORDER BY seq").fetchall():
        h = hashlib.sha256()
        for f in ("offgrid-op-v1", node, str(seq), str(ts), str(counter), kind, prev, payload): put(h, f)
        hx = h.hexdigest()
        c.execute("UPDATE op_chain SET prev=?, hash=? WHERE node_id=? AND seq=?", (prev, hx, node, seq))
        prev = hx
c.commit()
PY
}

say "1. A laptop and a phone share three tasks; both logs verify; I save the laptop's heads"
laptop exec -seed -db "$D/laptop.db" '{"intent":"list_tasks"}' >/dev/null
phone sync serve -create -db "$D/phone.db" -addr 127.0.0.1:0 -key-file "$D/key" >"$D/srv.out" 2>"$D/srv.err" &
SRV=$!
for _ in $(seq 50); do grep -q 'serving on' "$D/srv.out" 2>/dev/null && break; sleep 0.1; done
PEER=$(sed -n 's/.*serving on \(http[^ ]*\).*/\1/p' "$D/srv.out")
laptop sync once -db "$D/laptop.db" -peer "$PEER" -key-file "$D/key" >/dev/null
laptop store -db "$D/laptop.db" verify | sed -n '1p'
phone  store -db "$D/phone.db"  verify | sed -n '1p'
laptop store -db "$D/laptop.db" head | tee "$D/heads.txt" | tail -1
cp "$D/laptop.db" "$D/forged.db"; cp "$D/laptop.db" "$D/careless.db"

say "2. A careless edit: 'Leaking pump' becomes 'Leaking pumpkin' in an old op"
editor "$D/careless.db" plain
if laptop store -db "$D/careless.db" verify 2>"$D/err"; then fail "a careless edit was not caught"; else sed 's/^/   /' "$D/err" | sed -n '1,2p'; fi
laptop store -db "$D/laptop.db" verify >/dev/null || fail "the untouched log was accused"

say "3. A careful edit: same change, every later hash recomputed, the view rebuilt"
editor "$D/forged.db" rewrite
laptop store -db "$D/forged.db" rebuild >/dev/null
laptop store -db "$D/forged.db" verify | sed -n '1p;$p' | sed 's/^/   /'
echo "   (it passes on its own: the file is internally consistent. That is the limit.)"

say "3a. ...but the head I saved earlier no longer matches"
if laptop store -db "$D/forged.db" verify -anchor "$D/heads.txt" 2>"$D/err"; then fail "a rewritten chain passed the saved head"; else sed 's/^/   /' "$D/err" | sed -n '1p'; fi

say "3b. ...and the first new op it makes is refused by a replica that kept the real history"
kill "$SRV" 2>/dev/null || true; SRV=""
laptop exec -db "$D/forged.db" '{"intent":"create_task","title":"Fresh task","priority":"low"}' >/dev/null
laptop sync serve -db "$D/forged.db" -addr 127.0.0.1:0 -key-file "$D/key" >"$D/srv2.out" 2>"$D/srv2.err" &
SRV=$!
for _ in $(seq 50); do grep -q 'serving on' "$D/srv2.out" 2>/dev/null && break; sleep 0.1; done
PEER2=$(sed -n 's/.*serving on \(http[^ ]*\).*/\1/p' "$D/srv2.out")
if phone sync once -db "$D/phone.db" -peer "$PEER2" -key-file "$D/key" >"$D/o" 2>"$D/err"; then fail "the phone accepted an op built on a rewritten history"; else sed 's/^/   /' "$D/err" | sed -n '1,2p'; fi
phone store -db "$D/phone.db" verify | sed -n '1p' | sed 's/^/   phone: /'

[ "$FAILED" = "0" ] || exit 1
printf '\n\033[1mOK: the careless edit was named, the careful one was caught by a saved head and by sync, and the real logs were never accused.\033[0m\n'
