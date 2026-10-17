#!/usr/bin/env bash
# Checkpoint 1 demo: WiFi off, the agent keeps working, the queue fills.
#
#   scripts/checkpoint1-demo.sh [PAUSE]        PAUSE = seconds between steps (default 0)
#
# NET=auto      (default) probe the real network: turn WiFi off yourself on camera
# NET=offline   pretend the network is down: identical every time, safe for a retake
# MODEL=0       (default) rules only, no language model: nothing needs Ollama
# MODEL=1       also let the local model (Tier 1) read what the rules cannot
#
# Everything is kept in a throwaway folder that is deleted at the end.
set -euo pipefail
cd "$(dirname "$0")/.."

PAUSE="${1:-0}"
NET="${NET:-auto}"
MODEL="${MODEL:-0}"
BIN=./bin/offgrid
make build >/dev/null   # always rebuild: a stale binary would demo old behaviour

WORK="${TMPDIR:-/tmp}/offgrid-checkpoint1-$$"
mkdir -p "$WORK"
trap 'rm -rf "$WORK"' EXIT
export OFFGRID_DB="$WORK/tasks.db"
export OFFGRID_QUEUE="$WORK/queue.db"

FLAGS=(-route -net "$NET")
[ "$MODEL" = "1" ] || FLAGS+=(-no-model)

say() { printf '\n\033[1m== %s\033[0m\n' "$*"; sleep "$PAUSE"; }
ask() { printf '\033[2m$ offgrid agent "%s"\033[0m\n' "$1"; sleep "$PAUSE"; $BIN agent "${FLAGS[@]}" "$1" | sed '1,2d'; sleep "$PAUSE"; }

say "Is the network up? (offgrid net -once)"
$BIN net -once || true
[ "$NET" = "auto" ] || echo "(forced: -net $NET)"

say "1. Simple requests are answered at once, on this laptop"
$BIN agent -seed -no-model "list open tasks" >/dev/null   # three starting tasks
ask "what's still open?"
ask "new task: check the fuel line, site 2, high priority"
ask "note on the fuel line task: line is cracked near the clamp"

say "2. Hard requests are not guessed at. They are saved, and say so"
ask "summarize this week's incidents and suggest priorities"
ask "what should I prioritize this week?"
ask "why does the fuel line keep cracking"

say "3. Asking again does not queue it twice"
ask "what should I prioritize this week?"

say "4. What is waiting?"
ask "queue"
printf '\033[2m$ offgrid queue list\033[0m\n'; $BIN queue list

say "5. Back to simple work: still instant"
ask "summary"

printf '\nThe three hard requests are on disk. They run when the connection is back: offgrid drain (Day 12).\n'
