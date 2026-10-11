#!/usr/bin/env bash
# A complete offline demo: log field tasks with NO model and NO network,
# stop, start again, and show the tasks survived; then prove the task table
# is only a cache by deleting it and rebuilding it from the log.
#
# Safe to run any time: it uses its own throwaway database. Pass a pause in
# seconds as the first argument to slow it down for screen recording:
#   scripts/demo-offline.sh 2
set -euo pipefail
cd "$(dirname "$0")/.."

PAUSE="${1:-0}"
BIN=./bin/offgrid
[ -x "$BIN" ] || make build >/dev/null
export OFFGRID_DB="${TMPDIR:-/tmp}/offgrid-demo-$$/tasks.db"
trap 'rm -rf "$(dirname "$OFFGRID_DB")"' EXIT

step() { printf '\n\033[1m== %s\033[0m\n' "$*"; sleep "$PAUSE"; }
show() { printf '\033[2m$ %s\033[0m\n' "$*"; sleep "$PAUSE"; }

step "1. Log tasks. No internet, no model: Tier 0 rules do the understanding"
show 'offgrid agent -no-model   (3 messages typed in)'
$BIN agent -no-model <<'MSGS'
new task: check the fuel line, site 2, high priority
new task: replace the pump filter, site 4
note on the fuel task: line is cracked near the clamp
MSGS

step "2. Close the program. Start it again. The tasks are still here"
show 'offgrid agent -no-model   (a new process)'
$BIN agent -no-model <<'MSGS'
what's still open?
MSGS

step "3. Every change was stored as an immutable fact (the op log)"
show 'offgrid store info'
$BIN store info
show 'offgrid store ops'
$BIN store ops

step "4. Check the task table against a replay of the log"
show 'offgrid store verify'
$BIN store verify

# Drop the view tables with sqlite3 if installed, else with python3 (stdlib).
dropview() {
  if command -v sqlite3 >/dev/null; then
    sqlite3 "$OFFGRID_DB" 'DROP TABLE tasks; DROP TABLE task_notes;'
  else
    python3 -c 'import sqlite3,sys; c=sqlite3.connect(sys.argv[1]); c.executescript("DROP TABLE tasks; DROP TABLE task_notes;")' "$OFFGRID_DB"
  fi
}

if command -v sqlite3 >/dev/null || command -v python3 >/dev/null; then
  step "5. Destroy the task table. The log is the truth, so nothing is lost"
  show 'DROP TABLE tasks; DROP TABLE task_notes;'
  dropview
  show 'offgrid store rebuild'
  $BIN store rebuild
  show 'offgrid agent: ask again'
  echo "what's still open?" | $BIN agent -no-model
else
  step "5. (install sqlite3 or python3 to see the delete-and-rebuild step)"
fi
printf '\nDone. Nothing used the network.\n'
