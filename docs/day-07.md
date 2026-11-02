# Day 7: Week 1 recap

A light day. No new concepts in the code; the job is to make the repo something a stranger can run in three commands and understand in a minute.

## What changed

- `README.md` rewritten. It still said "Day 1". Now it has a three-command quickstart, a table of what works per day, the real layout, and the design rules.
- `scripts/demo-offline.sh`: the demo to record. It logs tasks with no model and no network, restarts, shows the tasks again, prints the op log, verifies it, deletes the task table and rebuilds it. `scripts/demo-offline.sh 2` pauses 2 seconds between steps so a screen recording is readable.
- `Makefile`: `make check` (vet + gofmt + tests), `make bench-store`, `make demo`.

## "Done when": a fresh clone runs in three commands

```bash
sudo apt install -y build-essential golang
make build && make test
scripts/demo-offline.sh
```

I checked this by copying the tree without `bin/` into an empty folder and running exactly these. It passed.

## Record the clip

1. Close the browser. Make the terminal font large.
2. Turn WiFi off (the point of the demo): top-right network menu, disconnect.
3. `make demo PAUSE=2` and record with Zorin's screen recorder (or `Ctrl+Alt+Shift+R` on GNOME-based desktops, then open the Files app to find it in Videos).
4. Trim to under 60 seconds. Show the WiFi icon crossed out in the first frame.

## Week 1 recap post (template)

> Week 1 of building Offgrid, an AI agent runtime that works with no internet. Everything below runs on one CPU-only laptop.
> Day 1: measured what local models really do on this machine [your benchmark table here]
> Days 3-4: the model proposes, the code disposes. Nothing a model says is trusted until validated.
> Day 5: simple rules answer first in microseconds; the model only handles what rules can't. [your tier table]
> Day 6: every change is an immutable fact. Delete the table, replay the log, identical state.
> Clean repo, fresh clone runs in 3 commands. [demo clip]
> Next week: connectivity monitor, durable job queue, crash safety.

Use only numbers you measured.

## Self-check

1. Why is an empty benchmark table better than a plausible number? *A made-up number can be quoted back at you; an honest gap can be filled.*
2. In the demo, which step proves the data is not just in memory? *Step 2: a new process lists the tasks.*
3. Which step proves the table is a cache? *Step 5: drop, rebuild, same answer.*

## What Day 8 builds on this

Day 8 adds a goroutine that probes connectivity (ONLINE / DEGRADED / OFFLINE) with hysteresis. It is the first concurrent component, and the offline demo you just recorded becomes the thing it will detect.
