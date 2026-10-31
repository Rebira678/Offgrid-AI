#!/usr/bin/env bash
# toggle-network.sh - Toggle the simulated network state between ONLINE and OFFLINE
# Useful for screen recordings and offline demos without waiting for probe intervals.

set -euo pipefail

TARGET="${1:-toggle}"
METRICS_FILE="./metrics/drain.json"

if [ ! -f "$METRICS_FILE" ]; then
  echo "Error: $METRICS_FILE not found. Run ./scripts/seed-simple-demo.sh first."
  exit 1
fi

CURRENT=$(jq -r '.net' "$METRICS_FILE")

if [ "$TARGET" = "toggle" ]; then
  if [ "$CURRENT" = "ONLINE" ]; then
    NEW_STATE="OFFLINE"
  else
    NEW_STATE="ONLINE"
  fi
elif [ "$TARGET" = "offline" ] || [ "$TARGET" = "OFFLINE" ]; then
  NEW_STATE="OFFLINE"
elif [ "$TARGET" = "online" ] || [ "$TARGET" = "ONLINE" ]; then
  NEW_STATE="ONLINE"
else
  echo "Usage: $0 [online|offline|toggle]"
  exit 1
fi

NOW=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

if [ "$NEW_STATE" = "OFFLINE" ]; then
  REASON="WiFi turned off / network interface disabled"
  PROBE_OK=false
else
  REASON="WiFi reconnected / mesh link restored"
  PROBE_OK=true
fi

jq --arg st "$NEW_STATE" \
   --arg prev "$CURRENT" \
   --arg now "$NOW" \
   --arg reason "$REASON" \
   --argjson ok "$PROBE_OK" \
   '.net = $st |
    .at = $now |
    .net_since = $now |
    .last_probe.ok = $ok |
    .last_probe.at = $now |
    .flips += [{at: $now, from: $prev, to: $st, reason: $reason}]' \
   "$METRICS_FILE" > "$METRICS_FILE.tmp"

mv "$METRICS_FILE.tmp" "$METRICS_FILE"

echo "Network state switched: $CURRENT -> $NEW_STATE"
echo "Dashboard at http://127.0.0.1:8787 will reflect this in ~1-2 seconds."
