#!/usr/bin/env bash
# Day 32. Run this ON THE NODE after it has booted again.
#
#   node$ ./powerpull-check.sh /var/lib/offgrid/tasks.db acks.log
#
# acks.log is the file the LAPTOP wrote while the writer ran (copy it over). Checks:
#   1. the database opens, SQLite's integrity check passes, the hash chain and the view agree
#   2. every ACK in acks.log is in the database  (zero lost acknowledged writes)
# Writes that are in the database but were never ACKed are fine and expected: the node
# committed them in the moment before the power went, and the ACK did not make it out.
#
# Exit 0 only if both pass. The summary line is meant to be pasted into the report.
set -u
DB="${1:?usage: powerpull-check.sh DB ACKS}"
ACKS="${2:?usage: powerpull-check.sh DB ACKS}"
BIN="${OFFGRID_BIN:-offgrid}"
fail=0

echo "== 1. doctor (integrity, hash chain, view)"
if ! "$BIN" doctor -db "$DB" -deep -listen ""; then
  echo "FAIL: doctor reported a failure"
  fail=1
fi

echo
echo "== 2. every acknowledged write is present"
# Only complete lines count: a line cut off mid-write by a dropped connection is not an ACK.
acked="$(grep -E '^ACK pp-[0-9a-f]{8}-[0-9]+-[0-9]+$' "$ACKS" | sed 's/^ACK //' | sort -u)"
present="$("$BIN" store -db "$DB" ops -n 2000000000 | grep -o '"title":"pp-[0-9a-f]\{8\}-[0-9]*-[0-9]*"' | sed 's/^"title":"//;s/"$//' | sort -u)"
n_acked="$(printf '%s' "$acked" | grep -c . || true)"
n_present="$(printf '%s' "$present" | grep -c . || true)"
missing="$(comm -23 <(printf '%s\n' "$acked") <(printf '%s\n' "$present") | grep . || true)"
extra="$(comm -13 <(printf '%s\n' "$acked") <(printf '%s\n' "$present") | grep -c . || true)"
n_missing="$(printf '%s' "$missing" | grep -c . || true)"

echo "acknowledged (laptop saw ACK): $n_acked"
echo "present in the database:       $n_present"
echo "written but not acknowledged:  $extra   (allowed: committed just before the power went)"
echo "LOST acknowledged writes:      $n_missing"
if [ "$n_acked" -eq 0 ]; then
  echo "FAIL: acks.log has no ACK lines, so nothing was tested (wrong file? writer not run?)"
  fail=1
fi
if [ "$n_missing" -gt 0 ]; then
  echo "$missing" | head -20 | sed 's/^/  lost: /'
  fail=1
fi

echo
if [ "$fail" -eq 0 ]; then
  echo "RESULT: PASS  acked=$n_acked present=$n_present lost=0"
else
  echo "RESULT: FAIL  acked=$n_acked present=$n_present lost=$n_missing"
fi
exit "$fail"
