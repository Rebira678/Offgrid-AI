# Offgrid

**An AI agent runtime that keeps working when the internet doesn't.**

A small local model makes instant decisions, a durable queue holds the hard
thinking until a connection returns, and replicas that were apart merge their
edits deterministically when they meet again. Everything runs on one laptop
with no GPU and costs $0.

[![Go Version](https://img.shields.io/badge/Go-1.22+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![Status](https://img.shields.io/badge/Release-0.1.0--v0.1-success)](https://github.com/Rebira678/Offgrid-AI)
[![Tests](https://img.shields.io/badge/Tests-Passing%20(980%2B%20tests)-brightgreen)](https://github.com/Rebira678/Offgrid-AI)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

> Status: **Day 25: Benchmarks Anyone Can Reproduce (Store, Sync, Queue & Memory Footprint).**
> Tasks are logged, updated and reviewed entirely offline; the data survives restarts and `kill -9`;
> hard requests wait in a durable queue. Tier 2 slow-thinking runs a larger 7-8B local model
> only while the machine is quiet (`-idle-max-busy`), with optional cloud failover (`-cloud-url`).
> An AI never overwrites an existing task: changes become suggestions with base snapshots and stale detection; only a person accepts or dismisses them (`make ai-suggests-demo` / `offgrid suggestions -db FILE`).
> Answers and suggestions are persisted into the append-only op log and sync deterministically across replicas (`make two-brains-demo` / `make ai-suggests-demo`).
> Chaos simulation verifies convergence across 1,000+ random schedules (`make sim`).
> System costs and resource usage are measured and verified with reproducible benchmarks (`make bench-system` / `make bench`).
> See [Honest limits](#honest-limits).

Read next: [the architecture](ARCHITECTURE.md) (guarantees, each with the test
that enforces it) and [Why I used CRDTs for an AI agent](docs/article-why-crdts-for-an-ai-agent.md) (draft).

---

## Quickstart: three commands

```bash
sudo apt install -y build-essential golang   # once: C compiler (for SQLite) and Go 1.22+
make build && make test                      # 1. build and test
scripts/demo-offline.sh                      # 2. see it work with no network and no model
```

That demo logs tasks, restarts, shows them again, then deletes the task table
and rebuilds it from the log. Nothing in it needs Ollama or the internet.

Want the language model too (Tier 1, for messages the rules do not understand)?

```bash
curl -fsSL https://ollama.com/install.sh | sh && scripts/pull-models.sh
./bin/offgrid agent -seed                    # type messages; shows which tier answered
```

Without a model, `offgrid agent -no-model` still handles everything the rules
cover, and says so politely for the rest.

---

## What works today

| Day | What | Try it |
|---|---|---|
| 1 | Measure local model speed honestly | `offgrid bench` |
| 2 | Streaming client, typed errors | `offgrid ask "..."` |
| 3 | Free text to a validated command | `offgrid parse "close the pump ticket"` |
| 4 | Closed tool menu, code re-validates what the model proposes | `offgrid exec -seed '{"intent":"list_tasks"}'` |
| 5 | Tier 0 rules first, model second | `offgrid agent -seed -no-model` |
| 6 | Event-sourced SQLite store: append-only op log plus a rebuildable view | `offgrid store -db FILE verify` |
| 7 | Automated offline demo & developer test harness | `make demo` / `make check` |
| 8 | Connectivity monitor: ONLINE / DEGRADED / OFFLINE with hysteresis | `offgrid net` |
| 9 | Durable job queue: backoff with jitter, idempotency keys, leases, dead-letter | `offgrid queue stats` |
| 10 | Router: answer now, or save hard requests in the queue with a visible status. **Checkpoint 1** | `offgrid agent -route -net offline` |
| 11 | A switch to break your own network: injected latency, packet loss and partitions | `scripts/partition-demo.sh` |
| 12 | Drain on reconnect, survive a crash: worker with concurrency limit, leases, checkpoints, stored results | `scripts/drain-demo.sh` / `offgrid drain` |
| 13 | See the system: `log/slog` logs, counters, and a one-screen live view of connection, queue and tiers | `scripts/top-demo.sh` / `offgrid top` |
| 14 | Recap: `ARCHITECTURE.md` v1, release v0.1 | `ARCHITECTURE.md`, `scripts/release-v0.1.sh` |
| 15 | Hybrid logical clock: stamps that survive slow, fast and backwards clocks | `offgrid clock demo` |
| 16 | LWW-Map: per-field last-writer-wins registers, pure merge, node-ID tie-break | `offgrid lww demo` |
| 17 | Property tests (`rapid`): merge is commutative, associative, idempotent; broken merges are caught | `scripts/prove-merge.sh` |
| 18 | OR-Set: add-wins observed-remove set with tombstones (notes, task lists) | `offgrid orset demo` |
| 19 | Sync protocol: version summaries, delta exchange, idempotent apply, HMAC auth | `make sync-demo` / `offgrid sync serve` / `once` |
| 20 | **Checkpoint 2:** two replicas edit offline (chaos switch), reconnect, state hashes match; delete task / note | `make converge-demo` / `scripts/converge-demo.sh` |
| 21 | Recap: README, the long-form article draft, the re-cut flagship video plan | `docs/day-21.md` |
| 22 | Tier 2 Deliberate Reasoning: local 7-8B slow thinking gated by CPU idle meter, optional cloud failover, answers synced via op log | `make two-brains-demo` / `offgrid answers -db FILE` |
| 23 | The AI never overwrites a human: proposals sit alongside tasks with stale detection; CLI accept/dismiss | `make ai-suggests-demo` / `offgrid suggestions -db FILE` |
| 24 | Deterministic simulation testing: 1,000+ random failure schedules, log oracle, schedule shrinker | `make sim` / `make sim-quick` / `offgrid sim` |
| 25 | Benchmarks anyone can reproduce: store, sync and queue times, and memory | `make bench-system` / `make bench` / `offgrid perf -quick` |

Keep tasks between runs with `-db FILE` (or `export OFFGRID_DB=~/offgrid/tasks.db`).

---

## Architectural Flow

```
                              [ User Command ]
                                     |
                                     v
                        +--------------------------+
                        |  Tiered Intent Router    |
                        +--------------------------+
                         /            |           \
           (Deterministic)     (SLM: ~1-3s)     (Complex Request)
                       v              v                   v
             +----------------+ +---------------+ +--------------------+
             | Tier 0: Regex  | | Tier 1: SLM   | | Tier 2: Job Queue  |
             | (Closed Vocab) | | (Llama 1B/3B) | | (Durable Deferral) |
             +----------------+ +---------------+ +--------------------+
                         \            |                    |
                          v           v           (Worker & Idle Gate)
                        +---------------+                  v
                        | Closed Tools  |         +--------------------+
                        | ("Propose,    |         | 7-8B Model / Cloud |
                        |   Dispose")   |         | (Quiet CPU / Net)  |
                        +---------------+         +--------------------+
                                |                          |
                       (Suggest Wrapper)                   |
                                v                          |
                        +---------------+                  |
                        | Suggestions   |                  |
                        | (Human Review)|                  |
                        +---------------+                  |
                                |                          |
                                \                          /
                                 v                        v
                        +----------------------------------+
                        |     Event-Sourced SQLite Log     |
                        | (`ops`: tasks, notes, answers,   |
                        |         suggestions)             |
                        +----------------------------------+
                                        |
                                        v
                        +----------------------------------+
                        |   Materialized Views (v4 DDL)    |
                        |   Disposable & 100% Rebuildable  |
                        +----------------------------------+
                                        |
                        (Sync Protocol & CRDT Merge: HLC)
                                        v
                           [ Remote Replica Nodes ]
```

---

## Benchmarks

Measured with `offgrid bench` (medians of 3 runs after a warm-up) and `make bench-store`. All numbers are measured directly on the target hardware:

**Hardware:** HP EliteBook 845 G8, AMD Ryzen 5 PRO 5650U (6 cores / 12 threads), 16 GB RAM, consumer SSD, no GPU used for inference. Zorin OS 18.1.

### 1. Local Language Model Inference (`offgrid bench`)

```bash
make bench-models     # or: make bench (runs system + models)
```

<!-- bench:start -->
**Hardware:** HP EliteBook 845 G8, AMD Ryzen 5 PRO 5650U (6 cores / 12 threads), 16 GB RAM, consumer SSD, no GPU used for inference. Zorin OS 18.1.

| Model | Prompt Type | Load Time | Time to First Token | Generation Rate | Prompt Processing |
|---|---|---|---|---|---|
| **Llama 3.2 (1B)** | Short (60 tok) | 3.3 s | **0.7 s** | **16.0 tok/s** | 88.0 tok/s |
| **Llama 3.2 (1B)** | Long (1,176 tok) | 3.3 s | 23.7 s | **13.9 tok/s** | 49.7 tok/s |
| **Llama 3.2 (3B)** | Short (60 tok) | 0.0 s | **1.0 s** | **9.9 tok/s** | 61.5 tok/s |
| **Llama 3.2 (3B)** | Long (1,176 tok) | 0.0 s | 16.5 s | **9.0 tok/s** | 71.8 tok/s |
<!-- bench:end -->

### 2. Storage & CRDT Replay Throughput (`make bench-store`)

```bash
make bench-store
```

| Operation | Scale | Latency / Rate | Architectural Guarantees |
|---|---|---|---|
| **Committed Write** (`BenchmarkAppend`) | 1 op | **910 µs / op** (~1,100 ops/s) | Full WAL flush (`PRAGMA synchronous=FULL`), trigger check, and view update. |
| **Rebuild View** (`BenchmarkRebuild`) | 1,000 ops | **23.1 ms** (~43,000 ops/s) | In-memory reconstruction of full materialized view from scratch. |
| **Rebuild View** (`BenchmarkRebuild`) | 10,000 ops | **233.6 ms** (~42,800 ops/s) | Entire dataset reconstructed in under a quarter of a second. |
| **Rebuild View** (`BenchmarkRebuild`) | 50,000 ops | **1.15 s** (~43,500 ops/s) | 50,000 historical ops replayed in barely over 1 second. |
| **CRDT Register Projection** (`BenchmarkFields`) | 10,000 ops | **26.0 ms** (~384,000 ops/s) | Reads op-log and folds into LWW-Map per-field registers with single allocation. |
| **HLC Timestamp Issue** (`BenchmarkNow`) | 1 op | **55.4 ns** (~18,000,000 ops/s) | Thread-safe, monotonic, causal timestamp issuance (0 allocs). |
| **CRDT Merge (Small)** (`BenchmarkMergeSmall`) | 4 fields | **345 ns** (~2,900,000 merges/s) | Pure semilattice join over per-field registers. |

### 3. System Costs & Resource Footprint (`make bench-system` / `offgrid perf`)

**System costs** (no model involved; `make bench` regenerates this block, and its Hardware line says which machine produced it):

<!-- perf:start -->
**Hardware:** AMD Ryzen 5 PRO 5650U with Radeon Graphics, 12 threads, 14.9 GiB RAM, Zorin OS 18.1  
**Run:** 2026-10-30, go1.26.5, median of 3 runs (min to max in brackets), seed 1, files in `/tmp/offgrid-perf-1056186326` (ext4), CPU governor `performance`. No model is involved in these numbers.

**Store** (one SQLite file, `synchronous=FULL`: every write is on disk when it returns):

| What | Ops | Time (s) | Ops/s | File (MiB) | Peak Go heap (MiB) | Peak RSS (MiB) |
|---|---|---|---|---|---|---|
| append (one durable write per op) | 1,000 | 1.04 [1.04 to 1.05] | 964 | 0.3 | 3.5 | 21 |
| open an existing log | 100 | 0.0007 [0.0006 to 0.0013] | - | 0.1 | 0.9 | 21 |
| rebuild the view from the log | 100 | 0.010 [0.0098 to 0.011] | 9,617 | 0.1 | 1.4 | 20 |
| open an existing log | 1,000 | 0.0007 [0.0007 to 0.0009] | - | 0.3 | 0.9 | 22 |
| rebuild the view from the log | 1,000 | 0.073 [0.069 to 0.074] | 13,766 | 0.3 | 3.2 | 23 |
| open an existing log | 10,000 | 0.0008 [0.0007 to 0.0011] | - | 2.3 | 1.0 | 24 |
| rebuild the view from the log | 10,000 | 0.748 [0.727 to 0.776] | 13,375 | 2.3 | 4.8 | 28 |

**Sync** (the replica that catches up must end with the same state hash as its source, or the run fails):

| What | Ops | Time (s) | Ops/s | HTTP requests | Peak Go heap (MiB) | Peak RSS (MiB) |
|---|---|---|---|---|---|---|
| new replica, in process | 100 | 0.0092 [0.0076 to 0.010] | 10,873 | - | 1.6 | 24 |
| new replica, HTTP on localhost | 100 | 0.0098 [0.0097 to 0.012] | 10,200 | 2 | 1.9 | 23 |
| 10 new ops on a 100-op log, HTTP | 10 | 0.0032 [0.0028 to 0.0032] | 3,166 | 2 | 1.2 | 24 |
| new replica, in process | 1,000 | 0.084 [0.079 to 0.087] | 11,945 | - | 3.6 | 25 |
| new replica, HTTP on localhost | 1,000 | 0.083 [0.083 to 0.086] | 11,978 | 3 | 3.6 | 26 |
| 10 new ops on a 1,000-op log, HTTP | 10 | 0.0037 [0.0034 to 0.0037] | 2,706 | 2 | 1.2 | 25 |
| new replica, in process | 10,000 | 0.943 [0.937 to 0.994] | 10,604 | - | 3.7 | 29 |
| new replica, HTTP on localhost | 10,000 | 0.976 [0.972 to 0.996] | 10,247 | 21 | 4.1 | 29 |
| 10 new ops on a 10,000-op log, HTTP | 10 | 0.0091 [0.0088 to 0.013] | 1,095 | 2 | 1.2 | 27 |

**Queue** (jobs that do nothing, so this is the queue's own cost: claim, run, complete, each a durable write):

| Jobs | At a time | Enqueue (s) | Drain (s) | Jobs/s drained | Peak Go heap (MiB) | Peak RSS (MiB) |
|---|---|---|---|---|---|---|
| 100 | 1 | 0.084 [0.083 to 0.084] | 0.186 [0.181 to 0.193] | 537 | 2.7 | 26 |
| 100 | 4 | 0.090 [0.089 to 0.098] | 0.182 [0.179 to 0.193] | 549 | 2.7 | 26 |
| 1,000 | 1 | 0.781 [0.779 to 1.62] | 1.74 [1.73 to 8.14] | 575 | 3.6 | 28 |
| 1,000 | 4 | 3.90 [3.86 to 4.11] | 8.14 [8.13 to 8.18] | 123 | 3.1 | 28 |

Peak RSS is the largest resident memory of the whole process during that row (counter reset before each row). Peak Go heap is sampled every 10 ms, so a very short spike can be missed. Sync over HTTP runs on one machine: it measures protocol, signing and storage cost, not a WiFi link.
<!-- perf:end -->

---

## Architecture

How the parts fit, what is guaranteed and what tests enforce it, and what is not built yet: [ARCHITECTURE.md](ARCHITECTURE.md).

---

## Layout

```
cmd/offgrid/            the CLI: models, bench, ask, parse, eval, exec, tools, agent, store, task, net, queue, drain, fault, top, clock, lww, orset, sync, answers, suggestions
internal/ollama/        HTTP client for Ollama: streaming, typed errors
internal/bench/         benchmark method and report writers
internal/sysinfo/       reads CPU and RAM so every result names its hardware
internal/intent/        the Command type and its validation
internal/structured/    getting valid JSON out of a model, with retries
internal/llm/           the interface models sit behind (Ollama, OpenAI-compatible cloud, and fakes)
internal/eval/          measures how reliably models produce valid commands
internal/task/          the Task type, matching rules, the Store and Suggester contracts
internal/task/storetest shared contract tests every store must pass
internal/suggest/       proposing wrapper: models propose changes, humans review and accept
internal/tools/         the closed menu of tools and the registry
internal/agent/         tiered loop: rules first, model second
internal/store/sqlstore SQLite store: op log, migrations, rebuild, verify, answers and suggestions projection
internal/connectivity   state machine, probers and the monitor goroutine
internal/jobqueue       durable job queue: backoff, jitter, leases, dead-letter
internal/router         answer now or defer: complexity gate, output validity, connectivity
internal/netfault       fault-injecting http.RoundTripper: latency, drop %, partition (file, schedule, flag)
internal/idle           measures CPU busy share (/proc/stat) so slow jobs start only when the machine is quiet
internal/drain          queue worker: concurrency, network gate, leases, release, checkpoints; slow-thinking handlers, safe drain
internal/obs            structured logs (slog), counters, snapshot files other processes can read
internal/dash           the one-screen view: a pure render function plus a repaint loop
internal/hlc            hybrid logical clock (Kulkarni et al.): ordering stamps that survive bad clocks
internal/crdt           LWW-Map (per-field registers) and OR-Set (add-wins sets): pure, order-independent merges
internal/syncer         sync over HTTP: version summaries, delta exchange, HMAC-signed requests and responses
internal/sim            deterministic simulation testing: random failure schedules, log oracle, schedule shrinker
internal/perf           reproducible benchmarks: store, sync, queue and memory footprint; Markdown, JSON and SVG reports
scripts/                pull-models.sh, demo-offline.sh, checkpoint1-demo.sh, partition-demo.sh, drain-demo.sh, top-demo.sh,
                        release-v0.1.sh, prove-merge.sh, sync-demo.sh, converge-demo.sh, converge-clip.sh, two-brains-demo.sh, ai-suggests-demo.sh
examples/               sample command and message files
docs/                   day-NN.md guides, architecture specifications, benchmarks, and article drafts
```

---

## Tests

As of Day 24, `go test -v ./...` runs **1,300+ test functions** (plus their subtests)
in 25 packages: about 20,500 lines of Go and about 30,000 lines of tests. One
test skips by design (a helper process that only runs when launched by another
test). Day 17 and later need `go get pgregory.net/rapid@v1.2.0` once.

```bash
make check                 # vet + gofmt + tests
go test -race ./...        # with the race detector
make sim                   # 1,000 random failure schedules (SEEDS=3000 make sim)
make sim-quick             # quick 100-schedule simulation run
scripts/prove-merge.sh     # the merge laws, and the broken merges they must catch
make converge-demo         # runs multi-replica convergence demo
make two-brains-demo       # runs Day 22 Tier 2 deliberate reasoning and idle gate demo
make ai-suggests-demo      # runs Day 23 safe AI suggestions and review demo
```

### Quality Guarantees
- **Deterministic Simulation Engine & Chaos Sweeps:** 1,000+ seeded failure schedules throw network partitions, severe clock skew (±90m), process crashes and restarts, dropped/duplicated/delayed/truncated/reversed sync replies, and gap injections at 3 real SQLite replicas. Assertions mathematically guarantee causal HLC monotonicity, atomic refusal, state hash convergence, and log oracle validation (recomputing deletes and suggestion resolutions from raw ops). A delta shrinker reduces failing 80-action traces to minimal reproducible test cases.
- **AI Suggestion Isolation & Stale Protection:** Proving mathematically and empirically that an AI model wrapper (`suggest.Store`) and safe queue drain (`drain.InterpretSafely`) cannot overwrite existing tasks. Base snapshot comparison prevents accepting stale proposals if a human concurrently edited the targeted fields.
- **Add-Wins Suggestion Lifecycle & Ring Sync:** Multi-replica convergence tests verify that suggestions, acceptances, and dismissals sync canonically over HLC timestamps across 3-node ring topologies, and SQLite rebuild from raw op log reconstructs suggestions with byte-for-byte state hash equality.
- **Contract Testing (`storetest`):** Shared contract test suite runs identically against `MemStore` and `sqlstore.Store`, including `task.Remover` (`DeleteTask`, `RemoveNote`).
- **Differential Random-Walk Testing:** `TestSQLiteAgreesWithTheInMemoryStoreOnRandomWorkloads` executes 5,000 operations across 25 random seeds comparing `sqlstore` directly against the in-memory oracle.
- **Causal & Monotonic Clock Testing:** Multi-node simulation across 25 random seeds and ±5s clock drift (66,677 messages verified with 0 HLC misorderings vs 33,190 wall clock failures).
- **CRDT Lattice & Purity Verification:** `internal/crdt` verifies commutativity, associativity, and idempotence of `Merge` over 20,000 randomized comparisons and 200 random multi-replica sync scenarios.
- **Property-Based Testing (`pgregory.net/rapid`):** 67,000+ generated test cases across 21 formal algebraic properties mathematically proving that `crdt.Merge` and `crdt.MergeSet` form bounded join-semilattices. Deliberately broken merge mutants (12 anti-patterns) are proven caught and shrunk.
- **Observed-Remove Set (OR-Set) Lattice & Oracle Proofs:** Add-wins semantics with tombstones proven via Rapid (3,000 cases per law). Proves mathematically that deleting an item never destroys an unseen concurrent edit.
- **Multi-Replica Convergence:** Chaos tests and end-to-end multi-process simulation (`scripts/converge-demo.sh`) subject replicas to network cuts, concurrent offline edits, deletions, and lossy networks. Replicas reconnect and achieve 100% cryptographic state hash parity.
- **Tier 2 Deliberate Reasoning & Idle Gate:** End-to-end testing of `internal/idle` CPU sampler, window boundaries, `/proc/stat` counter resets, graceful fail-open on non-Linux, and worker idle backoff (`make two-brains-demo`).
- **Cloud Failover & Dead-Letter Isolation:** Exhaustive failure testing verifying that cloud HTTP 401/403/429/500/503 errors safely trigger automatic fallback to local models, while missing local models (`*llm.ModelNotFoundError`) are marked `Permanent` to prevent endless CPU burns.
- **Answer Synchronization & Rebuild Parity:** Three-replica out-of-order delta sync tests prove that Tier 2 answers converge canonically across partitions. Tamper injection tests prove `Verify` detects corrupted projection rows and `Rebuild` restores cryptographic op-log parity.
- **UTF-8 Boundary Safety:** Truncation guards ensure multi-byte UTF-8 runes (Japanese, Amharic, Emoji) crossing the 8KB limit are never bisected, preventing JSON encoding or SQLite projection errors.
- **Race Detection:** Clean under `go test -race ./...`.
- **Adversarial & Tamper Testing:** Unit tests deliberately corrupt SQLite database rows and assert that `Verify` detects the divergence and `Rebuild` restores cryptographic parity.
- **Crash-Resilience Testing (`kill -9`):** Subprocess-level SIGKILL injection tests verify that killed workers in mid-execution leave no leaked leases, duplicate results, or corrupted state.

---

## Honest limits

- **No real devices, no real network.** Everything ran on one HP EliteBook
  over localhost. Network cuts are simulated by the fault-injection transport.
- **Sync is signed, not encrypted** (HMAC with a shared key). Use a trusted
  network or put TLS in front. The signature has a five-minute clock window,
  and an op stamped more than an hour in the future is refused.
- **Deleting is not final against unseen edits:** an edit made by a replica that
  had not seen the delete brings the task back (add-wins). Only the command
  line deletes; the language model cannot.
- **Task IDs carry the device that made them (Day 24).** `T3.laptop`; screens
  show `T3` and `T3` still finds it when exactly one task has that number. This
  fixed a bug the simulator found: two devices that each created a first task
  before meeting both chose `T1` and could never sync. Databases written before
  Day 24 still hold plain IDs; two such databases that collided are still refused
  with a clear error, and nothing is changed.
- **The simulation checks convergence and two recomputed rules** (delete vs edit,
  first resolution of a suggestion), not every meaning. A store that converged
  on a wrong answer the oracles do not recompute would pass. It models neither a
  disk that loses committed writes, nor a replica restored from an old backup
  (that forks the log, which sync refuses on purpose), nor a hostile peer.
- **The log only grows.** No compaction of old ops or tombstones.
- **Model quality is measured separately** (`offgrid bench`, `offgrid eval`);
  the benchmark table above is filled from measurements on this machine.
- Upgrade both replicas together: an older replica refuses ops it does not know.

---

## Design rules

- The model proposes, the code disposes: nothing a model says is trusted until validated.
- Precision over coverage: when unsure, ask or refuse, never guess a task.
- The op log is the truth; tables are a disposable cache.
- Honest numbers only, always with the hardware named.

---

