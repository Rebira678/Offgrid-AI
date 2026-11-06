#!/usr/bin/env bash
# Day 31: can the merge code run in a browser or any WebAssembly host?
#
#   scripts/wasm-check.sh          build ./internal/crdt and ./internal/hlc for WebAssembly
#   scripts/wasm-check.sh --run    also run their tests as WebAssembly (needs node 18 or newer)
#
# Why: a phone that cannot run the Go binary (the PWA, Day 33 to 36) may need to run the
# same merge rules, and two copies of the rules in two languages is how replicas diverge.
# These two packages use only the standard library, so they build for GOOS=js GOARCH=wasm.
# This script keeps that true: it fails on any other import (the SQLite driver, say) and on
# a build error, and CI runs it.
#
# Done means: green, or a short list of what blocks it (printed below on failure).
set -euo pipefail
cd "$(dirname "$0")/.."

pkgs=(./internal/crdt ./internal/hlc)

# A build is not enough. With cgo off (it always is for js/wasm), some cgo packages compile
# to a stub that fails only when called (mattn/go-sqlite3 does). So the real rule is on the
# imports: the merge packages may use the standard library and each other, nothing else.
echo "== imports of ${pkgs[*]}"
bad="$(go list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' "${pkgs[@]}" | grep -v -x -e offgrid/internal/crdt -e offgrid/internal/hlc || true)"
if [ -n "$bad" ]; then
  echo "BLOCKED. The merge packages import something that is not the standard library or each other:"
  echo "$bad" | sed 's/^/  /'
  echo "Move that code out, or the phone cannot run the same merge rules."
  exit 1
fi
echo "ok: standard library only"

echo "== build for js/wasm: ${pkgs[*]}"
if ! GOOS=js GOARCH=wasm go build "${pkgs[@]}"; then
  echo
  echo "BLOCKED. The merge packages do not build for WebAssembly. What they import that is not pure Go:"
  GOOS=js GOARCH=wasm go list -deps -f '{{if .CgoFiles}}cgo: {{.ImportPath}}{{end}}' "${pkgs[@]}" 2>/dev/null | grep . || true
  exit 1
fi
echo "ok: builds"

[ "${1:-}" = "--run" ] || exit 0

command -v node >/dev/null || { echo "BLOCKED: --run needs node (18 or newer) to host the WebAssembly"; exit 1; }
goroot="$(go env GOROOT)"
host="$goroot/lib/wasm/wasm_exec_node.js"
[ -f "$host" ] || host="$goroot/misc/wasm/wasm_exec_node.js" # Go before 1.24 keeps it here
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
for p in "${pkgs[@]}"; do
  name="$(basename "$p")"
  echo "== test as wasm: $p"
  GOOS=js GOARCH=wasm go test -c -o "$tmp/$name.wasm" "$p"
  # A clean environment: wasm_exec refuses to start when the environment is large (CI runners'
  # are), and the tests do not need any variable. The tests read testdata relative to their package folder.
  (cd "$p" && env -i PATH="$PATH" HOME="$tmp" node "$host" "$tmp/$name.wasm")
done
echo "ok: tests pass under WebAssembly"
