# Day 20: Two replicas converge (Checkpoint 2)

Day 19 moved ops between replicas. Today is the proof: two replicas lose their connection, both edit the same task, add and delete things, reconnect, and **end with identical state, checked by comparing state hashes.**

**Done when:** both replicas end with identical state hashes. They do: `scripts/converge-demo.sh` (real processes), `TestCheckpoint2TwoReplicasEditOfflineAndConverge` (in-process, through the fault-injection package) and `TestCheckpoint2DemoScript` (runs the script from `go test`).

## Needs

- Nothing new to install for the demo. `scripts/converge-clip.sh` needs `tmux` (`sudo apt install tmux`).
- If you have not yet: `go get pgregory.net/rapid@v1.2.0`, and commit `go.mod` and `go.sum`.

## What is in the zip

| File | What it is |
|---|---|
| `internal/store/sqlstore/{ops,apply,store,fields,sync,rebuild}.go` | `task.removed` and `note.removed` ops; `DeleteTask`, `RemoveNote`; the view's delete rules (projection version 2) |
| `internal/store/sqlstore/delete_test.go` | delete and note-removal tests, a random three-replica run against an independent oracle |
| `internal/task/{store,mem}.go`, `internal/task/storetest/storetest.go` | the optional `task.Remover` interface; the in-memory store implements it; one shared contract test for both stores |
| `internal/syncer/chaos_test.go` | Checkpoint 2 through `netfault`; a network that loses requests and responses; a cable cut at every point of a round |
| `cmd/offgrid/taskcmd.go`, `task_test.go` | `offgrid task -db F list / delete REF / remove-note REF N` |
| `cmd/offgrid/synccmd.go`, `sync_test.go`, `storeopen.go`, `storecmd.go`, `main.go` | `offgrid sync run` (keeps syncing, takes the `-fault-*` flags), `$OFFGRID_NODE`, version `0.20.0-day20` |
| `scripts/converge-demo.sh`, `scripts/converge-clip.sh` | the scenario with real processes; the same as a tmux split-screen clip |
| `README.md`, `Makefile`, `ARCHITECTURE.md` | updated (they overwrite yours: merge by hand if you edited them); `make converge-demo` |
| `docs/day-20.md`, `docs/day-20-converge.{svg,png}` | this file and the diagram |

## What the scenario does

Replica A serves. Replica B runs `offgrid sync run` through the **chaos switch**: `offgrid fault set partition` is its WiFi off, `offgrid fault set ok` is WiFi on. Both are real `offgrid` processes with their own SQLite files on localhost.

1. A holds three tasks, one with a note. B starts empty and catches up.
2. **WiFi off.** B's loop prints `OFFLINE` and keeps trying.
3. While offline, A: finishes T1, adds a note to T1, deletes T2 and T3, creates "Fuel line". B, later in time: sets T1 to `in_progress` and `low` (the **same task**), adds a note, removes the original note, edits T2's priority, creates "Order gasket".
4. The two state hashes differ.
5. **WiFi on.** One round later both replicas hold the same state, and the hashes match.

The result, and why (the script checks each line):

| Item | Result | Rule |
|---|---|---|
| T1 status and priority | B's later values, each field on its own | per-field last writer wins (Days 15-16) |
| T1 notes | A's new note and B's new note; the removed one is gone | a note is removed by name, so concurrent notes survive (Day 18) |
| T2 | still there, with B's priority | A deleted it, but B edited it without having seen the delete: add-wins |
| T3 | gone | deleted, and nobody else touched it |
| New tasks | both there, as `T4` and `T4.node-b` | IDs carry the replica name once a replica holds another's ops (Day 19) |

## Concepts

**Delete is a fact, not an erasure.** `DeleteTask` appends a `task.removed` op. Nothing leaves the log. The op carries the deleter's version summary: everything it had seen at that moment.

**Add-wins, decided from the set of ops.** A task is shown if some op that touched it (its creation, an edit, a note) was *not* seen by every delete. An edit the deleter had seen happened before the delete, so the delete wins. An edit it had not seen is concurrent, so the task stays. The rule looks only at which ops exist, never at the order they arrived in, so replicas that hold the same ops agree. (Comparing timestamps instead would be wrong: B's edit was *earlier* in time than A's delete and still must win.) If A, having now seen B's edit, deletes again, that second delete has seen everything and the task goes for good (tested).

**A note removal names one note.** `note.removed` carries the identity `(node, seq)` of the op that added the note. A note added at the same time on another replica is a different note and stays. The same text added again later is a new note. This is the OR-Set from Day 18, now written by the store, and a test checks the view against `crdt.TaskNotes` on random runs.

**The model cannot delete.** Deletion is a command-line operation. The agent's intents are unchanged; `delete_task` is refused as "not allowed", and a test checks it. A small local model deciding to delete data is a risk this project does not need.

**The view is still disposable.** The projection grew two tables and a column, so its version went from 1 to 2. Opening an older database rebuilds the view from the log (tested), and `Rebuild` after deletes gives the same hash.

## What was checked

- `go test -race ./...` passes; `gofmt` and `go vet` are clean.
- Store: a delete that saw the edit stays deleted; a delete that missed the edit loses to it; two deletes of one task; removing a note beside a concurrent add; removing the same note on both sides; malformed removals refused; and 25 seeded runs of three replicas with skewed clocks, random creates, edits, notes, note removals and deletes, with random partial pulls. At the end every replica must have the same hash, `Verify` must pass, a rebuild must change nothing, the set of shown tasks must equal an **independent oracle** written from the rules, and each task's notes must equal what the OR-Set computes. The run is required to contain at least 30 deletes and 3 tasks that survived a delete, so it cannot pass by doing nothing.
- Network chaos, in-process: the whole scenario through the real `netfault` switch (a failed round changes nothing); 15 seeds of a network that loses 20% of requests **and** 20% of responses (the server did the work but the client believes it did not, and sends the same ops again) with `Verify` after every round; a cable cut after each of 1..12 requests of a round. After the network heals, the replicas are in step every time.
- Mutation testing on the delete code: 13 mutants killed (the "seen" comparison, the alive/deleted decision, the delete check on every read path, note offsets and targets, the OR-Set removal). One survivor is equivalent (dropping a `break` in a loop that only sets a flag).
- Real processes: `TestCheckpoint2DemoScript` runs the script from `go test`, and I ran `converge-clip.sh` in a detached tmux session to check that the typed commands produce the right screens.

## Honest limits

- **The WiFi is simulated.** The partition is offgrid's fault-injection transport on localhost, not a real WiFi drop, and no phones are involved. The demo and the clip say so on screen ("FAULT INJECTION ON (simulated ...)").
- A delete can be **undone by a concurrent edit**. That is the intended rule, but it surprises people who expect "delete means gone". It is the same add-wins rule the Day 18 demo showed.
- `T1`-style IDs can still collide if two replicas each created tasks **before ever syncing** (Day 19 limit; the first sync is refused). Start the second replica empty.
- **Upgrade both sides together.** A Day 19 replica refuses a batch that contains Day 20 delete ops (unknown kind).
- Deleted tasks and tombstones are never cleaned up; the log only grows.
- Deleting is CLI-only; there is no delete in the agent loop, by design.
- Still true from Day 19: no encryption, a 5-minute clock window for the HMAC, a 1-hour future-timestamp limit, localhost only.

## Try it on your laptop

```bash
make converge-demo                          # the whole scenario, narrated, with checks (PAUSE=0 to skip the pauses)
go test -race ./internal/syncer ./internal/store/sqlstore ./cmd/offgrid

# by hand, in two terminals (A and B), after: make build
export D=/tmp/og; mkdir -p $D; openssl rand -hex 32 > $D/key; echo ok > $D/fault
# terminal A
OFFGRID_NODE=node-a bin/offgrid exec -db $D/a.db '{"intent":"create_task","title":"Leaking pump","priority":"high"}'
OFFGRID_NODE=node-a bin/offgrid sync serve -db $D/a.db -addr 127.0.0.1:7878 -key-file $D/key
# terminal B
OFFGRID_NODE=node-b bin/offgrid sync run -db $D/b.db -peer http://127.0.0.1:7878 -key-file $D/key -fault-file $D/fault
bin/offgrid fault set -file $D/fault partition    # WiFi off, from any terminal
bin/offgrid fault set -file $D/fault ok           # WiFi on
bin/offgrid task -db $D/b.db list                 # ends with a state hash; compare A and B
```

## Recording flagship clip #2

```bash
make build
# start your screen recorder, then:
scripts/converge-clip.sh          # A on the left, B on the right, B's sync loop underneath
```

The script types the commands for you (`TYPE_DELAY=0.03` is faster, `START_DELAY=6` gives you longer to start recording). What the viewer sees, in about two minutes: B syncs; `WiFi OFF` and the sync loop turns to `OFFLINE`; edits on both sides, with the same task T1 on both; the two `task list` outputs differ; `WiFi ON`, the loop prints `SYNCED`; the two lists are the same, and the two `state hash:` lines are the same. Suggested captions to add in your editor: "WiFi off", "both sides edit the same task", "different state", "WiFi on", "same state, same hash". Say in the post that the network cut is simulated.

If you cannot use tmux: open two terminals side by side and use the "by hand" commands above.

## Post draft: flagship clip #2

> Two replicas. WiFi off. Both edit the same task. Both delete things. I reconnect.
>
> Same state on both, checked by hash.
>
> The rules that make it work, each one a day of the build: every field is last-writer-wins on a hybrid clock; a note is removed by name so a concurrent note survives; a delete only beats edits its author had seen, so an edit made without knowing about it brings the task back; and both sides fold the same edits in the same order.
>
> It is real processes on one laptop with the network cut by a fault switch, not real phones on real WiFi yet. 25 random runs of three replicas with deletes, a network that eats 20% of requests and 20% of replies, a cable cut at every point of a sync round: always the same hash at the end.
>
> Clip below. Code: github link.

Hooks: this is the clip for founders and CTOs of field-work and offline-app companies. Lead with the clip, one number (the matching hashes), one lesson (a delete beats only what its author had seen).

## Checkpoint 2 verdict

| Criterion (roadmap) | Status |
|---|---|
| Both replicas go offline using the chaos switch | yes, `offgrid fault set partition`; simulated |
| Edit the same task | yes (T1, different fields, and T2 edit vs delete) |
| Add and delete list items | yes (tasks and notes) |
| Reconnect and compare state hashes | yes, equal in the script and in the Go tests |
| Risk rule: if the CRDT block overruns, cut Tier 2 and the dashboard before extending | not needed: Day 20 landed on schedule |

## Self-check

1. Why must a delete carry what its author had seen, instead of a timestamp?
2. B's edit to T2 was earlier in time than A's delete. Why does T2 survive?
3. What does it mean that the state hash is the same on both replicas, and what would it not tell you?
4. Why does a lost *response* matter more than a lost request?
5. Why can the language model not delete tasks?

## Tomorrow: Day 21 (light), recap and the long form

Re-cut the flagship video, update the README, and draft the long article "Why I used CRDTs for an AI agent". No new features.
