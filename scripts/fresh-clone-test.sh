#!/usr/bin/env bash
# Day 29: what a friend does on a clean machine, timed. "Done when: a friend clones the repo and runs it in under 10 minutes."
#
#   scripts/fresh-clone-test.sh              copy this repo to a temp folder, then build, test, run both demos
#   scripts/fresh-clone-test.sh URL_OR_PATH  clone that instead (a git URL, e.g. your GitHub repo: the real test)
#
# Fails if any step fails or the total is over LIMIT seconds (default 600). Changes nothing in this repo.
# It needs Go 1.22+ and a C compiler, like a friend would. It does not need Ollama or the network (except a clone).
set -euo pipefail
LIMIT="${LIMIT:-600}"
SRC="${1:-}"
here="$(cd "$(dirname "$0")/.." && pwd)"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
dest="$tmp/offgrid"

if [ -n "$SRC" ]; then
  git clone --quiet --depth 1 "$SRC" "$dest"
else
  mkdir "$dest"
  # the tree as a friend would get it: no build output, no local databases
  tar -C "$here" --exclude=./bin --exclude=./dist --exclude=./.git -cf - . | tar -C "$dest" -xf -
fi
cd "$dest"

start=$SECONDS; n=0
step() { # name, command...
  local name="$1"; shift
  local t0=$SECONDS
  printf '\n\033[1m[%d] %s\033[0m\n' "$((++n))" "$name"
  if ! "$@" >"$tmp/step.log" 2>&1; then
    echo "FAILED: $name (last lines of its output):"; tail -25 "$tmp/step.log"; exit 1
  fi
  printf '    ok, %ds\n' "$((SECONDS - t0))"
}

step "make build"                     make build
step "make test"                      make test
step "scripts/demo-offline.sh"        scripts/demo-offline.sh
step "make converge-demo"             make converge-demo
step "./bin/offgrid version"          ./bin/offgrid version

total=$((SECONDS - start))
printf '\ntotal: %ds (limit %ds)\n' "$total" "$LIMIT"
if [ "$total" -gt "$LIMIT" ]; then echo "TOO SLOW"; exit 1; fi
echo "OK: a clean copy builds, passes its tests and runs both demos."
