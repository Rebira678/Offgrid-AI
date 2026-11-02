# Why I used CRDTs for an AI agent

*Draft 1, Day 21. Written from what the code does and what the tests show as of Day 20. Numbers in it were measured on one HP EliteBook (Ryzen 5 PRO 5650U, 16 GB, no GPU), and the network cuts were simulated on localhost. Where something is not built or not tested, the text says so.*

## The setup

Offgrid is an agent runtime for places where the network comes and goes: a field worker types "pump at site 4 is leaking, high priority" into a phone with one bar of signal, or none. A small language model running on the device turns that into a command. The command is checked by ordinary code. The result is a task in a local database. On my laptop (Ryzen 5 PRO 5650U, CPU-only), a 1B model generates at ~16 tokens/second and a 3B model at ~10 tokens/second—plenty fast for short field commands without a server. When the connection returns, whatever needs a bigger model, or another device, catches up.

Most of the interesting engineering was not the model. It was the question that arrives the moment a second device exists: **two devices edited the same data while they could not talk to each other. What now?**

This article is about why I answered that with CRDT-style data structures, what that looked like for an agent, and the four places where the first design was wrong.

## Why a queue is not enough

In the second week I built a durable job queue, a connectivity monitor and a router: offline, hard requests wait in a queue with backoff and retries, and drain when the network is back. The queue survives `kill -9` without losing or duplicating a job.

That solves *requests that have to reach somewhere else*. It does nothing for *two copies of the same data*. If Lidia closes a task on her phone and Beka, in a different valley, changes its priority on his, a queue just delivers both edits, in some order, and one of them silently wins or the program crashes. The options for that situation are short:

- **Ask a server.** Needs the connection that is missing.
- **Lock the record.** Needs the connection that is missing.
- **Last write wins by wall clock.** Works until a phone's clock is ten minutes slow (more below), and then loses a later edit without any error.
- **Make the data merge by construction.** Pick data structures where applying the same edits in any order gives the same result. That is what a CRDT (conflict-free replicated data type) is.

## The smallest thing that could work

CRDT research is mostly about collaborative text. A task list is not text. A task is a small record (title, site, priority, status) with notes attached, and the useful questions are "who set the status last?" and "did a note survive a delete?". So I used two small, well-understood pieces:

- **A last-writer-wins register per field.** Status and priority each keep the value with the largest timestamp, ties broken by node ID. Two people editing *different* fields of the same task both keep their edits.
- **An observed-remove set for notes.** Every note has a unique tag. Removing a note removes only the tags the remover had seen, so a note added at the same time elsewhere survives. "Add wins" over a concurrent remove.

Each is a pure function with no database and no clock inside it, so it can be tested on its own. I wrote them as that first, and the store second.

## Where the agent fits: it is just another writer

An agent that edits data is the part people worry about, and the design answer is the same as for a human: **everything that changes anything is an op, and ops are facts.** "Task T3 was created." "T1's status became done." They are appended to a log and never edited. The tables the app reads are a disposable view of the log: delete them, replay the log, get the identical state (the tests do exactly that, with a hash to prove it).

The language model sits in front of this. It proposes a command, and the code decides:

- The model can only pick from a closed menu of operations.
- Every field it fills in is validated again by code before anything is written.
- Some things are not on the menu at all. **Deleting a task is a command-line operation; the model cannot ask for it**, and a test checks that `delete_task` is refused. A small local model deciding to destroy data is a risk I did not want to carry.

Once the agent's writes are ops like any other, sync does not care whether an op came from a human typing or a model proposing. That is the whole reason this combination works.

## The part that is easy to get wrong, in the order it bit me

### 1. A wall clock is a claim, not a fact

If "last writer wins" compares wall clocks, a later edit made on a phone whose clock is slow loses to an earlier one, and nothing complains. So every op carries a hybrid logical clock stamp (wall time plus a counter, from the paper by Kulkarni et al.): when a replica receives an op, its own clock jumps to at least that stamp, so anything it does next sorts after what it has seen. In a simulation with fake clocks (offsets up to ±5 seconds and clock steps; 25 seeds, 66,677 messages) the hybrid clock ordered every message correctly, while ordering by raw physical time got 33,190 wrong. That is a simulation, not real phones.

It has a cost, and I wrote it down: a replica refuses a remote stamp more than an hour in the future, because one phone with a wrong clock must not drag everyone into next year. The refused batch waits until the clock is fixed or the limit is raised.

### 2. My view depended on arrival order

This is the one I would warn anyone about. I had built the readable view by replaying ops in the order *this replica received them*. That is fine for one device and wrong for two: replica A applies B's ops after its own, replica B applies A's after its own, and they can disagree. The fix is to make the view a **pure function of the set of ops**: fold them in a canonical order (timestamp, counter, node, sequence number) that is the same everywhere. If a new op sorts after everything already applied, apply it on top. If it sorts before something already applied (a concurrent edit that happened earlier in time), refold the view, in the same transaction.

Once the state is a function of the *set* of ops, the replication problem becomes almost boring: the merge is set union. Each replica numbers its own ops 1, 2, 3 with no gaps, so "what I hold" is one number per node, and "what you are missing" is a comparison of two small maps. Sync sends only that difference, applying a batch twice is the same as once, and a batch that skips an op or contradicts one already held is refused whole.

### 3. A delete is not an edit with a timestamp

The obvious way to delete: write a tombstone with a timestamp, and the later one wins. In my scenario that is wrong. A deletes task T2 at 10:02. B, offline and unaware, edits T2 at 10:01. By timestamp the delete is later and wins, B's edit is thrown away, and nobody is told.

What I wanted is the add-wins behaviour of the set: a delete should beat only what its author had *seen*. So a delete op carries a summary of everything its author held, and a task stays visible if any op that touched it was not seen by every delete. That depends only on which ops exist, not on timestamps or arrival order. In the demo, A deletes T2, B edits it, they reconnect: T2 is back with B's edit. If A, having now seen it, deletes again, it is gone for good. This surprises people who expect "delete means gone", so it is documented, and it is the rule I would defend: silently destroying someone's offline work is worse than a task that comes back.

### 4. Two replicas will both call their first task T1

Task IDs were per-replica counters. Two replicas that each create a task before ever meeting both have a T1. Once a replica holds another replica's ops it now names new tasks `T4.node-b`. For the original case (two databases that diverged before ever syncing) I did not invent a clever repair: the first sync is detected, refused with a clear error, and changes nothing. The fix is to start the second replica empty and let it pull.

## How I tried to break it

I trust a merge function about as far as the tests that tried to break it.

- **Property tests** state the laws (merging is commutative, associative, idempotent, never moves a field backwards) and run them on thousands of generated cases. The same laws, run against seven deliberately broken merges, must fail each one; two legitimate alternatives must pass. A law that cannot catch a bad merge proves nothing.
- **An independent oracle.** The test for deletes computes which tasks should exist from the rules, using none of the store's tables, and compares: 25 seeded runs of three replicas with skewed clocks doing random creates, edits, notes, note removals and deletes, with partial random pulls. They must all end with the same state hash, a rebuild must change nothing, and the run must contain enough deletes and resurrected tasks that it cannot pass by doing nothing.
- **A hostile network.** One test loses 20% of requests *and* 20% of responses. Losing a response is the cruel case: the server did the work, the client thinks it did not, and sends the same ops again. Another cuts the connection after each of 1 to 12 requests of a round. After every round, both replicas must verify against their own logs, and after the network heals, they must match.
- **Mutation testing.** I broke the code on purpose in dozens of places (a `<` for a `<=`, a missing filter) and checked that a test fails each time. The survivors were equivalent changes, and I wrote down why.

The numbers, as of today: 821 test functions in 22 packages, about 16,000 lines of Go and about 24,000 lines of tests. The tests are bigger than the product, and I think that is correct for this kind of code.

The final check of the project so far is the two-replica scenario: both replicas lose the network, edit the **same** task (different fields), add and remove notes, delete tasks, create tasks; then reconnect. After one round both have the same state hash. The network cut is simulated by a fault-injection transport on localhost, and the clip says so.

## What this does not do

Honesty is the point of a post like this, so:

- **Real devices and real WiFi are untested.** Everything ran on one laptop, over localhost, with a simulated partition.
- **Requests are signed, not encrypted.** Sync uses an HMAC with a shared key, which proves who is talking and that nothing was altered, and nothing more. Use a trusted network or TLS in front. The signature also has a five-minute window, so a phone with a badly wrong clock cannot sync until it is fixed.
- **The log only grows.** Tombstones and old ops are never compacted.
- **Two people creating the same task title offline get two tasks**, not one.
- **It is a task list, not a document.** I chose the CRDT pieces for this data shape. Collaborative text would need a different, heavier structure.
- **The local model's quality is a separate story.** I have not shown anything here about how well a small model understands field workers, and I am not claiming to.

## What I would tell someone starting

1. **Make state a function of a set, then replicate the set.** Everything after that gets easier, and most of what goes wrong before that is order.
2. **Keep merge functions pure and test them before any database exists.** A merge that needs a database to test is a merge you will not test enough.
3. **Write the test that must fail.** Break the merge on purpose and check the law notices.
4. **Decide what a delete means before the first delete.** "Gone" and "gone unless someone edited it" are different products.
5. **Say what it does not do.** Readers trust the guarantees you list more when the limits are right next to them.

The code, the tests and a one-command demo of the offline scenario are in the repository: `make converge-demo`. Next I will make the model side as measured as this side is.

---

*Editing notes (delete before publishing):*
- *Add your own Day 1 number (tokens per second on your machine) in "The setup" if you want one; I did not have the table, so the draft has none.*
- *The 66,677 / 33,190 figures come from the Day 15 simulation (`internal/hlc`); rerun `go test -v -run TestManyNodesWithBadClocks ./internal/hlc` to see them printed.*
- *Test counts will drift as you add tests: recount with `go test -v ./... | grep -c '^--- PASS'`.*
- *Link the repository and the Day 20 clip near the top once they are public.*
