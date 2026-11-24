#!/usr/bin/env bash
# Daily growth check for one replica (Day 49). Put it in cron:
#   17 6 * * *  OFFGRID_DB=/home/pi/offgrid.db /home/pi/offgrid/scripts/growth-alarm.sh
#
# It records today's sample (so the rate alarm has history), prints the numbers, and when any limit is
# crossed it exits 3, writes the reason to $ALARM_FILE (default <db>.growth.alarm) and, if $ALARM_CMD is
# set, runs it with the reason as its only argument (mail, a push, a line on the status page).
# The alarm file is removed when everything is back under its limits.
#
# Limits are MiB except ops. Defaults are ADR 010's triggers plus a blob ceiling for a small SD card:
#   MAX_OPS=500000 MAX_DB=1024 MAX_BLOBS=8192 MAX_DB_DAY=5 MAX_BLOBS_DAY=200 YEAR_DB=1024 YEAR_BLOBS=16384
set -u
db="${OFFGRID_DB:?set OFFGRID_DB to the database file}"
bin="${OFFGRID_BIN:-offgrid}"
alarm_file="${ALARM_FILE:-$db.growth.alarm}"

out="$("$bin" store -db "$db" growth -record \
  -max-ops "${MAX_OPS:-500000}" -max-db "${MAX_DB:-1024}" -max-blobs "${MAX_BLOBS:-8192}" \
  -max-db-per-day "${MAX_DB_DAY:-5}" -max-blobs-per-day "${MAX_BLOBS_DAY:-200}" \
  -year-db "${YEAR_DB:-1024}" -year-blobs "${YEAR_BLOBS:-16384}" 2>&1)"
code=$?
printf '%s\n' "$out"
if [ "$code" -eq 3 ]; then
  reason="$(printf '%s\n' "$out" | sed -n 's/^ALARM *//p')"
  umask 077
  printf '%s\n%s\n' "$(date -u +%FT%TZ)" "$reason" >"$alarm_file"
  if [ -n "${ALARM_CMD:-}" ]; then "$ALARM_CMD" "$reason" || true; fi
elif [ "$code" -eq 0 ]; then
  rm -f "$alarm_file"
fi
exit "$code"
