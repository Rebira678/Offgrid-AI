#!/usr/bin/env bash
# Day 28: turn each "## Title" section of docs/good-first-issues.md into a GitHub issue
# labelled "good first issue". Needs the gh CLI, logged in (gh auth login), run inside the clone.
#
#   scripts/create-good-first-issues.sh          create them
#   DRY=1 scripts/create-good-first-issues.sh    print what would be created, create nothing
#
# It skips a title that already has an open or closed issue, so running it twice is safe.
set -euo pipefail
cd "$(dirname "$0")/.."
FILE=docs/good-first-issues.md
LABEL="good first issue"
if [ -z "${DRY:-}" ] && ! command -v gh >/dev/null 2>&1; then
  echo "error: gh (GitHub CLI) is not installed or not in PATH. Please install it (https://cli.github.com) or run with DRY=1" >&2
  exit 1
fi

emit() { # title, body file
  local title="$1" body="$2"
  if [ -n "${DRY:-}" ]; then
    echo "--- would create: $title"; sed 's/^/    /' "$body"; return
  fi
  if gh issue list --state all --search "\"$title\" in:title" --json title --jq '.[].title' | grep -Fxq "$title"; then
    echo "exists, skipped: $title"; return
  fi
  gh label create "$LABEL" --color 7057ff --description "Good for newcomers" >/dev/null 2>&1 || true
  gh issue create --title "$title" --body-file "$body" --label "$LABEL"
}

tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
title=""; : >"$tmp/body"
flush() {
  if [ -n "$title" ]; then
    printf '\nSetup is in CONTRIBUTING.md in the repository. Please comment here before you start.\n' >>"$tmp/body"
    emit "$title" "$tmp/body"
  fi
}
while IFS= read -r line; do
  case "$line" in
    "## "*) flush; title="${line#\#\# }"; : >"$tmp/body" ;;
    *) [ -n "$title" ] && printf '%s\n' "$line" >>"$tmp/body" ;;
  esac
done <"$FILE"
flush
