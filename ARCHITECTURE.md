# Offgrid architecture (v1, at tag v0.1)

Offgrid is an AI agent runtime for places where the network comes and goes. A small local model makes the instant decisions, a durable queue holds the hard thinking for later, and (not built yet) CRDT sync will merge offline and online work. This document says what exists **today**, how the parts fit, which guarantees hold and what tests enforce them, and what is deliberately not done. Where something is planned rather than built, it says so.

## 1. The system in one picture

```
                         worker message ("log a leaking pump at site 4, high priority")
                                           |
                                           v
                  +------------------- router (internal/router) -------------------+
                  |  complexity gate (closed vocabulary, threshold 3)              |
                  |  connectivity state (affects only the optional cloud model)    |
                  +-----+----------------------+-----------------------+-----------+
                        | easy                 | "I could not read it" | hard (judgement,
                        v                      v                       v  analysis, a time span)
                 agent (internal/agent)    saved as job "interpret"   saved as job "deep_think"
                 Tier 0: rules, no model          |                       |
                 Tier 1: small local model        +-----------+-----------+
                        |                                     v
                        v                          queue.db (internal/jobqueue)
                 tools (internal/tools)            jobs + results, leases, backoff
                 5 typed tools, validated                      |
                        |                                      v
                        v                          drain worker (internal/drain)
                 tasks.db (internal/store/sqlstore)   network gate, N at a time, heartbeat,
                 ops (append-only log) is the truth;  fenced reports, handlers:
                 tasks / task_notes are a cache       interpret (changes tasks) / deep_think (reads)

   connectivity monitor (internal/connectivity): ONLINE / DEGRADED / OFFLINE with hysteresis
   netfault (internal/netfault): breaks the network on purpose, reproducibly, for tests and demos
   obs + dash (internal/obs, internal/dash): structured logs, counters, and the one-screen `offgrid top`
```

Also built, but a library rather than a box: the hybrid logical clock (`internal/hlc`, Day 15). Every op in the log carries an HLC stamp: `ts` is its wall part in nanoseconds and `counter` its tie-break part. Also built: the LWW-Map (`internal/crdt`, Day 16), a pure `Merge` over per-field registers, which `Store.Fields` fills from the op log. Property tests (Day 17, `pgregory.net/rapid`) check that merge is commutative, associative and idempotent, and that the same tests catch deliberately broken merges. Not built yet: the OR-Set (notes) and sync between two replicas.

## 2. Process model

There is one binary, `offgrid`, and **no daemon**. What looks like a system is separate processes over two SQLite files:

| Process | Role | Reads / writes |
|---|---|---|
| `offgrid agent -route` | decides per message: answer now, or defer | `tasks.db` (answers), `queue.db` (enqueue), `metrics/agent.json` |
| `offgrid drain` | empties the queue when it can | `queue.db`, `tasks.db`, `metrics/drain.json` |
| `offgrid top` | shows the whole system | `metrics/*.json`, `queue.db` (**read-only**) |
| `offgrid queue`, `store`, `net`, `fault` | inspect and operate | the files above |

They share **no memory**. The files are the interface, which is what makes `kill -9` on any of them a normal event instead of a special case.

## 3. Components

Dependencies point downward; nothing lower knows about anything above it.

| Layer | Package | Responsibility | Knows nothing about |
|---|---|---|---|
| Edge | `cmd/offgrid` | flags, wiring, printing; the only place that assembles the parts | |
| Orchestration | `internal/router` | answer-now-or-defer decision, with a reason a person can read | how jobs run |
| | `internal/drain` | the worker: claim, gate, run, report; job handlers | how decisions are made |
| Understanding | `internal/agent` | Tier 0 rules, then Tier 1 model; outcome with tier and timings | the queue, the network |
| | `internal/intent`, `internal/structured` | the `Command` type; valid JSON out of a model, with retries | which model |
| | `internal/llm` | the interface models sit behind (real Ollama client, fake for tests) | |
| Acting | `internal/tools` | a **closed menu** of five typed tools (`create_task`, `update_task`, `list_tasks`, `add_note`, `daily_summary`) | models |
| State | `internal/task` | the `Task` type and the `Store` contract, plus shared contract tests | SQLite |
| | `internal/store/sqlstore` | the SQLite store: append-only ops, migrations, rebuild, verify | |
| | `internal/jobqueue` | durable queue on any `*sql.DB`: idempotency, backoff, leases, fencing, results | the SQLite driver, the task store (a test enforces this) |
| Environment | `internal/connectivity` | probes, hysteresis state machine, monitor goroutine | what the state is used for |
| | `internal/netfault` | an `http.RoundTripper` that injects latency, loss and partitions | |
| Visibility | `internal/obs`, `internal/dash` | logs, counters, snapshot files; a pure render function | |
| Measurement | `internal/bench`, `internal/eval`, `internal/sysinfo` | benchmarks and model reliability, always naming the hardware | |

## 4. How a message travels

**Answered now.** The router scores the text against a closed vocabulary (judgement words, a time span, a second request). Below the threshold the local agent handles it: Tier 0 rules read the common phrasings in microseconds; only what the rules cannot read goes to the small local model. The model's output is a *proposal*: it is parsed, validated against the typed command, and only then executed by a tool.

**Deferred.** Hard requests, and messages the local model could not turn into a valid command, become jobs in `queue.db` with a deterministic idempotency key (the same sentence twice on the same day is one job). The person gets an immediate, readable status ("saved, 3 ahead of it, waiting for the network"). Connectivity never changes *whether* something is deferred; it only changes the wording and whether an optional cloud model may answer.

**Drained.** The worker asks the connectivity monitor whether the network is up, then claims only the job kinds it can run right now (a claim is an attempt, so it filters first rather than claim-and-put-back). `interpret` needs only this laptop, so it runs offline; `deep_think` runs offline too unless `-deep-host` points at another machine, in which case it waits for the network.

## 5. Data

**`tasks.db`: the op log is the truth.** `ops` is append-only (SQL triggers reject UPDATE and DELETE) with a per-replica gap-free `seq`, so an op's identity is `(node_id, seq)`. `tasks` and `task_notes` are a **disposable cache**: `offgrid store rebuild` throws them away and replays the log; `verify` compares the two. WAL and `synchronous=FULL`: an op is on disk before it is acknowledged.

**`queue.db`: jobs and results.** `jobs` holds status (`queued`, `running`, `done`, `dead`), attempts, lease expiry, progress (a checkpoint), last error; `results` holds an answer, written in the **same transaction** that marks the job done. Schema versions live in SQLite's `PRAGMA user_version`; a file from a newer program is refused, not modified.

**`metrics/*.json`: snapshots.** One small file per publishing process, rewritten once a second by rename. They are a view, not a source of truth, and may be stale or absent.

## 6. Guarantees, and what enforces them

| Guarantee | How | Test |
|---|---|---|
| Nothing a model says is trusted until validated | typed `Command`, closed tool menu, validation before execution | `internal/intent`, `internal/tools`, `internal/eval` |
| The op log cannot be rewritten | triggers on `ops` | `internal/store/sqlstore` migration and store tests |
| Rebuilding the cache from the log gives the same state | replay is the only way state is derived | `rebuild` and `verify` tests |
| A repeated request creates one job and one task | idempotency key; `Create` is atomic and returns the existing task | `internal/jobqueue`, `internal/task/storetest` |
| **Delivery is at-least-once; the recorded outcome is exactly-once** | the attempt number is a fencing token on Complete, Fail, Extend, Checkpoint and Release (`ErrStaleClaim`); result and "done" in one transaction | `internal/jobqueue` fencing tests; the kill test |
| A killed worker loses nothing and duplicates nothing | leases expire and another worker claims the job (try 2) | `TestKill9MidDrainLosesNothingAndDuplicatesNothing` (3 SIGKILLs, 60 jobs) |
| Going offline never burns a job's attempts | filter by kind inside the claim; a stop or network loss is `Release`, not failure | worker tests (a job with 2 attempts restarted 6 times is still intact) |
| A job that changes data is not repeated after a crash | `interpret` saves an `executing` marker; a rerun that finds it goes to a human | handler tests |
| The state machine does not flap | hysteresis, asymmetric thresholds (down faster than up) | `internal/connectivity` |
| The viewer cannot change the queue | read-only connection, no migrations | `TestReadOnlyViewSeesTheQueueAndCannotChangeIt` |
| The screen never presents old data as live | snapshot age against the publisher's own interval | `internal/dash`, `internal/obs` |
| Op timestamps never go backwards and always follow what a node has seen, even with a wrong clock | hybrid logical clock; restored from the log on open | `internal/hlc` tests (including a 25-seed skewed-clock simulation); `sqlstore/hlc_test.go` |
| Merging task fields gives the same result in any order, grouping or repetition | `crdt.Merge` keeps the larger of a total order (stamp, node, deleted, value) | `internal/crdt/props_test.go` (13 properties x 3000 generated cases); `broken_test.go` shows broken merges are caught |

**Not promised:** exactly-once *execution* (impossible after a crash: the worker cannot know whether it died before or after the effect), protection against power loss beyond what SQLite's WAL and `synchronous=FULL` give (not tested by pulling a plug), or any cross-replica behaviour (no sync yet).

## 7. Decisions worth defending

- **Precision over coverage.** When the agent is unsure it asks or refuses; a wrong task created silently is worse than a question.
- **Rules before a model.** Most field messages are formulaic. Tier 0 answers in microseconds and cannot hallucinate; the model is only for what the rules cannot read.
- **A queue in SQLite, not a broker.** One file, no daemon, survives power loss, inspectable with `offgrid queue`. The cost: one writer at a time, which is fine at this scale and would not be at another.
- **Files as the interface between processes.** No sockets or RPC means nothing to keep alive and a crash never leaves a connection in a bad state.
- **Boundaries enforced by tests, not by convention.** The queue may not import the SQLite driver or the task store, and a test fails the build if it does.
- **Injected clocks and a fault-injecting transport.** Time and the network are the two things that make distributed code untestable; both are replaceable, so outages and crashes are reproducible test cases.
- **Plain ANSI instead of a UI framework** for `offgrid top`: a pure `Render(view)` and a few lines that repaint. A framework can wrap it later without changes elsewhere.
- **Honest numbers only.** Every benchmark names the hardware; demos label what is simulated.

## 8. Known limits (v0.1)

- Real Ollama and real network loss are **untested here**: the test and demo model is a stand-in server with canned answers, and the network is simulated.
- No sync yet. `crdt.Merge` can merge two field maps, but nothing yet moves ops between replicas, and task IDs (`T1`, `T2`...) are per-replica counters that would collide. The HLC exists and is tested (including a multi-node simulation with skewed clocks), but only a simulation has exercised it across nodes, not real devices.
- A clock once far ahead drags the HLC with it (`offgrid store info` warns). A remote stamp more than 1 h ahead is refused by `Clock.Update`; a last-writer-wins merge will then prefer that remote value.
- Counters are per process; the tiers panel in `top` shows only the latest `agent -route` run.
- One writer per SQLite file. Two `drain` workers on one queue are safe (fencing) but publish under the same snapshot name.
- The complexity gate is a closed vocabulary: it is explainable and measured on held-out cases, but it will miss phrasings it has not seen.
- Tier 2 as drawn in the roadmap (a bigger local or cloud model) exists as the `deep_think` job and an unused `Cloud` hook in the router; no cloud client is written.

## 9. Where to start reading

1. `internal/router/router.go`: the decision, in one function.
2. `internal/jobqueue/queue.go`: claim, fence, complete, release.
3. `internal/drain/worker.go`: the loop that uses them.
4. `internal/drain/kill_test.go`: the test that makes the claims above believable.
5. `docs/day-NN.md`: the concept behind each day's code.







