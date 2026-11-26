#!/usr/bin/env bash
# The nightly model check on your own machine (Day 50). Cron it, with the machine idle:
#   30 2 * * *  cd /home/you/offgrid && scripts/eval-nightly.sh
#
# Runs the 105 labelled cases against a REAL model, appends a row to docs/eval/history.csv, writes
# docs/eval/latest.md, and compares with docs/eval/baseline-real.json if you have committed one.
# Exit 4 means a rate fell more than MARGIN below the baseline.
#
#   MODEL=llama3.2:3b   HOST=llamacpp@http://192.168.1.20:8080   MARGIN=0.05   ABLATION=1
#
# To make today's numbers the baseline (after you have looked at them and accept them):
#   offgrid eval -models "$MODEL" -write-baseline docs/eval/baseline-real.json
set -u
cd "$(dirname "$0")/.."
model="${MODEL:-llama3.2:3b}"
margin="${MARGIN:-0.05}"
mkdir -p docs/eval
args=(eval -models "$model" -md docs/eval/latest.md -record docs/eval/history.csv -margin "$margin")
[ -n "${HOST:-}" ] && args+=(-host "$HOST")
[ -n "${ABLATION:-}" ] && args+=(-ablation)
[ -f docs/eval/baseline-real.json ] && args+=(-baseline docs/eval/baseline-real.json)
exec "${OFFGRID_BIN:-offgrid}" "${args[@]}"
