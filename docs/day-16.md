# Day 16: LWW-Map

Two people edit the same task while apart. When they meet, both must end with the same task, with no server and no one deciding. Today builds the rule.

**Done when:** unit tests cover concurrent edits to the same field. They do (and more).

## What is in the zip

| File | What it is |
|---|---|
| `internal/crdt/lwwmap.go` | `Entry`, `Map`, `Merge`, `Put`, `Delete`, `Get`, `Equal`: the whole algorithm, pure |
| `internal/crdt/tasks.go` | task fields as registers: key `task/<id>/<field>`, `TaskFields` |
| `internal/crdt/lwwmap_test.go` | the concurrent-edit scenarios, purity, total order, 200-seed convergence |
| `internal/store/sqlstore/fields.go`, `fields_test.go` | `FieldsFromOps` and `Store.Fields`: the op log read as registers; tests against the real view and two forked replicas |
| `cmd/offgrid/lwwcmd.go`, `lww_test.go` | `offgrid lww demo`; `offgrid store -db F fields` |
| `cmd/offgrid/storecmd.go`, `main.go` | the `fields` subcommand, usage, version `0.16.0-day16` |
| `ARCHITECTURE.md`, `README.md`, `Makefile` | updated (they overwrite yours: merge by hand if you edited them) |
| `docs/day-16.md`, `docs/day-16-lww.{svg,png}` | this file and the diagram |

## Concepts

**Per field, not per task.** If the whole task were one value and the later edit won, Alice changing the priority and Bob changing the status would cost one of them their change. Each field is its own register, so both survive. Only two edits to the *same* field compete.

**Each register keeps one entry:** the value, the HLC stamp it was written at, and the node that wrote it. Merging two maps keeps, per key, the entry that sorts last.

**The order must be total.** Stamp first (Day 15), then node ID when the stamps are exactly equal, then `Deleted`, then the value. Because every pair of entries has a definite order, "keep the larger" gives the same answer whichever replica merges, in whatever order, any number of times. Those are the three laws (commutative, associative, idempotent) that make a state-based CRDT converge. Day 17 proves them with generated tests; today's tests check them on examples and on 200 random runs.

**The node-ID tie-break is arbitrary and that is fine.** Two edits stamped at the same instant on different replicas are concurrent: neither is truly later. Any rule works if every replica applies the same one. "bob" sorting after "alice" has no meaning.

**A losing edit is overwritten, not wrong.** It is as if the other person had typed afterwards. That is the cost of last-writer-wins: no conflict is ever shown to a person. For a priority or a status that is usually acceptable; for text people write together it is not (notes get a different type on Day 18).

**Pure merge.** `Merge(a, b)` reads no clock, does no I/O, and never changes `a` or `b`. The result shares nothing with them. This is tested, including that mutating a result does not change an input.

**Deletes are tombstones.** `Delete` writes an entry with `Deleted: true` at a stamp. It must stay in the map: if it were removed, an older write arriving late would bring the key back.

## How it connects to what exists

The op log already carries what a register needs: every op has an HLC stamp and a node ID. `Store.Fields` reads the log and builds the registers: `task.created` writes title, site, priority and status; `task.updated` writes the fields that changed; notes are skipped. The test `TestFieldRegistersAgreeWithTheMaterializedViewOnRandomWorkloads` checks that, over random workloads, the registers say exactly what the SQLite view says for every task. Nothing in the existing store changed.

## What was checked

- `gofmt`, `go vet` and `go test -race ./...` pass.
- Concurrent edits: same field (later stamp wins, both merge orders), same wall with different counter, exactly equal stamps (node ID decides), different fields (both kept), three replicas in any order.
- Two real stores forked from one history, each edited, then merged: converge; a tie between them is decided by node ID; a replica whose clock is slow loses until it has received the other's stamp (the test pins this honestly, see below).
- Mutation testing on `internal/crdt` and `Fields`: every behaviour-changing mutant is caught. Equivalent survivors: `>=` instead of `>` in `Put` (an equal entry is identical, so replacing it changes nothing) and a paging variant that re-reads rows (put is idempotent). Three real gaps were found and closed: a test copy that aliased its input, paging across pages, and the "field is after the last slash" rule.

## Honest limits

- **Task IDs collide.** `T1` is a per-replica counter. Two replicas that each create a task both make `T1`. The registers are keyed by it, so they would merge two different tasks into one. Fixing that (IDs that include the node, or a mapping layer) comes with sync on Days 19 and 20. The forked-store tests here share one history on purpose.
- **A slow clock still loses until it has seen the other side's stamp.** The HLC cannot know about an edit it has not received. After a sync, the next edit is ordered after it.
- **No sync yet.** Nothing moves ops between replicas, and the store's own view is not yet rebuilt from merged registers. Today builds and proves the merge rule; Days 19 and 20 use it.
- Notes are not registers. The demo and diagram are simulations with fake clocks.

## Try it on your laptop

```bash
make lww-demo                         # or: bin/offgrid lww demo
go test -race ./internal/crdt
bin/offgrid exec -db /tmp/d.db '{"intent":"create_task","title":"Pump","priority":"low"}'
bin/offgrid exec -db /tmp/d.db '{"intent":"update_task","task_ref":"pump","priority":"high"}'
bin/offgrid store -db /tmp/d.db fields   # one line per register: value, winning stamp, node
```

## Post draft: a two-person conflict, in plain language

> Alice and Bob both fix the same task while offline. Alice sets priority to low. Bob sets it to high, and also marks it in progress. They reconnect. Who wins?
>
> Offgrid's rule: every field of a task is its own little register, with the time and the name of whoever wrote it. When two copies meet, for each field keep the later one.
>
> So the priority becomes Bob's "high" (he wrote later), the status stays Bob's too, and nothing Alice did to other fields is lost. If two edits carry exactly the same time, the names break the tie, the same way on both phones. No server, no asking anyone, no order that matters.
>
> Alice's edit isn't an error: it's treated as if Bob typed after her. That's the trade-off of "last writer wins", and why I'm giving notes a different structure on Day 18.
>
> Simulated in `offgrid lww demo`; the merge is a pure function, so tomorrow I can attack it with thousands of random cases. Picture: `docs/day-16-lww.png`.

Hook: anyone, because it needs no jargon.

## Self-check

1. Why keep one register per field instead of one per task?
2. What are the four parts of the order, and why does a tie need all of them?
3. Why must a delete stay in the map as a tombstone?
4. Why can Alice's edit lose even though she did nothing wrong?
5. Why will two replicas that each create `T1` break the registers, and when is it fixed?

## Tomorrow: Day 17, prove the merge is correct

Property-based tests with `pgregory.net/rapid`: merge is commutative, associative and idempotent over thousands of random maps. Then break the tie-break on purpose and show the tests catch it. (This needs the `rapid` module: if your machine has network access, `go get pgregory.net/rapid` there.)
