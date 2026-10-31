# Offgrid architecture (v1, at tag v0.1)

Offgrid is an AI agent runtime for places where the network comes and goes. A small local model makes the instant decisions, a durable queue holds the hard thinking for later, and CRDT-style sync merges offline and online work (two replicas exchange ops over HTTP since Day 19, tested on localhost only). This document says what exists **today**, how the parts fit, which guarantees hold and what tests enforce them, and what is deliberately not done. Where something is planned rather than built, it says so.

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

Also built, but a library rather than a box: the hybrid logical clock (`internal/hlc`, Day 15). Every op in the log carries an HLC stamp: `ts` is its wall part in nanoseconds and `counter` its tie-break part. Also built: the LWW-Map (`internal/crdt`, Day 16), a pure `Merge` over per-field registers, which `Store.Fields` fills from the op log. Property tests (Day 17, `pgregory.net/rapid`) check that merge is commutative, associative and idempotent, and that the same tests catch deliberately broken merges. Also built: the OR-Set (`crdt.Set`, Day 18), an add-wins observed-remove set with tombstones, used for notes. Also built (Day 19): sync between two replicas, `internal/syncer` plus `Store.Summary`, `OpsSince` and `ApplyRemote`. The view is the op log folded in HLC order `(ts, counter, node, seq)`, so replicas holding the same ops hold the same state; an op that sorts before something already applied makes the store rebuild the view in the same transaction. Task IDs become `T<n>.<node>` once a replica holds another replica's ops. Requests and responses are HMAC-SHA256 signed with a shared key (authentication and integrity, not encryption). Also built (Day 20, Checkpoint 2): deletion. `task.removed` records the deleter's version summary, so an edit it had not seen brings the task back (add-wins); `note.removed` names one note by the identity of the op that added it. The view keeps `task_marks` and `task_removals` so whether a task is deleted depends on the set of ops, not their order (projection version 2; old databases rebuild their view on open). Deleting is available from the command line (`offgrid task delete`, `remove-note`) and is deliberately not an intent the language model can use. Not built yet: tombstone cleanup, TLS, discovery of peers, and any test over a real network. A replica from before Day 20 refuses a batch containing delete ops (unknown kind), so upgrade both replicas together.

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
| | `internal/llm` | the interface models sit behind (Ollama, an optional OpenAI-compatible cloud client, a fake for tests) | |
| Acting | `internal/tools` | a **closed menu** of five typed tools (`create_task`, `update_task`, `list_tasks`, `add_note`, `daily_summary`) | models |
| State | `internal/task` | the `Task` type and the `Store` contract, plus shared contract tests | SQLite |
| | `internal/store/sqlstore` | the SQLite store: append-only ops, migrations, rebuild, verify | |
| | `internal/jobqueue` | durable queue on any `*sql.DB`: idempotency, backoff, leases, fencing, results | the SQLite driver, the task store (a test enforces this) |
| | `internal/suggest` | wraps a task store for AI code: creating passes through, but a change to an existing task becomes a recorded suggestion (`*Suggested`) instead of a write | the model, the queue |
| | `internal/sim` | deterministic simulation: one seed becomes a schedule of edits, partitions, clock skew, crashes and lossy sync, run against the real store; checks convergence, causality, atomic refusal and two rules recomputed from the log; shrinks a failing schedule | the model, the network, real time |
| | `internal/perf` | reproducible measurements: store append/open/rebuild, replica catch-up (in process and over signed HTTP), queue drain, peak heap and RSS, with the hardware recorded; writes the README tables and the chart between markers | the model (tokens/sec is `offgrid bench`), real networks |
| | `internal/web` | the dashboard: an embedded page (`go:embed`), `/api/state` assembled from the task log, the queue and the published snapshots (the same merge the terminal screen uses), and one write: accept or dismiss a suggestion through the store's own `Resolve` | any other write, any non-local client, any outside network |
| | `internal/idle` | is the CPU quiet? gates slow (Tier 2) jobs so they never slow the fast tier | what the jobs are |
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
| A slow-thinking answer is recorded once, is identical on every device, and a crash after saving never re-runs the model | `answer.recorded` op with the job's idempotency key; first answer in the canonical fold order wins; the handler looks the key up before asking a model | `sqlstore/answers_test.go`, `drain/think_test.go` |
| **An AI never overwrites a person:** no model-side code can write an existing task; its updates and notes are suggestions stored beside the task, applied only by a person's explicit accept, which is refused if the task changed since | `internal/suggest` wraps the store handed to model code; `suggestion.made` never touches the task row; `Resolve` refuses stale accepts | `sqlstore/suggestions_test.go` (incl. `TestAHumanEditMadeOfflineSurvivesAnAIJobFinishingLater`, a 12-seed random test), `suggest/suggest_test.go`, `drain/interpret_safe_test.go` |
| **Replicas converge under random failure:** 1,000+ seeded schedules of partitions, clock skew, crashes, and duplicated, dropped, delayed, cut, reordered or holed sync replies end with the same ops and state everywhere; every new edit sorts after what its replica had seen; a refused batch changes nothing | `internal/sim` runs real SQLite replicas; a seed is a pure function, so any failure replays exactly (`offgrid sim -seed N`) and shrinks to a few actions | `sim/sim_test.go`, `make sim`, deliberate-bug runs in `docs/day-24.md` |
| **Benchmarks are reproducible:** one command measures the system costs with a fixed seed, exact workload sizes, median of repeats with min and max, and a check that every replica ended identical before any time is reported | `internal/perf`; `make bench` rewrites only the marked README blocks | `perf/perf_test.go` (workload exactness, same-state refusal, report and chart), `docs/day-25.md` |
| **The dashboard cannot be driven from outside this machine or from another site:** it listens on loopback only; a request whose Host is not local, a POST without its header, or one from another origin is refused; the page loads only its own files and never turns data into HTML; accepting a stale suggestion is refused exactly as on the command line | `internal/web` `guard`, `RequireLoopback`, a CSP of `default-src 'none'` plus own script and style | `web/web_test.go`, `cmd/offgrid/web_test.go` (real process, real socket), `docs/day-26.md` |
| A slow job never starts beside fast work, nor two at once, nor on a busy CPU, and waiting costs no attempts | `Spec.Background`: the claim excludes the kind while the gate is closed | `drain/idle_test.go` |
| The viewer cannot change the queue | read-only connection, no migrations | `TestReadOnlyViewSeesTheQueueAndCannotChangeIt` |
| The screen never presents old data as live | snapshot age against the publisher's own interval | `internal/dash`, `internal/obs` |
| Op timestamps never go backwards and always follow what a node has seen, even with a wrong clock | hybrid logical clock; restored from the log on open | `internal/hlc` tests (including a 25-seed skewed-clock simulation); `sqlstore/hlc_test.go` |
| Deleting something never destroys an add the deleter had not seen (add-wins); removed stays removed | `crdt.MergeSet`: union of adds and of tombstones; a remove tombstones only the tags it observed | `internal/crdt/orset_props_test.go` (oracle and laws, 3000 cases each); `orset_broken_test.go` |
| Merging task fields gives the same result in any order, grouping or repetition | `crdt.Merge` keeps the larger of a total order (stamp, node, deleted, value) | `internal/crdt/props_test.go` (13 properties x 3000 generated cases); `broken_test.go` shows broken merges are caught |

**Not promised:** exactly-once *execution* (impossible after a crash: the worker cannot know whether it died before or after the effect), protection against power loss beyond what SQLite's WAL and `synchronous=FULL` give (not tested by pulling a plug), or behaviour across a real network (sync is tested over localhost and in-process only).

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
- Sync is young. Ops move between replicas (Day 19) but only over localhost and in-process tests, never a real WiFi link or real phones. Task IDs carry the replica that made them (`T3.laptop`, since Day 24; the simulator found that plain `T1` on two unsynced replicas wedged sync). Plain IDs in older databases remain valid, and two older databases that collided are refused with an ID-collision error. A far-future timestamp (over `-max-drift`, 1h) makes a batch be refused until the clock is fixed. The HMAC timestamp window (5 min) means badly skewed clocks cannot sync. Nothing is encrypted. Tombstones and the log are never compacted. Deletion is final only for edits the deleter had seen: an unseen concurrent edit resurrects the task. The convergence scenario uses a simulated network cut (offgrid's fault injection) on localhost; no real WiFi or phones.
- A clock once far ahead drags the HLC with it (`offgrid store info` warns). A remote stamp more than 1 h ahead is refused by `Clock.Update`; a last-writer-wins merge will then prefer that remote value.
- Counters are per process; the tiers panel in `top` shows only the latest `agent -route` run.
- One writer per SQLite file. Two `drain` workers on one queue are safe (fencing) but publish under the same snapshot name.
- The complexity gate is a closed vocabulary: it is explainable and measured on held-out cases, but it will miss phrasings it has not seen.
- Suggestions (Day 23): staleness is judged on the device where a person accepts, from the edits it has received; an edit on an unsynced device is invisible until sync, after which the suggestion shows STALE. If two devices resolve one suggestion differently, the earlier resolution in the canonical order wins on both. Deleting a task keeps its suggestions in the log.
- Tier 2 (Day 22) is a background job: a bigger local model (`-deep-model`) plus an optional OpenAI-compatible cloud client (`-cloud-url`, off by default; it sends task data off the device). Both are tested only against stand-in servers here. The idle gate reads CPU use from `/proc/stat` (fails open elsewhere).

## 9. Where to start reading

1. `internal/router/router.go`: the decision, in one function.
2. `internal/jobqueue/queue.go`: claim, fence, complete, release.
3. `internal/drain/worker.go`: the loop that uses them.
4. `internal/drain/kill_test.go`: the test that makes the claims above believable.
5. `docs/day-NN.md`: the concept behind each day's code.
