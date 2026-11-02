# Day 27: a log that shows when it was edited

**Done when:** editing a past op makes `verify` fail. It does, and it names the op.

```bash
offgrid store -db FILE verify                 # recomputes every hash; names the first op that no longer matches
offgrid store -db FILE head > heads.txt       # the newest hash of every replica: keep this somewhere else
offgrid store -db FILE verify -anchor heads.txt
make tamper-demo                              # edit the log three ways and watch (needs python3)
```

```
TAMPERED: op laptop/1: was changed after it was recorded (its content no longer matches its hash)
The log does not match the hashes recorded when it was written. Nothing has been changed or repaired:
the log is evidence. Compare with another replica (offgrid sync summary) or a saved copy before you do anything.
```

## How it works

Each op carries a hash of its own content (replica, sequence number, timestamp, counter, kind, payload) **and the hash of the previous op from the same replica**. Change a word in op 7 and its hash changes, so op 8's hash (which contains op 7's) is wrong, and op 9's, all the way to the end.

It is a chain **per replica**, not one chain for the whole log, on purpose. Ops reach each replica in a different order, so "the op before this one in my log" is different on every device. "The op before this one from the same replica" is a fact about the op itself, so every replica computes the same hash for the same op and sync can compare them. The simulator checks this: after every kind of failure it throws at three and five replicas (1,600 schedules), the chains end in identical heads everywhere.

The hashes live in their own insert-only table (`op_chain`), protected by the same kind of append-only triggers as the log, so the log's table did not change shape. Fields are length-prefixed before hashing, so text cannot be shifted between fields, and JSON whitespace is ignored, so a peer that re-indents a payload does not break anything.

## What it catches

| Someone... | Result |
|---|---|
| changes a word, a timestamp, a counter or a kind in an old op | `verify` names that op (the earliest one, if several) |
| deletes an op from the middle | "is missing from the log" |
| deletes the newest op but leaves its hash | "has a hash but no op" |
| removes or edits a hash | "has no entry in the hash chain" / "is not linked" / "was changed" |
| rewrites an op **and** recomputes every later hash **and** rebuilds the view | **passes `verify` on its own.** Caught by either of the next two rows |
| ...and you saved the heads earlier | `verify -anchor` fails at the first saved head after the edit |
| ...and that replica syncs with one that kept the real history | the first new op it makes is refused: "arrived with hash X but this replica computes Y from its own copy of the earlier ops" (HTTP 409, code `chain`) |
| cuts the log's tail cleanly (ops and hashes) | passes `verify`; a saved head fails: "no longer has it: ops were removed" |

Before today, sync only ever exchanged the ops a peer lacked, so nobody compared old ops: a quietly edited old op stayed that way forever. Now the next op built on it exposes it.

## Threat model

**What I am defending against:** someone (or something, such as a buggy script or a disk fault) that changes the database file *after* ops were recorded, without knowing about the chain: an accidental edit, a careless insider, a sync bug, a corrupted page. And a replica whose history has quietly diverged from the others.

**What I am not defending against:**

- **Someone who understands the chain and has the file.** They can rewrite an op, recompute every later hash, rebuild the view, and the file verifies. The defence is *outside the file*: heads saved elsewhere (a note, an email to yourself, a post, another machine) and the other replicas. A chain with no outside reference proves consistency, not honesty.
- **Authentication.** A hash chain says an op is unchanged; it does not say who made it. An op is as trusted as the replica that sent it, and sync is still HMAC with a shared key: anyone with the key can send new ops in a valid chain. (Per-replica signing keys would be the next step; not built.)
- **A malicious replica from the start.** It can build a perfectly valid chain of false ops. It cannot change ops other replicas already hold.
- **Old ops in a database that predates today.** They were chained when the program first opened it, from their content *at that moment*. An edit made before that cannot be seen. `verify` says how many ops are in this state ("3 of them were chained when this database was upgraded").
- **Deleting the whole database**, or the chain table and the log together and starting over. That is loss, not tampering with a record.
- **Secrecy.** Nothing is encrypted.
- **Time.** Timestamps are the writer's clock (hybrid logical clock); the chain does not prove when something happened.

**Choices worth knowing about:**

- If the chain table loses rows later, opening the database does **not** quietly rebuild it from the ops (that would hide the deletion). `verify` reports the first op without a chain entry, and new writes are refused until a person looks. A tampered log is evidence; the program does not repair it.
- `store head` refuses to print heads of a chain that does not verify, so a saved head is always of a good log.
- An op that arrives with no hash (an older peer, or an op built by hand) is accepted and chained locally, without the cross-check. Upgrade both replicas together, as before.

## What it costs (this sandbox: 2 cores, ext4, one run, noise about ±10%)

| | Day 26 | Day 27 |
|---|---|---|
| 1,000 durable appends per second | 860 | 873 |
| a new replica catches up on 10,000 ops (in process) | 2.14 s | 2.31 s |
| ...over HTTP on localhost | 2.19 s | 2.61 s |
| database file for 10,000 ops | 2.3 MiB | 3.9 MiB |
| `store verify`'s chain pass over 10,000 ops | - | about 0.1 s (a Go benchmark) |

Appends are unchanged because the chain entry is written in the same transaction, so there is still one fsync per op. Catching up costs a little more (a hash per op, and one more row). The file grows by about 70%: two 64-character hashes per op. The README's benchmark tables are still the Day 25 numbers; `make bench` on your laptop refreshes them.

## How I tested it

- Every field of the hash is covered by its own test, and a test checks that text cannot be moved between fields.
- 11 ways of editing the log (a word, a timestamp, a counter, a kind, a deleted op, a missing hash, a bent link, a changed hash, the last op...), each must name the right op; two edits report the earlier one; 60 random single-character edits are each found at the right op.
- The limit is a test: a fully recomputed chain passes `VerifyChain` and fails against a saved head.
- Sync: a wrong hash is refused with nothing kept; a duplicate with a different hash is refused; a replica whose old op was rewritten refuses the next real op ("the scenario the chain exists for"); an op with no hash is still accepted; a re-indented payload with the real hash is accepted.
- An old database is upgraded and says so; a chain that goes missing later is never rebuilt.
- A real process: `make tamper-demo` edits the file with python's sqlite3 (the person with a file editor) in a careless and a careful way and fails if either goes unnoticed.
- The simulator now also checks that every replica ends in identical chain heads and that every chain verifies: 1,000 default schedules, 300 with a shared first task, and 300 with five replicas and 120 actions, all pass.
- 38 bugs put into the chain, sync and command code (drop a field from the hash, skip the link check, never compare duplicates, re-chain a missing chain, ...). Two got past my first tests and are closed: the length-prefix test could not tell a prefix from none, and the error-code test compared a constant with itself. Not mutated: the new simulator check itself.

## What this does not show

- It has not run on your laptop's data. Your existing database will be upgraded the first time any command opens it; make a copy first if that worries you (`cp tasks.db tasks.db.bak`).
- Anchors are only as good as where you keep them. I did not build anything that saves them for you, on purpose: a program that saves its own anchors in its own folder protects nothing.
- Per-replica signatures, a public transparency log and key rotation are all out of scope.

## The post (Day 27)

Attach a screenshot of the failing `verify`, or record `make tamper-demo`.

> I edited my own database by hand and my system caught it.
>
> Every operation in the offline log now carries a hash of itself and the one before it, per device. Change one word in an old op and `offgrid store verify` says exactly which op changed: "op laptop/1 was changed after it was recorded."
>
> The honest part: if I'm careful (rewrite the op, recompute every later hash, rebuild the view), the file passes its own check. So I also made two things catch that: a head I saved earlier, and the next sync with a device that kept the real history, which refuses the new op because it doesn't match the past it remembers.
>
> It proves consistency, not honesty. A chain with no outside copy can be rewritten. Saving the head somewhere else is the whole defence.

Hook for compliance-minded founders: *it tells you when the record changed, and it is honest about what it can't tell you.*

## Checklist

- [ ] `go get pgregory.net/rapid@v1.2.0`, commit `go.mod` and `go.sum`.
- [ ] **Back up your real database first** (`cp ~/offgrid/tasks.db ~/offgrid/tasks.db.bak`), then open it with any command: it is upgraded and chained. `offgrid store -db ~/offgrid/tasks.db verify`.
- [ ] `make tamper-demo` (about 10 seconds; needs python3). Record it for the post.
- [ ] Save a head somewhere that is not that folder: `offgrid store -db ~/offgrid/tasks.db head`.
- [ ] Sync the laptop with a second device, then `verify` on both: same heads (`store head` prints the same lines).
- [ ] Still open from before: `make bench` on the EliteBook; the Day 26 clip; the 7-8B row; `ollama --version`.
- [ ] 15 minutes of outreach.
