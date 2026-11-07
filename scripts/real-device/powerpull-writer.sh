#!/usr/bin/env bash
# Day 32. Run this ON THE NODE under test, and read its output on ANOTHER machine:
#
#   laptop$ ssh -o ServerAliveInterval=2 pi@node './powerpull-writer.sh /var/lib/offgrid/tasks.db' | tee -a acks.log
#
# It writes one task at a time, through the same command a person would use, and prints
# "ACK <title>" only after offgrid has exited successfully, i.e. after the commit is on
# disk (synchronous=FULL). Because the ACK travels over the network to the laptop, the
# laptop's acks.log is a record that survives the node losing power. That is the point:
# a log kept on the node itself would be lost with the thing being tested.
#
# Pull the node's power at a random moment. Boot it, then run powerpull-check.sh.
# An ACK that is missing from the database afterwards is a lost acknowledged write.
#
# usage: powerpull-writer.sh DB [WORKERS]      (WORKERS defaults to 2)
set -u
DB="${1:?usage: powerpull-writer.sh DB [WORKERS]}"
P="${2:-2}"
BIN="${OFFGRID_BIN:-offgrid}"

# A fresh run id each time, so titles never repeat across power pulls. (Not the date:
# after a cold boot with no battery clock, the date is the thing we do not trust.)
RUN="$(head -c4 /dev/urandom | od -An -tx1 | tr -d ' \n')"
echo "RUN $RUN"

worker() {
  local w="$1" n=0 title
  while :; do
    n=$((n + 1))
    title="pp-$RUN-$w-$n"
    # -strict: a refused command is a failure, not an ACK.
    if "$BIN" exec -strict -db "$DB" "{\"intent\":\"create_task\",\"title\":\"$title\"}" >/dev/null 2>&1; then
      echo "ACK $title"
    else
      echo "ERR $title" >&2
      sleep 0.2
    fi
  done
}

for w in $(seq 1 "$P"); do worker "$w" & done
wait
