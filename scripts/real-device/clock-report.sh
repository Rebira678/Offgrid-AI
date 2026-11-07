#!/usr/bin/env bash
# Day 32. The cold-boot clock test. Run this ON THE NODE, right after it boots with NO network
# (no router, no phone hotspot): this is the situation of a site whose internet is down.
#
#   node$ ./clock-report.sh /var/lib/offgrid/tasks.db | tee clock-coldboot.txt
#
# Before you shut it down, write on paper what the real time is. Compare with "system clock" below.
# Paste the output into docs/real-device-report.md, with how long the node had been off.
set -u
DB="${1:-}"
BIN="${OFFGRID_BIN:-offgrid}"

echo "== clock report ($(hostname), $(uname -m))"
echo "system clock (UTC):   $(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo "booted at (local):    $(uptime -s 2>/dev/null || echo unknown)"
echo "time server (NTP):    $(timedatectl show -p NTPSynchronized --value 2>/dev/null || echo 'unknown (no timedatectl)')"
if [ -d /sys/class/rtc ] && [ -n "$(ls /sys/class/rtc 2>/dev/null)" ]; then
  echo "hardware clock:       present ($(ls /sys/class/rtc | tr '\n' ' ')); $(cat /sys/class/rtc/rtc0/name 2>/dev/null)"
  echo "hardware clock time:  $(cat /sys/class/rtc/rtc0/since_epoch 2>/dev/null | xargs -I{} date -u -d @{} +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo unreadable)"
else
  echo "hardware clock:       none"
fi
if command -v fake-hwclock >/dev/null 2>&1; then
  echo "fake-hwclock:         installed (restores the time it last saved; it does not keep time while off)"
  [ -f /etc/fake-hwclock.data ] && echo "fake-hwclock saved:   $(cat /etc/fake-hwclock.data)"
fi
echo
if [ -n "$DB" ]; then
  "$BIN" doctor -db "$DB" -listen "" | grep -E '^ +(PASS|WARN|FAIL)  clock|fix:'
  echo
  echo "newest edit in the log: $("$BIN" store -db "$DB" info 2>/dev/null | grep -i -E 'newest|clock ahead' | tr '\n' ' ')"
fi
