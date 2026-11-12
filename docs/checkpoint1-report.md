# Checkpoint 1 report: two real devices, signed ops, one-hour soak

**Status: NOT YET RUN.** Nothing in this file has happened on real devices. Fill a row only after you ran it, with the date and the hardware named. The roadmap's rule for this day: **gate on reality**. If the two devices do not sync cleanly, stop building (Days 38 onward) and fix.

How to run: [`docs/day-37.md`](day-37.md). The script: `scripts/real-device/soak.sh`.

## Hardware (name it; numbers without it do not count)

| | Laptop | Second device |
|---|---|---|
| Model | HP EliteBook 845 G8 | |
| CPU, RAM | | |
| Storage | | *(SD card brand and class / SSD)* |
| OS and architecture | Zorin OS, amd64 | |
| Network | | *(router model, WiFi band, distance, is the router shared with other traffic?)* |
| `offgrid version` | | |
| Date and start time of the soak | | |
| Device IDs (`offgrid identity show`) | | |

## Gate

| Check | Result | Evidence |
|---|---|---|
| Two devices synced over real WiFi, nothing refused | *(pass / fail / not run)* | |
| Both `sync summary` state hashes identical once both were quiet | | paste both lines |
| `store verify` on both: **every op signed, by 2 devices, no WARNING** | | paste `ok: N of N ops are signed, by 2 device(s)` |
| The shared-key warning appeared on `sync serve` and `sync run` | | |
| One-hour soak: RESULT line of **each** side | | paste both |
| **Gate decision** | **PASS / FAIL** | if FAIL: what broke, and the fix before Day 38 |

## One-hour soak, per device

| | Laptop (`sync run`) | Second device (`sync serve`) |
|---|---|---|
| Duration, samples | | |
| Goroutines: start / max / end / steady | | |
| RSS KB: start / max / end / steady | | |
| Rounds, failed rounds | | *(serve counts none)* |
| Ops at the end | | |
| `RESULT:` | | |

The verdict uses the limits written down before the run: **10% warm-up ignored, goroutines may grow by at most 5, RSS by at most 20%, at least 10 minutes and 20 samples.** Change a limit only with a reason written here, not because a run failed.

## What the sandbox already showed (not a substitute for the table above)

See the "What I measured here" section of [`day-37.md`](day-37.md): an in-process soak on one x86 cloud machine, and a two-process loopback rehearsal.

## Everything that broke or surprised you

| # | What happened | How you noticed | Fixed? (commit) | Became a test? |
|---|---|---|---|---|
| 1 | | | | |
| 2 | | | | |
