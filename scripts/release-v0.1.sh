#!/usr/bin/env bash
# Day 14: tag v0.1 once everything checks out.
#
#   scripts/release-v0.1.sh            check, then create the annotated tag v0.1 locally
#   scripts/release-v0.1.sh --push     ... and push the tag to origin
#
# It refuses to tag if: the working tree has uncommitted changes, the tests fail,
# the binary does not report version 0.1.0, or the tag already exists.
# It never commits anything and never moves an existing tag.
set -euo pipefail
cd "$(dirname "$0")/.."

TAG=v0.1
WANT=0.1.0

git rev-parse --git-dir >/dev/null 2>&1 || { echo "not a git repository: nothing to tag"; exit 1; }
if [ -n "$(git status --porcelain)" ]; then
  echo "uncommitted changes: commit (or stash) them first, so the tag points at what you tested"
  git status --short
  exit 1
fi
if git rev-parse -q --verify "refs/tags/$TAG" >/dev/null; then
  echo "tag $TAG already exists at $(git rev-parse --short "$TAG"): not moving it"
  exit 1
fi

echo "== tests (with the race detector)"
go vet ./...
go test -race ./...

echo "== build"
make build >/dev/null
GOT="$(./bin/offgrid version | awk '{print $2}')"
[ "$GOT" = "$WANT" ] || { echo "binary reports version $GOT, expected $WANT"; exit 1; }

echo "== tag $TAG at $(git rev-parse --short HEAD)"
git tag -a "$TAG" -m "Offgrid v0.1: offline-first agent runtime. Local model, durable queue, router, drain worker that survives kill -9, one-screen status. No sync yet."
git show --stat --oneline "$TAG" | head -3

if [ "${1:-}" = "--push" ]; then
  git push origin "$TAG"
  echo "pushed $TAG"
else
  echo "created locally. To publish it: git push origin $TAG"
fi
