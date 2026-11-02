# Day 15: Hybrid logical clock

Every op in the log has a timestamp. Until today it was a plain wall-clock reading. This is the day that stops being good enough, because the next three days (LWW-Map, OR-Set, sync) all ask the same question: **which of two edits came last?**

**Done when:** the tests pass, including skew cases. They do.

## What is in the zip

| File | What it is |
|---|---|
| `internal/hlc/hlc.go` | the clock: `Timestamp{Wall, Counter}`, `Clock.Now`, `Clock.Update`, `Restore`, `Ahead`, `Parse`/`String` |
| `internal/hlc/hlc_test.go` | monotonicity, clock going backwards, skew, counter exhaustion, concurrency, the paper's cases, and a 25-seed multi-node simulation |
| `internal/store/sqlstore/migrate.go` | migration 3: a `counter` column on `ops` and an index on `(ts, counter)` |
| `internal/store/sqlstore/{ops,store,rebuild}.go` | ops are stamped by the HLC; the clock is restored from the log on open; `Info` reports the newest stamp and how far ahead it is |
| `internal/store/sqlstore/hlc_test.go`, `migrate_test.go` | store-level tests (below); the migration tests no longer hard-code a migration count |
| `cmd/offgrid/clockcmd.go`, `clock_test.go` | `offgrid clock demo [-skew 10m]` |
| `cmd/offgrid/storecmd.go`, `store_test.go`, `main.go` | `store ops` shows an `hlc` string; `store info` shows the newest stamp and warns if the log is ahead of the clock; version `0.15.0-day15` |
| `ARCHITECTURE.md`, `README.md`, `Makefile` | updated (README, Makefile and ARCHITECTURE overwrite yours: merge by hand if you edited them) |
| `docs/day-15.md`, `docs/day-15-hlc.{svg,png}` | this file and the diagram |

## Concepts

**A wall clock is a claim, not a fact.** A phone can be minutes slow, be set back by an automatic correction, or lose its time on a dead battery. If "last writer wins" compares wall clocks, a later edit made on a slow phone loses to an earlier one, and nothing complains. The data is just wrong.

**A logical clock fixes ordering, not time.** A Lamport clock only counts events, so it is always consistent with cause and effect, but it has no relation to real time: you cannot say "show me edits from yesterday".

**A hybrid logical clock keeps both.** A stamp is a pair `(wall, counter)`. `wall` stays close to real time; `counter` breaks ties. The rules, from Kulkarni et al.:

- Local event or send: `wall' = max(wall, physical)`. If `wall'` did not change, `counter + 1`, otherwise `counter = 0`.
- Receive a stamp from another node: `wall' = max(wall, remote wall, physical)`. The counter becomes one more than the larger of the counters that share that wall value, or 0 if the physical clock won.

Three guarantees follow, and the tests check each:

1. **Monotonic.** One node's stamps only increase, even if its physical clock stands still or goes backwards.
2. **Causal.** If A happened before B (B received A's stamp), `stamp(A) < stamp(B)`, whatever the physical clocks say.
3. **Bounded.** `wall` never runs ahead of the fastest physical clock in the system.

**What it cannot do:** order two edits that really were concurrent. It orders them deterministically (and the same on every replica), which is exactly what last-writer-wins needs, but it is a tie-break, not the truth.

## Decisions

- `Wall` is nanoseconds (int64), `Counter` is uint32. If the counter ever runs out, the wall moves forward by 1 ns instead of wrapping. Order is kept.
- `Update` **refuses** a remote stamp more than 1 hour ahead of the local clock (`DefaultMaxDrift`; `NoDriftLimit` turns it off) and leaves the clock unchanged. One bad phone must not pull everyone's clock into next year. The cost is real: edits made here afterwards are ordered before that stamp, so on a conflict the remote value wins. The data is not dropped.
- The store restores the clock from the newest stored stamp on open, so a restart with a clock that is behind never issues an older stamp.
- **Migration 3 rewrites nothing.** It adds `counter` (default 0) and an index. Old ops keep their exact times (their `ts` was already a wall reading), the log stays append-only, and no projection version bump is needed.

## What was checked

- `go test -race ./...` passes; `gofmt` and `go vet` are clean.
- The multi-node simulation (25 seeds, clock offsets of ±5 s and clock steps) sent 66,677 messages: the HLC ordered all of them correctly; ordering by raw physical time got 33,190 wrong.
- Mutation testing on `internal/hlc` and the store's stamping and restore code: every mutant was caught by a test except four that cannot change behaviour (`>=` vs `>` where the difference is 0, a clock at exactly -1 ns, and an `ORDER BY` that the new index already satisfies). Two real gaps were found and closed by new tests: the pre-epoch case and the newest-stamp ordering.
- Upgrade path: a Day 6-style version 2 log opens, keeps its times, and new ops sort after old ones.

## Honest limits

- The demo and the simulation use **fake clocks**. No real devices, no real network skew were involved.
- A clock that was once far ahead drags the HLC with it. `offgrid store info` prints a warning when the newest stamp is more than a minute ahead of this computer's clock.
- There is no sync yet, so no two replicas have exchanged stamps outside tests. `Clock.Update` exists and is tested; Day 19 is where it gets used.

## Try it on your laptop

```bash
make clock-demo                      # or: bin/offgrid clock demo -skew 10m
bin/offgrid clock demo -skew 0s      # no skew: wall clock and HLC agree
go test -race ./internal/hlc
bin/offgrid exec -db /tmp/d.db '{"intent":"create_task","title":"Pump"}'
bin/offgrid store -db /tmp/d.db ops  # each op has "hlc": "2026-...Z#0"
bin/offgrid store -db /tmp/d.db info # newest timestamp
```

## Post draft: "Why your phone's clock can't be trusted for sync, and the fix"

> Two people edit the same task. Alice at 10:00. Bob a minute later, after he's seen her edit. But Bob's phone is 10 minutes slow, so his edit is stamped 9:51.
>
> "Last writer wins" by wall clock: Alice wins. Bob's later edit disappears. No error. Just wrong data.
>
> The fix is a hybrid logical clock (Kulkarni et al.). A timestamp is (wall time, counter). When you receive someone's timestamp, your clock jumps to at least that. So Bob's edit is stamped 10:00:00 #2, after Alice's, whatever his phone thinks.
>
> Today for Offgrid: the clock, tests for clocks going backwards and running fast, a 25-run simulation where raw wall time misordered 33k of 66k messages and the HLC got 0 wrong, and a one-command demo. It's all simulated; real devices come later.
>
> Picture: `docs/day-15-hlc.png`. Code: github link.

Hooks: distributed-systems people, CTOs who have shipped offline apps.

## Self-check

1. Why is a wall clock alone not enough to decide which edit came last?
2. What does the counter do, and when does it reset to 0?
3. Why does `Update` refuse a stamp from a year ahead, and what does that cost?
4. Why did migration 3 not need to rewrite any ops?
5. Why can't an HLC tell you which of two concurrent edits "really" came last?

## Tomorrow: Day 16, LWW-Map

Per-field last-writer-wins registers for tasks: each field of a task keeps the value with the largest HLC stamp, ties broken by node ID, as a pure merge function that can be tested without a database.
