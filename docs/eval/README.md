# Model evaluation records

- `history.csv`: one row per model, per setup, per run. Header in `eval.HistoryHeader`. Written by `offgrid eval -record` (the nightly job and `scripts/eval-nightly.sh`). On GitHub the nightly job writes it to the `eval-history` branch so the main branch is not touched every night.
- `latest.md`: the last report.
- `baseline-real.json`: the numbers a real model was last accepted at (create it with `offgrid eval -models M -write-baseline docs/eval/baseline-real.json`). Not committed yet: it needs a real run on your hardware.
- The CI reference baseline is `internal/eval/baseline.json`; it measures the pipeline, not a model.
