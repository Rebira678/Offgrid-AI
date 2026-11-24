# ADR 015: Measure growth first; compaction stays unbuilt

**Status:** accepted, 2026-11-23 (Day 49). Does not supersede [ADR 010](010-compaction.md): that design stands as the plan if the alarm below fires. This records the decision not to build it now.

## Context

ADR 010 designed compaction (snapshot plus signed checkpoint, a declared maximum time away) and said to build it only if the pilot's growth alarm fired. Compaction is the riskiest change in the project: it touches the hash chain, per-op signatures and revocation cutoffs, and it has had no outside review. Days 46 to 48 changed what we know about size: the log is small per op, photos are not.

## Decision

Do not build compaction. Ship the alarm (`offgrid store -db F growth`, `scripts/growth-alarm.sh`) and decide from the pilot's own numbers.

- **What an op costs.** A crew-style mix of task, note and status ops takes **565 bytes of database per op unsigned and 767 signed** (`TestWhatOneOpCostsOnDisk`; payload alone is 68). That includes the tasks view, the chain and the indexes. A budget in the test fails the build if either goes over 700 / 950.
- **What that means.** At 200 ops a day a signed log adds 56 MB a year and reaches ADR 010's 500,000-op trigger in 6.8 years; at 1,000 a day it adds 280 MB and reaches the trigger in 1.4 years. The 1 GB database trigger is further off than the ops trigger at every rate.
- **Photos are the growth.** One 3 MB photo a day is 1.1 GB a year; five a day is 5.5 GB. Blob growth, not log growth, is what fills a Pi's card, and it has its own tool already (`gc`, the free-space guard from Day 48).
- **The alarm.** Limits on ops, database size, blob size, growth per day over the last week, and the projected size a year ahead. A projection from under 7 days of history never raises an alarm (it is a guess and says so).

## Rejected

- **Building compaction now.** Cost is high and unreviewed; the benefit is unproven at any measured rate.
- **No alarm.** "Probably fine" is how a card fills at 3 a.m. in a pilot site.

## What it costs

The log cannot shrink. If a crew exceeds the limits, the options are the ADR 010 design (after outside review), or starting a new crew from a signed export, which loses the old log's history.

## What would change it

The alarm firing on real pilot data; a pilot crew above roughly 1,000 ops a day; startup or `store verify` over 30 seconds on the reference Pi.

## Evidence so far

All measured on a 2-vCPU cloud container (Intel Xeon), 3,000 synthetic ops. **Not yet measured:** the pilot's real ops per day, a Pi, a phone. Run `offgrid store -db F growth` on a database from real use and replace the examples above.
