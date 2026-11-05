#!/usr/bin/env bash
# Day 30: the checks to run right before you post. It changes nothing.
#
#   scripts/launch-check.sh            local checks, plus GitHub checks if gh is installed and logged in
#
# Exit 1 if anything is wrong. "skip" means a check could not run here, not that it passed.
set -uo pipefail
cd "$(dirname "$0")/.."
bad=0
ok()   { printf '  ok    %s\n' "$*"; }
fail() { printf '  FAIL  %s\n' "$*"; bad=1; }
skip() { printf '  skip  %s\n' "$*"; }

echo "files"
for f in LICENSE CONTRIBUTING.md README.md ARCHITECTURE.md .github/workflows/ci.yml .github/workflows/release.yml docs/day-29-demo.gif docs/architecture.png; do
  [ -f "$f" ] && ok "$f" || fail "$f is missing"
done

echo "README"
# every relative link and image must point at a file that exists
missing=$(python3 - <<'PY'
import re, os
t = open("README.md", encoding="utf-8").read()
for m in re.finditer(r'\]\(([^)\s]+)\)', t):
    u = m.group(1)
    if re.match(r'^[a-z]+:', u) or u.startswith('#') or u.startswith('../../'):
        continue
    p = u.split('#')[0]
    if p and not os.path.exists(p):
        print(p)
PY
)
[ -z "$missing" ] && ok "every relative link in the README exists" || { fail "README links to files that do not exist:"; echo "$missing" | sed 's/^/          /'; }
if grep -nE '<your |⟦|TODO|FIXME|XXX' README.md >/dev/null; then fail "README has a placeholder:"; grep -nE '<your |⟦|TODO|FIXME|XXX' README.md | sed 's/^/          /'; else ok "no placeholders in the README"; fi
if grep -qiE "xeon|cloud sandbox" README.md; then
  fail "README still holds sandbox benchmark rows (a Xeon cloud sandbox). Run make bench on your laptop, commit the result"
else ok "README benchmark rows are not the sandbox's"; fi

echo "module"
grep -q 'pgregory.net/rapid' go.mod && ok "go.mod lists rapid" || fail "go.mod does not list rapid (go get pgregory.net/rapid@v1.2.0)"
grep -q 'pgregory.net/rapid' go.sum && ok "go.sum has rapid" || fail "go.sum has no rapid"

echo "build and tests"
if go build ./... && go vet ./... && [ -z "$(gofmt -l .)" ]; then ok "build, vet, gofmt"; else fail "build, vet or gofmt"; fi
if go test -count=1 ./... >/tmp/offgrid-launch-test.$$ 2>&1; then ok "go test ./..."; else fail "tests (see below)"; grep -v '^ok\|no test files' /tmp/offgrid-launch-test.$$ | tail -15; fi
rm -f /tmp/offgrid-launch-test.$$

echo "git and GitHub"
if git rev-parse --git-dir >/dev/null 2>&1; then
  [ -z "$(git status --porcelain)" ] && ok "working tree is clean" || fail "uncommitted changes"
  [ "$(git rev-parse --abbrev-ref HEAD)" = "main" ] && ok "on main" || fail "not on main"
  if git rev-parse '@{u}' >/dev/null 2>&1; then
    [ "$(git rev-parse HEAD)" = "$(git rev-parse '@{u}')" ] && ok "pushed" || fail "local main differs from its remote: push (or pull) first"
  else skip "no upstream set"; fi
else skip "not a git repository"; fi
if command -v gh >/dev/null && gh auth status >/dev/null 2>&1; then
  c=$(gh run list --workflow ci --branch main --limit 1 --json conclusion --jq '.[0].conclusion' 2>/dev/null)
  [ "$c" = "success" ] && ok "latest CI run on main: success" || fail "latest CI run on main: ${c:-none}"
  r=$(gh release list --limit 1 --json tagName --jq '.[0].tagName' 2>/dev/null)
  [ -n "$r" ] && ok "latest release: $r" || fail "no release published"
  n=$(gh issue list --label "good first issue" --state open --json number --jq 'length' 2>/dev/null)
  [ "${n:-0}" -ge 3 ] && ok "$n open good first issues" || fail "fewer than 3 open good first issues"
else skip "gh is not installed or not logged in: check CI, the release and the issues by eye"; fi

echo
[ "$bad" = 0 ] && echo "READY (for what these checks can see)" || echo "NOT READY"
exit "$bad"
