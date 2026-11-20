#!/usr/bin/env bash
# Day 45: compare two runs made by scripts/perf/bench.sh.
#
#   scripts/perf/compare.sh OLD.txt NEW.txt
#
# Prints benchstat's table when benchstat is installed (go install golang.org/x/perf/cmd/benchstat@latest),
# then runs the gate (cmd/perfgate), which is what can fail: regressions beyond the thresholds in
# internal/perfgate, and budgets in perf/budgets.json (enforced only on the hardware they name).
#
# To get past a failing gate on purpose, say why in words:
#   PERF_OVERRIDE="trading 12% on Rebuild for crash safety" scripts/perf/compare.sh OLD.txt NEW.txt
set -euo pipefail
old="${1:?usage: scripts/perf/compare.sh OLD.txt NEW.txt}"
new="${2:?usage: scripts/perf/compare.sh OLD.txt NEW.txt}"
cd "$(dirname "$0")/../.."
if command -v benchstat >/dev/null 2>&1; then
  benchstat "$old" "$new" || true
  echo
else
  echo "(benchstat is not installed: go install golang.org/x/perf/cmd/benchstat@latest for the full table)"
  echo
fi
go run ./cmd/perfgate -old "$old" -new "$new" -budgets "${BUDGETS:-perf/budgets.json}"
