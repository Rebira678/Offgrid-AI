# Day 24: a deterministic simulation test

**Done when:** 1,000+ seeds pass in a CI-friendly time, and a failing seed is exactly reproducible. Both hold, and the simulator found a real bug on its first run.

```bash
make sim                       # 1,000 random failure schedules (SEEDS=3000 make sim for more)
bin/offgrid sim -seed 968      # replay one seed, with its whole trace
bin/offgrid sim -replay examples/day24-id-collision.txt
```

## What a seed is

A seed is turned into a plain list of **actions** before anything runs. The list is the whole schedule. The run itself has no goroutines, no real clock and no real network, so one seed always gives the same trace and the same final hash. (A test runs four seeds twice and compares every trace line. It failed on my first try: I was printing a memory address into the trace. Fixed, and the test stays.)

Each schedule runs three replicas on real SQLite files, with the real op log and the real sync functions. Per schedule, 80 actions drawn from:

| Action | What it does |
|---|---|
| create, update, note, delete | a person edits on one replica. Titles repeat on purpose, so replicas collide. |
| suggest, resolve | an AI proposes, a person accepts or dismisses (Day 23) |
| skew | a replica's clock is set up to ±90 minutes off true time |
| crash | the replica is closed and reopened from its file, and a third of the time its clock restarts up to 40 hours in the past (a dead battery) |
| partition, heal | a link between two replicas goes down or up |
| sync | one replica pulls from another, 1 to 4 rounds, in batches of 1 to 7 ops, and the reply may be **dropped, duplicated, delayed** (delivered at a later sync), **cut short, reversed, or missing its first op** |
| wait | time passes |

Actions never name state. "Update" means "the N-th task that exists when this runs", so **any subset of a schedule is still a valid schedule**. That is what lets the shrinker delete actions freely.

## What is checked

While running:

- **Causality, after every edit.** The new op must sort after every op that replica already held. If a restart or a received stamp left the clock behind, a later edit would silently lose to an earlier one.
- **A refused batch changes nothing.** After any refusal the replica holds exactly what it held, and for a batch with a hole in it the state hash is unchanged too. Those refusals must be a gap or bad-op error.
- A sync refused for clock drift is allowed (it is the rule: a stamp over an hour ahead is refused). Any other refusal is a failure.

At the end, after every link is healed, clocks are corrected, four hours pass, and every pair has synced reliably until nothing moves:

1. every replica holds the same ops and the same state hash;
2. each live view equals a replay of its own log (`Verify`), and rebuilding the view changes nothing;
3. a **fresh replica fed the same ops in another, random order** (an op that needs another replica's op waits its turn) reaches the same hash;
4. two decisions **recomputed from the raw log** match the store's view: whether each deleted task is alive (a delete only beats edits it had seen), and the outcome of every suggestion (the first resolution in fold order). These matter because convergence alone passes a store whose replicas all agree on the wrong answer.

## The bug it found, on the first run

Before Day 24 a task's ID was `T<n>` while a replica had only its own ops. Two replicas that each create a task before they have ever met both choose `T1`. When they meet, the sync is refused, and it is refused again every time. The old error message said to start the second replica from an empty database, which means throwing away its work.

On the Day 23 code, 744 of the first 1,000 seeds hit it. The shrinker cut the first failing schedule from 80 actions to 3:

```
create r2
create r1
sync r2-r1
```

(`examples/day24-id-collision.txt`; it fails on the Day 23 code and passes now.) This is the headline offline scenario: a phone and a laptop that both start working before they meet.

**The fix:** a new task's ID is always `T<n>.<replica>` (for example `T3.laptop`), so two replicas cannot choose the same one without agreeing on anything. Screens show `T3`, and typing `T3` finds it when exactly one task has that number (two tasks that would show the same short ID keep their full IDs). IDs already in a database stay valid, and two old databases that did collide are still refused with the same error and left unchanged. About 30 tests that hard-coded `"T1"` were updated, and a new test pins the new behaviour. The README and the Day 4 and 5 demos still print `T1`, because the display is shortened.

Only that one bug was found. After the fix, nothing else broke in the sweeps below.

## Results (this sandbox: 2 CPU cores, `/dev/shm`)

| Sweep | Schedules | Result | Time |
|---|---|---|---|
| default, from nothing, seeds 1..3000 | 3,000 | all converged | 247 s |
| `-shared` (one common first task), seeds 10001..11000 | 1,000 | all converged | 109 s |
| 5 replicas, 120 actions, seeds 20001..20300 | 300 | all converged | 53 s |

Over the 3,000: 77,777 edits, 72,105 syncs (4,759 dropped, 4,895 duplicated, 4,828 delayed, 2,259 cut short, 5,516 reversed, 4,087 with a hole), 12,090 partitions, 9,680 clock skews (5,546 syncs refused as too far ahead), 9,428 crashes and restarts, and 21,038 refolds of the view. 65,769 ops converged in total.

About 12 schedules a second on 2 cores, so **1,000 seeds is about 80 seconds here**. Schedules are independent, so the speed scales with cores (`-j`). I have not timed it on your machine. The unit test (`go test ./...`) runs 150 seeds (60 with `-short`; `SIM_SEEDS=N` overrides).

## Does the simulator notice bugs? Deliberate-bug runs

I put ten bugs into the store, one at a time, and ran the sweep against each:

| Bug put in | Caught by | Seeds failing |
|---|---|---|
| a late op no longer triggers a refold of the view | convergence | 273 of 300 |
| sync batches no longer in fold order | sync / convergence | 284 of 300 |
| a duplicated batch is no longer skipped | sync | 149 of 300 |
| the clock is not moved forward by received stamps | causality | 216 of 300 |
| the clock is not restored from the log on restart | causality | 173 of 300 |
| a delete always beats concurrent edits | delete rule (log oracle) | 178 of 300 |
| delete off by one: an edit the deleter had just seen counts as unseen | delete rule (log oracle) | 76 of 300 |
| a gap in a batch is accepted | verify / convergence | 51 of 300 |
| a later resolution of a suggestion overwrites the first | suggestion rule (log oracle) | 12 of 300 |
| the tie-break between nodes is dropped from the fold order | convergence | **1 of 1,000** (seed 968) |

All ten are caught. Three were **not** caught by my first version of the simulator (delete off by one, a gap accepted, the later resolution winning). Each exposed a gap in the checks, which is why the log oracles and the "hole in the reply" fault exist now. The weakest catch is the last: it needs two replicas to stamp ops with the same time, counter and sequence number, which is rare in these schedules. Treat that kind of bug as only partly covered.

## What this does not show

- It checks convergence plus the rules above. A store that converged on a wrong answer that none of the checks recompute would pass.
- No disk that loses committed writes (SQLite with `synchronous=FULL` is assumed to keep them), no replica restored from an old backup (that forks its log; sync refuses forks on purpose), no hostile peer, no network below the sync functions (no TCP, no HMAC; those have their own tests).
- Clock skew is bounded at ±90 minutes plus the crash resets, and the schedule ends by correcting every clock. A replica whose clock stays hours wrong is refused by the others, by design.
- One schedule is 80 actions and 3 replicas (5 and 120 tested). Bugs that need long histories are less likely to show.
- The sync run here calls the store directly; the 1,000 schedules do not use HTTP.

## The post (Day 24)

> I ran 3,000 random failure schedules against my offline sync engine. Here's the one that broke it.
>
> Each seed throws partitions, clock skew, crashes and lossy sync (drops, duplicates, delays, reordering, holes) at three real replicas, then checks they converge. Same seed, same run, every time.
>
> It found a real bug on its first run: two devices that each create a task before ever meeting both call it T1, and then sync is refused forever. 744 of the first 1,000 seeds hit it. The shrinker cut an 80-action schedule to three:
>
>     create r2
>     create r1
>     sync r2-r1
>
> The fix is that task IDs carry the device that made them. Now 3,000 schedules pass.
>
> To check the simulator can see bugs, I put 10 deliberate ones into the store. It caught all 10, one of them only on seed 968 of 1,000. Three got past my first version, so I added checks recomputed from the raw log.

Hook for distributed-systems readers: *a seed is a bug report; shrinking turned 80 actions into 3.* Attach `docs/day-24-simulation.png`.

## Checklist

- [ ] `go get pgregory.net/rapid@v1.2.0`, commit `go.mod` and `go.sum` (still needed from Day 17).
- [ ] `make sim` (about 80 s on 2 cores; faster on yours). Put your own time in the post if you quote one.
- [ ] Existing databases keep working. New tasks get IDs like `T4.<your node>`; screens show `T4`. `offgrid store -db FILE ops` shows the full ID.
- [ ] Optional: see the bug on your Day 23 tree. Add `internal/sim`, `cmd/offgrid/simcmd.go` and the `sim` case in `main.go` to a copy, then `offgrid sim -replay examples/day24-id-collision.txt`. It fails there and passes in this zip.
- [ ] 15 minutes of outreach.
