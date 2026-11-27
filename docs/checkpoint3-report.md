# Checkpoint 3: resource governor and node profiles

Decisions: [ADR 017](adr/017-resource-governor-and-node-profiles.md). This report holds what was measured, on what, and what is still to be measured by you.

## Measured so far (rehearsal, not the 24-hour run)

**Hardware:** one cloud container, Intel Xeon (2 vCPU), 8 GiB, Linux 6.18.44 x86_64, Go 1.24.7, offgrid 0.30.1. The "node" and the "laptop" of each run were two processes on that one machine, syncing over loopback with signed ops (a task written every 3 seconds on each side, sync every 2 seconds, samples every 10 seconds). **Not a Pi, not a laptop, not 24 hours.** Each run lasted 12 minutes.

| | light profile | full profile |
|---|---|---|
| Memory limit applied (this process) | 96 MiB | 768 MiB |
| Memory budget set for the soak | 128 MiB | 256 MiB |
| Serve side: RSS start / max / end | 15.6 / 24.4 / 24.0 MiB | 15.6 / 24.6 / 24.0 MiB |
| Run side: RSS start / max / end | 15.7 / 24.6 / 24.6 MiB | 15.5 / 22.6 / 22.3 MiB |
| Peak as a share of the budget | 19% | 10% |
| Goroutines, serve: start / max / end | 6 / 64 / 6 | 6 / 8 / 6 |
| Goroutines, run: start / max / end | 8 / 10 / 8 | 6 / 8 / 8 |
| Sync rounds, failures | 359, 0 | 359, 0 |
| Ops, all signed, chains verified | 481 | 480 |
| Both sides ended with the same state hash | yes | yes |
| Soak verdict | PASS | PASS |

What the numbers say, and do not say:

- Both profiles stayed far inside their budgets (19% and 10%). The two profiles differ in what they allow a **model** to do, and this soak made no model calls, so the memory numbers of the two profiles are the same sync-only numbers. They are a floor, not the cost of a node that also serves the web UI with photos or runs the drain loop.
- RSS climbed from 16 to about 24 MiB in the first five minutes and then went flat. That is caches and SQLite's page cache filling, and it is why the soak does **not** judge a trend on a short run: a line through that climb, extrapolated to a day, looks like a leak (a first version of the trend check failed these two runs for exactly that reason, and was changed to need hours of data; see below).
- The 64-goroutine peak on the light serve side was a burst of simultaneous connections that went away; it ended at 6.
- Same machine for both ends means the network cost of sync is not in these numbers.

## What was built to get a real verdict from a long run

- `offgrid sync stats -max-rss MiB` fails a run whose RSS ever peaked above the budget (the highest sample, warm-up included).
- A trend check fits a line to the steady part, extrapolates 24 hours and fails above 50% growth (`-max-rss-drift`). It is only judged when the steady part spans **2 hours** or more; shorter runs print `trend: not judged` and say why.
- `scripts/real-device/soak.sh` takes `PROFILE=light|full|auto`, `MAX_RSS_MIB=N` and `PPROF=127.0.0.1:6060`.

## Your part: the 24-hour runs

On the Pi (light) and on the laptop (full), two machines, same `DURATION`:

```
# Pi
PROFILE=light MAX_RSS_MIB=128 PPROF=127.0.0.1:6060 MIN_DURATION=20h ./scripts/real-device/soak.sh node  pi.db  key 24h
# Laptop
PROFILE=full  MAX_RSS_MIB=256 PPROF=127.0.0.1:6060 MIN_DURATION=20h ./scripts/real-device/soak.sh laptop lt.db key http://PI:7878 24h
```

Pick the budgets from the first hour of your own numbers if the defaults above do not suit (they are about five times the container peak, which is generous for a sync-only node and may be tight once photos and the web UI are in play). If the verdict is FAIL on the trend, grab profiles while it runs:

```
curl -s http://127.0.0.1:6060/debug/pprof/heap > heap1.pb.gz      # now
curl -s http://127.0.0.1:6060/debug/pprof/heap > heap2.pb.gz      # an hour later
go tool pprof -base heap1.pb.gz heap2.pb.gz                       # what grew between them
curl -s 'http://127.0.0.1:6060/debug/pprof/goroutine?debug=1' | head -50
```

Paste the result blocks here:

| | Pi, light | Laptop, full |
|---|---|---|
| Hardware (CPU, memory, storage) | | |
| Duration | | |
| RSS start / max / end | | |
| Peak share of budget | | |
| Trend (KB/h) and verdict | | |
| Goroutines start / max / end | | |
| Rounds, failures | | |
| Same state hash on both | | |
