#!/usr/bin/env bash
# Day 17 (extended on Day 18 with the OR-Set): run the property tests on the merges, then show them catching a broken one.
#
#   scripts/prove-merge.sh [CHECKS]     CHECKS = generated cases per law (default 3000)
#
# Needs the rapid module once:  go get pgregory.net/rapid@v1.2.0
# Writes nothing to your repo and never touches git.
set -euo pipefail
cd "$(dirname "$0")/.."
CHECKS="${1:-3000}"

if ! grep -q 'pgregory.net/rapid' go.mod; then
  echo "pgregory.net/rapid is not in go.mod yet. Run this once, then try again:" >&2
  echo "    go get pgregory.net/rapid@v1.2.0" >&2
  exit 1
fi

echo "== 1. the real merge: every law, $CHECKS generated cases each =="
go test ./internal/crdt ./internal/store/sqlstore -count=1 -run 'Laws|Generated|Written|Writes|PutIs|Property|Oracle|SyncOrder|EveryElement|NoteElementsRound' -v \
  -args -rapid.checks="$CHECKS" 2>&1 | grep -E '\[rapid\] OK|^(ok|FAIL|--- FAIL)'
test "${PIPESTATUS[0]}" -eq 0

echo
echo "== 2. deliberately broken merges: the same laws must catch them =="
go test -count=1 -run 'Deliberately' -v ./internal/crdt 2>&1 \
  | grep -E '^(=== RUN .*Deliberately[A-Za-z]*/|\s+--- FAIL|\s+(broken_test|orset_broken_test)\.go:[0-9]+: (caught by|accepted by))' \
  | sed -E 's/(broken_test|orset_broken_test)\.go:[0-9]+: //; s#^=== RUN +[A-Za-z]+/#\n#; s#_# #g; s/^ +//'
echo
echo "Every broken merge is caught by at least one law; the CONTROLS (legitimate, just different) are accepted."

