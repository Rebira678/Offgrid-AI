# Day 18: OR-Set

Yesterday's registers handle fields: one value, last writer wins. Notes are different. People add notes; nobody overwrites one. And sooner or later someone will want to delete a note or a task. This is the type for things that are added and removed, and it answers the question every offline-first user eventually hits: **what happens when I delete something while you edit it?**

**Done when:** tests cover concurrent add and remove of the same item. They do, as example tests, as properties over thousands of generated cases, and in the demo.

## Needs the same dependency as Day 17

`go get pgregory.net/rapid@v1.2.0` (once, and commit `go.mod`/`go.sum`) if you have not already.

## What is in the zip

| File | What it is |
|---|---|
| `internal/crdt/orset.go` | `Tag`, `Set`, `Add`, `Remove`, `RemoveTags`, `Observed`, `Has`, `Elements`, `MergeSet`, `EqualSet` |
| `internal/crdt/tasks.go` | notes as set elements: `NoteElement`, `SplitNoteElement`, `TaskNotes` |
| `internal/crdt/orset_test.go` | the concurrent add/remove scenarios, tombstone-before-add, notes |
| `internal/crdt/orset_props_test.go` | 8 laws, an oracle-based convergence property, 3 more properties; 3000 cases each |
| `internal/crdt/orset_broken_test.go`, `broken_test.go` | the same laws catching five broken set merges (`runProp` factored out of `runLaw`) |
| `internal/store/sqlstore/fields.go`, `notes_test.go` | `NotesFromOps`, `Store.Notes`; notes tests incl. two forked replicas and two properties over real logs |
| `cmd/offgrid/orsetcmd.go`, `orset_test.go`, `main.go` | `offgrid orset demo`; version `0.18.0-day18` |
| `internal/drain/worker_test.go` | **flake fix** in the Day 12 `takeOver` test helper (below) |
| `scripts/prove-merge.sh` | now also runs the set properties and the broken set merges |
| `ARCHITECTURE.md`, `README.md`, `Makefile` | updated (they overwrite yours: merge by hand if you edited them); `make orset-demo` |
| `docs/day-18.md`, `docs/day-18-orset.{svg,png}` | this file and the diagram |

## Concepts

**Why not last-writer-wins for a list?** Two people add different notes: with one register the later one would erase the earlier. A list needs a set-like type where adds accumulate.

**Why remove is the hard part.** "Remove x" must mean something that survives merging. If a remove says "x is gone", it also destroys an add of x that happened elsewhere and that the remover never saw. If it says nothing durable, removed items come back when a stale copy syncs.

**The observed-remove idea.** Every add gets a unique **tag**: the node that made it plus its hybrid logical timestamp (so no two adds ever share one). To remove x, a replica looks at which tags of x it can see and writes a **tombstone** for each of those tags. An element is in the set while at least one of its tags has no tombstone.

So a remove only covers what its author had *observed*. An add made somewhere else, which the remover never saw, has a tag no tombstone mentions, and it survives. That is **add-wins**: deleting a task does not destroy an edit you had not seen.

**Merge is a union.** The union of the adds and the union of the tombstones. Union is commutative, associative and idempotent, so the three laws from Day 17 hold by construction. Membership is computed from both sets, so a tombstone that arrives *before* the add it refers to still works. Delivery order does not matter.

**Edit as re-add.** In the demo and diagram a task edit is modelled as touching the task, which adds it again with a new tag. That is the model for "delete while edited": the delete hits the tags it saw, the edit brought a new one. The store does not delete tasks today, so this is a model, not a feature (see limits).

**Notes are elements, each with its own tag.** Two people writing "ok" are writing two notes, so a note's element embeds its add's tag: `note/<task>/<wall>.<counter>.<node>` then a newline and the text. That keeps identical texts distinct, keeps notes sortable by time, and lets text hold newlines or anything else.

## How it connects to what exists

`Store.Notes` reads the op log and builds the set: each `task.note_added` op is one add, tagged with the writing node and the op's HLC stamp. The test `TestNoteSetAgreesWithTheMaterializedViewOnRandomWorkloads` checks that, over random workloads, the set lists exactly the notes (and order) the SQLite view lists, for every task. Two forked replicas that each add notes get all notes, in time order, after merging; and a note removed on one replica while another adds a new one leaves the new note.

## What was checked

- `gofmt`, `go vet` and `go test -race ./...` pass.
- Examples: concurrent add and remove (both merge orders); a stale copy cannot bring a removed element back; re-adding after a remove; a tombstone arriving before its add; several tags of one element removed by one remove; no input is changed.
- Properties (3000 generated cases each): commutative, associative, idempotent, identity, no add or tombstone is lost, **add wins**, **a remove holds against a stale copy**, purity; replicas that run random adds, removes and partial syncs converge to the oracle (an independent computation from the history: live = adds not observed by any remove); sync order and repeated delivery do not matter; note elements round-trip for arbitrary text.
- The same laws catch five broken set merges: tombstones not merged, tombstones intersected (removed items resurrect), adds intersected (new items lost), remove-wins, and in-place edits.
- Mutation testing of the set, the note encoding and the store projection: every behaviour-changing mutant was caught after I added tests for four survivors (map-order-dependent sorting, the last-slash split, the stamp length check and a separator check, and same-stamp ordering by node).

## A flaky test, found and fixed

While running the whole suite under load, a Day 12 drain test failed once (`takeover claim: ok=false`), and under artificial CPU load two lease tests failed within seconds. Cause: the test helper `takeOver` expired a job's lease and then called `Claim`. In the gap, the worker under test (which also polls for work, and renews short leases) could claim or renew the job itself. Nothing in the queue was wrong: the fencing did its job. The helper was. It now makes the takeover in one SQL statement, the same attempt-number bump `Claim` makes. The two lease tests then pass 25 runs in a row under four busy-loop processes. Only the test file changed.

## Honest limits

- **The store cannot remove a note or a task yet.** There is no remove op, no `RemoveNote`, and no CLI for it. The set type supports removal and is tested thoroughly, and `Store.Notes` reads adds only. Writing removal ops belongs with the sync work on Days 19 and 20 (a remove op must carry the tags it observed, which `Set.Remove` returns).
- **The delete-a-task story is a model**: the task set and the "edit is a re-add" rule are exercised in tests and the demo, not in the store.
- **Tombstones are never forgotten**, so a set only grows. Cleaning them up needs to know that every replica has seen them, which the sync layer has to establish.
- Task IDs and per-replica note numbers still collide across replicas (see Day 16); notes are keyed by their tags, which do not.
- The demo is a simulation with fake clocks.

## Try it on your laptop

```bash
make orset-demo                         # or: bin/offgrid orset demo
go test ./internal/crdt -run 'SetMerge|Oracle|Concurrent' -v
make prove-merge                        # now includes the set laws and the broken set merges
```

## Post draft: "What happens when I delete a task while you edit it?"

> You delete a task on your phone, offline. I edit the same task on mine, offline. We reconnect. What should happen?
>
> The obvious answers are both bad. "The later one wins" needs a trustworthy clock (phones don't have one). "Delete always wins" throws away my edit and I never find out.
>
> Offgrid's answer: every addition gets a unique tag, and a delete only removes the tags it had actually seen. Your delete saw the original task. My edit created something new that your delete never saw. After syncing, the task is still there, with my edit. If I hadn't touched it, the delete holds.
>
> No clock decides it. The question is "what had you seen when you deleted?". That's an observed-remove set; the rule is called add-wins.
>
> The cost: deleted things leave tombstones that are kept for now, so the data only grows until I build a safe cleanup. Simulated in `offgrid orset demo`; the merge is checked by thousands of generated cases and by deliberately broken versions it must reject. Picture: docs/day-18-orset.png.

Hook: product-minded founders. It is a real user problem.

## Self-check

1. Why does a remove write tombstones for tags instead of marking the element as gone?
2. Walk through Lidia deleting T1 while Beka edits it. Which tags exist, which have tombstones, and why does T1 survive?
3. Why does a tombstone arriving before its add still work?
4. Why do two notes with identical text have to be two elements?
5. What would it take to delete tombstones safely?

## Tomorrow: Day 19, sync protocol

Version summaries per node, delta exchange of only the missing ops, `/sync/summary` and `/sync/ops` endpoints, idempotent apply, and minimal HMAC authentication. This is where op removal, task IDs that include the node, and the HLC's `Update` finally get used for real.
