# Offgrid

**An AI agent runtime that keeps working when the internet doesn't.**

A small local model makes instant decisions, a durable queue holds the hard
thinking until a connection returns, and replicas that were apart merge their
edits deterministically when they meet again. Everything runs on one laptop
with no GPU and costs $0.

[![Go Version](https://img.shields.io/badge/Go-1.22+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![Status](https://img.shields.io/badge/Release-0.1.0--v0.1-success)](https://github.com/Rebira678/Offgrid-AI)
[![Tests](https://img.shields.io/badge/Tests-Passing%20(919%20tests)-brightgreen)](https://github.com/Rebira678/Offgrid-AI)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

> Status: **Day 22: Deliberate Reasoning (Tier 2), Idle Gating & Cloud Failover.**
> Tasks are logged, updated and reviewed entirely offline; the data survives restarts and `kill -9`;
> hard requests wait in a durable queue. Tier 2 slow-thinking runs a larger 7-8B local model
> only while the machine is quiet (`-idle-max-busy`), with optional cloud failover (`-cloud-url`).
> Answers are persisted into the append-only op log and sync deterministically across replicas (`make two-brains-demo`).
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
                                \                          /
                                 v                        v
                        +----------------------------------+
                        |     Event-Sourced SQLite Log     |
                        |  (`ops`: tasks, notes, answers)  |
                        +----------------------------------+
                                        |
                                        v
                        +----------------------------------+
                        |   Materialized Views (v3 DDL)    |
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
make bench     # writes docs/benchmarks.md and docs/benchmarks.json
```

| Model | Prompt Type | Load Time | Time to First Token | Generation Rate | Prompt Processing |
|---|---|---|---|---|---|
| **Llama 3.2 (1B)** | Short (60 tok) | 3.3 s | **0.7 s** | **16.0 tok/s** | 88.0 tok/s |
| **Llama 3.2 (1B)** | Long (1,176 tok) | 3.3 s | 23.7 s | **13.9 tok/s** | 49.7 tok/s |
| **Llama 3.2 (3B)** | Short (60 tok) | 0.0 s | **1.0 s** | **9.9 tok/s** | 61.5 tok/s |
| **Llama 3.2 (3B)** | Long (1,176 tok) | 0.0 s | 16.5 s | **9.0 tok/s** | 71.8 tok/s |

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

---

## Architecture

How the parts fit, what is guaranteed and what tests enforce it, and what is not built yet: [ARCHITECTURE.md](ARCHITECTURE.md).

---

## Layout

```
cmd/offgrid/            the CLI: models, bench, ask, parse, eval, exec, tools, agent, store, task, net, queue, drain, fault, top, clock, lww, orset, sync, answers
internal/ollama/        HTTP client for Ollama: streaming, typed errors
internal/bench/         benchmark method and report writers
internal/sysinfo/       reads CPU and RAM so every result names its hardware
internal/intent/        the Command type and its validation
internal/structured/    getting valid JSON out of a model, with retries
internal/llm/           the interface models sit behind (Ollama, OpenAI-compatible cloud, and fakes)
internal/eval/          measures how reliably models produce valid commands
internal/task/          the Task type, matching rules, the Store contract
internal/task/storetest shared contract tests every store must pass
internal/tools/         the closed menu of tools and the registry
internal/agent/         tiered loop: rules first, model second
internal/store/sqlstore SQLite store: op log, migrations, rebuild, verify, answers projection
internal/connectivity   state machine, probers and the monitor goroutine
internal/jobqueue       durable job queue: backoff, jitter, leases, dead-letter
internal/router         answer now or defer: complexity gate, output validity, connectivity
internal/netfault       fault-injecting http.RoundTripper: latency, drop %, partition (file, schedule, flag)
internal/idle           measures CPU busy share (/proc/stat) so slow jobs start only when the machine is quiet
internal/drain          queue worker: concurrency, network gate, leases, release, checkpoints; slow-thinking (Tier 2) handlers
internal/obs            structured logs (slog), counters, snapshot files other processes can read
internal/dash           the one-screen view: a pure render function plus a repaint loop
internal/hlc            hybrid logical clock (Kulkarni et al.): ordering stamps that survive bad clocks
internal/crdt           LWW-Map (per-field registers) and OR-Set (add-wins sets): pure, order-independent merges
internal/syncer         sync over HTTP: version summaries, delta exchange, HMAC-signed requests and responses
scripts/                pull-models.sh, demo-offline.sh, checkpoint1-demo.sh, partition-demo.sh, drain-demo.sh, top-demo.sh,
                        release-v0.1.sh, prove-merge.sh, sync-demo.sh, converge-demo.sh, converge-clip.sh, two-brains-demo.sh
examples/               sample command and message files
docs/                   day-NN.md guides, architecture specifications, benchmarks, and article drafts
```

---

## Tests

As of Day 22, `go test -v ./...` runs **919 test functions** (plus their subtests)
in 23 packages: about 17,500 lines of Go and about 26,500 lines of tests. One
test skips by design (a helper process that only runs when launched by another
test). Day 17 and later need `go get pgregory.net/rapid@v1.2.0` once.

```bash
make check                 # vet + gofmt + tests
go test -race ./...        # with the race detector
scripts/prove-merge.sh     # the merge laws, and the broken merges they must catch
make converge-demo         # runs multi-replica convergence demo
make two-brains-demo       # runs Day 22 Tier 2 deliberate reasoning and idle gate demo
```

### Quality Guarantees
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
- **Two replicas that each created tasks before ever syncing** both own a `T1`;
  the first sync is refused with a clear error and changes nothing.
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

## Roadmap

The 30-day plan is in the roadmap file; each day has a guide in `docs/`.
