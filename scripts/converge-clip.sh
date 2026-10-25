#!/usr/bin/env bash
# Flagship clip #2: split-screen, two replicas, WiFi off, edits on both sides,
# reconnect, hashes match. Needs tmux. Start your screen recorder, then run:
#
#   make build && scripts/converge-clip.sh
#
# Layout: replica A (left), replica B (right), and underneath B's live sync log.
# The script types the commands for you, like a person would. Nothing here is
# faked: the same binary, the same databases, the same fault switch as
# scripts/converge-demo.sh. The WiFi is simulated by offgrid's fault injection.
#
#   TYPE_DELAY=0.04   seconds per typed character (0 for tests)
#   START_DELAY=4     seconds before the first command, to start recording
#   DETACH=1          do not attach (tests)
set -euo pipefail
cd "$(dirname "$0")/.."
command -v tmux >/dev/null || { echo "this script needs tmux (sudo apt install tmux)"; exit 1; }
BIN=$(cd "$(dirname "${BIN:-bin/offgrid}")" && pwd)/$(basename "${BIN:-bin/offgrid}")
[ -x "$BIN" ] || { echo "build first: make build"; exit 1; }
TYPE_DELAY=${TYPE_DELAY:-0.04}
START_DELAY=${START_DELAY:-4}
S=offgrid-clip
D=$(mktemp -d /tmp/offgrid-clip.XXXXXX)
tmux kill-session -t $S 2>/dev/null || true
head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$D/key"; echo ok > "$D/fault"

tmux new-session -d -s $S -x "${COLUMNS_OVERRIDE:-200}" -y "${LINES_OVERRIDE:-52}" -c "$D"
PL=$(tmux display -p -t $S '#{pane_id}')
PB=$(tmux split-window -v -l 14 -t "$PL" -c "$D" -P -F '#{pane_id}')
PR=$(tmux split-window -h -t "$PL" -c "$D" -P -F '#{pane_id}')
tmux set -t $S pane-border-status top
tmux set -t $S pane-border-format ' #{pane_title} '
tmux select-pane -t "$PL" -T "REPLICA A  (node-a)"
tmux select-pane -t "$PR" -T "REPLICA B  (node-b)"
tmux select-pane -t "$PB" -T "B's sync loop  (the connection)"

prep() { # pane node
  tmux send-keys -t "$1" "export OFFGRID_NODE=$2 PS1='\$ ' D=$D; og() { $BIN \"\$@\"; }; clear" Enter
}
prep "$PL" node-a; prep "$PR" node-b; prep "$PB" node-b
sleep 0.5

typeit() { # pane text : type like a person, then Enter
  local pane=$1 text=$2 i
  if [ "$TYPE_DELAY" = 0 ]; then tmux send-keys -t "$pane" -l "$text"
  else for ((i = 0; i < ${#text}; i++)); do tmux send-keys -t "$pane" -l "${text:$i:1}"; sleep "$TYPE_DELAY"; done; fi
  tmux send-keys -t "$pane" Enter
}
beat() { sleep "${1:-1.2}"; }
note() { typeit "$1" "# $2"; }   # a shell comment is a caption

driver() {
  sleep "$START_DELAY"
  # --- setup: A has work, serves; B syncs through the chaos switch
  note "$PL" "A has three tasks"
  typeit "$PL" "og exec -db \$D/a.db '{\"intent\":\"create_task\",\"title\":\"Leaking pump\",\"site\":\"4\",\"priority\":\"high\"}' >/dev/null"; beat 0.8
  typeit "$PL" "og exec -db \$D/a.db '{\"intent\":\"create_task\",\"title\":\"Replace filter\",\"priority\":\"low\"}' >/dev/null"; beat 0.8
  typeit "$PL" "og exec -db \$D/a.db '{\"intent\":\"create_task\",\"title\":\"Check valve\",\"priority\":\"medium\"}' >/dev/null"; beat 0.8
  typeit "$PL" "og exec -db \$D/a.db '{\"intent\":\"add_note\",\"task_ref\":\"T1\",\"note\":\"seal cracked\"}' >/dev/null"; beat 0.8
  typeit "$PL" "og sync serve -db \$D/a.db -addr 127.0.0.1:7878 -key-file \$D/key 2>/dev/null &"; beat 1.2
  note "$PR" "B starts empty and syncs with A"
  typeit "$PB" "og sync run -db \$D/b.db -peer http://127.0.0.1:7878 -key-file \$D/key -every 500ms -name B -fault-file \$D/fault"; beat 3
  typeit "$PR" "og task -db \$D/b.db list"; beat 2.5
  # --- wifi off
  note "$PR" "WiFi OFF"
  typeit "$PR" "og fault set -file \$D/fault partition"; beat 2.5
  # --- edits on both sides (same task T1)
  note "$PL" "offline: finish T1, add a note, delete two tasks, add one"
  typeit "$PL" "og exec -db \$D/a.db '{\"intent\":\"update_task\",\"task_ref\":\"T1\",\"status\":\"done\"}' | tail -1"; beat 1
  typeit "$PL" "og exec -db \$D/a.db '{\"intent\":\"add_note\",\"task_ref\":\"T1\",\"note\":\"replaced the seal\"}' | tail -1"; beat 1
  typeit "$PL" "og task -db \$D/a.db delete T2"; beat 0.8
  typeit "$PL" "og task -db \$D/a.db delete T3"; beat 0.8
  typeit "$PL" "og exec -db \$D/a.db '{\"intent\":\"create_task\",\"title\":\"Fuel line\",\"priority\":\"medium\"}' | tail -1"; beat 1
  note "$PR" "offline: the SAME task T1, a note, remove a note, edit T2, add one"
  typeit "$PR" "og exec -db \$D/b.db '{\"intent\":\"update_task\",\"task_ref\":\"T1\",\"status\":\"in_progress\",\"priority\":\"low\"}' | tail -1"; beat 1
  typeit "$PR" "og exec -db \$D/b.db '{\"intent\":\"add_note\",\"task_ref\":\"T1\",\"note\":\"waiting for parts\"}' | tail -1"; beat 1
  typeit "$PR" "og task -db \$D/b.db remove-note T1 1"; beat 0.8
  typeit "$PR" "og exec -db \$D/b.db '{\"intent\":\"update_task\",\"task_ref\":\"T2\",\"priority\":\"high\"}' | tail -1"; beat 1
  typeit "$PR" "og exec -db \$D/b.db '{\"intent\":\"create_task\",\"title\":\"Order gasket\",\"priority\":\"high\"}' | tail -1"; beat 1.5
  # --- they differ
  typeit "$PL" "clear; og task -db \$D/a.db list"; typeit "$PR" "clear; og task -db \$D/b.db list"; beat 4
  # --- wifi on
  note "$PR" "WiFi ON"
  typeit "$PR" "og fault set -file \$D/fault ok"; beat 4
  typeit "$PL" "clear; og task -db \$D/a.db list"; typeit "$PR" "clear; og task -db \$D/b.db list"; beat 6
}

if [ "${DETACH:-0}" = 1 ]; then driver; echo "$D"; exit 0; fi
driver &
tmux attach -t $S
wait
echo "done; the session is gone, files are in $D (delete them when you like)"
