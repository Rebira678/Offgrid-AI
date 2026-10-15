# Offgrid: Offline-First AI Agent Runtime

> **An AI agent runtime designed to keep operating when the internet goes down. Features tiered intent execution (sub-millisecond rules to edge SLMs), an immutable event-sourced SQLite log, and conflict-free multi-replica CRDT synchronization.**

Everything runs locally on consumer x86_64 CPU hardware with no GPU, zero cloud dependencies, and $0 infrastructure cost.

[![Go Version](https://img.shields.io/badge/Go-1.22+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![Status](https://img.shields.io/badge/Release-0.10.0--day10-success)](https://github.com/)
[![Tests](https://img.shields.io/badge/Tests-Passing%20(366%2B%20cases)-brightgreen)](https://github.com/)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

> Status: **Day 10 (Checkpoint 1).** A field worker can log, update and review tasks entirely offline, the data survives restarts, the program knows whether it is ONLINE, DEGRADED or OFFLINE, and work that cannot run now is kept in a durable queue. A router decides per message whether to answer now or save it. Draining the queue (Day 12) and sync come next.

---

## What Works Today (Week 1)

| Day | Capability | Verification & CLI Command |
|---|---|---|
| **1** | Empirical hardware benchmarking & token speeds | `offgrid bench` |
| **2** | Streaming local LLM client & typed errors | `offgrid ask "what is a CRDT?"` |
| **3** | Constrained JSON schema & bounded retry loops | `offgrid parse "close the pump ticket"` |
| **4** | Closed tool registry & blast-radius containment | `offgrid exec -seed '{"intent":"list_tasks"}'` |
| **5** | Tiered agent loop (Tier 0 rules vs Tier 1 model) | `offgrid agent -seed -no-model` |
| **6** | Event-sourced SQLite log & zero-drift view rebuilds | `offgrid store -db FILE verify` |
| **7** | Automated offline demo & developer test harness | `make demo` / `make check` |
| **8** | Connectivity monitor: ONLINE / DEGRADED / OFFLINE with hysteresis | `offgrid net` |
| **9** | Durable job queue: backoff with jitter, idempotency keys, leases, dead-letter | `offgrid queue stats` |
| **10** | Router: answer now or defer to durable queue with visible status (Checkpoint 1) | `offgrid agent -route -net offline` |


---

## The Problem
Most AI agent frameworks assume constant gigabit connectivity, unbounded cloud API budgets, and zero-latency network roundtrips. When connectivity drops—in field operations, disaster response, remote utility grids, agriculture, or offline logistics—these systems fail completely.

Offgrid is engineered with **local-first, resilience-first, and distributed systems principles**:

```
                              [ User Command ]
                                     |
                                     v
                        +--------------------------+
                        |  Tiered Intent Router    |
                        +--------------------------+
                         /                        \
           (Deterministic: 26 µs)        (Natural Language: ~1-3s)
                       v                            v
             +--------------------+       +--------------------+
             | Tier 0: Regex Rules|       | Tier 1: Local SLM  |
             | (Closed Vocabulary)|       | (Llama 3.2 1B/3B)  |
             +--------------------+       +--------------------+
                         \                        /
                          v                      v
                        +--------------------------+
                        |   Closed Tool Registry   |
                        | ("Model Proposes, Code   |
                        |        Disposes")        |
                        +--------------------------+
                                     |
                                     v
                        +--------------------------+
                        | Event-Sourced SQLite Log |
                        | (Immutable `ops` table)  |
                        +--------------------------+
                                     |
                                     v
                     +--------------------------------+
                     | Materialized View (`tasks`)    |
                     | Disposable & 100% Rebuildable  |
                     +--------------------------------+
                                     |
                    (Sync Protocol & CRDT Merge: HLC)
                                     v
                        [ Remote Replica Nodes ]
```

---

## Core Architectural Pillars

### 1. Tiered Execution Pipeline ("Rules Before Models")
Calling an LLM for structured, repetitive commands waste seconds of battery and latency. Offgrid implements an asymmetric routing loop:
- **Tier 0 (Deterministic Rules):** A strict closed-vocabulary tokenizer resolves common domain intents (`list_tasks`, `daily_summary`, `create_task`) in **26 microseconds** with 100% precision. If an unfamiliar token appears, it safely stays silent.
- **Tier 1 (Edge SLM):** Natural language commands ("I finished replacing the gasket on pump 4") route to local small language models (Llama 3.2 1B/3B via Ollama) with constrained JSON decoding and bounded retries.
- **Tier 2 (Durable Queue):** Heavy analytical reasoning jobs are deferred to an offline-safe SQLite job queue with exponential backoff and jitter.

### 2. "Model Proposes, Code Disposes"
Language models are untrusted natural language translators, never direct database mutators:
- Both Tier 0 and Tier 1 terminate at the exact same **closed tool registry**.
- The Go runtime validates schemas, bounds parameter values, resolves ambiguities (prompting for clarification if multiple tasks match), and enforces business logic.
- A hallucinating model cannot delete tables or execute unauthorized mutations.

### 3. Event-Sourced Storage ("The Log is Truth, Tables are a Cache")
Traditional CRUD architectures overwrite rows in place, discarding historical facts and making offline multi-master sync impossible:
- **Append-Only Op Log:** State transitions (`task.created`, `task.updated`, `task.note_added`) are written to an immutable `ops` table with unique sequence pairs `(node_id, seq)` and nanosecond timestamps.
- **Engine-Level Immutability:** SQLite triggers (`BEFORE UPDATE`, `BEFORE DELETE`) physically reject modifications to past operations at the database engine level.
- **Disposable Projections:** The `tasks` and `task_notes` tables are materialized view caches. Dropping or corrupting them triggers an automatic rebuild from the immutable log.
- **Pure Replay Function (`applyOp`):** Never accesses the system clock; every timestamp and ID is frozen into the op at commit time, guaranteeing bit-for-bit replay equivalence.
- **Cryptographic State Verification:** `offgrid store verify` replays history into an in-memory scratch database (`:memory:`) and asserts state parity via canonical SHA-256 state hashes.

---

## Quickstart

### Three Commands: Fresh Clone to Running Demo
```bash
sudo apt install -y build-essential golang   # C compiler (for SQLite CGO) & Go 1.22+
make build && make test                      # 1. Build binary and verify tests pass
make demo                                    # 2. Run offline demo (no network, no model)
```

The offline demo runs end-to-end: it creates tasks with Tier 0 rules, restarts in a new process to prove persistence, verifies the log against a scratch replay, destroys the `tasks` table with `DROP TABLE`, and rebuilds it bit-for-bit.

### Interactive CLI Workflows
```bash
# 1. Seed tasks into persistent SQLite storage
./bin/offgrid exec -db offgrid.db -seed '{"intent":"create_task","title":"Inspect solar inverter","site":"4","priority":"high"}'

# 2. Query state via natural language (Tier 0 resolves in microseconds)
./bin/offgrid exec -db offgrid.db '{"intent":"list_tasks","site":"4"}'

# 3. Run interactive agent loop (without model)
./bin/offgrid agent -no-model

# 4. Optional: Run with local LLM (Tier 1 fallback via Ollama)
# curl -fsSL https://ollama.com/install.sh | sh && scripts/pull-models.sh
./bin/offgrid agent -db offgrid.db -seed

# 5. Inspect storage metadata and facts log
./bin/offgrid store -db offgrid.db info
./bin/offgrid store -db offgrid.db ops -n 5

# 6. Cryptographic integrity check (scratch in-memory replay vs live view)
./bin/offgrid store -db offgrid.db verify

# 7. The "Kill-the-View" Test: Drop tables and rebuild from the immutable log
sqlite3 offgrid.db 'DROP TABLE tasks; DROP TABLE task_notes;'
./bin/offgrid store -db offgrid.db rebuild

# 8. Checkpoint 1: Routing with complexity gate & durable queue deferral
./bin/offgrid agent -route -net offline "summarize this week's incidents and suggest priorities"
./bin/offgrid agent -route -net offline "queue"
./bin/offgrid agent -route-eval
```

---

## Empirical Benchmarks

All benchmarks are measured directly on consumer mobile hardware (**AMD Ryzen 5 PRO 5650U**, 6 cores / 12 threads, 16 GB RAM, consumer SSD, no GPU inference):

### 1. Storage & Replay Throughput (`make bench-store`)
```bash
make bench-store
```

| Operation | Scale | Latency / Rate | Architectural Guarantees |
|---|---|---|---|
| **Committed Write** (`BenchmarkAppend`) | 1 op | **910 µs / op** (~1,100 ops/s) | Full WAL flush (`PRAGMA synchronous=FULL`), trigger check, and view update. |
| **Rebuild View** (`BenchmarkRebuild`) | 1,000 ops | **23.1 ms** (~43,000 ops/s) | In-memory reconstruction of full materialized view from scratch. |
| **Rebuild View** (`BenchmarkRebuild`) | 10,000 ops | **233.6 ms** (~42,800 ops/s) | Entire dataset reconstructed in under a quarter of a second. |
| **Rebuild View** (`BenchmarkRebuild`) | 50,000 ops | **1.15 s** (~43,500 ops/s) | 50,000 historical ops replayed in barely over 1 second. |

### 2. Tier 0 vs Tier 1 Intent Routing Latency
```bash
./bin/offgrid agent -eval
```
- **Tier 0 (Deterministic Rules):** **26 µs** median latency (100% precision on domain commands).
- **Tier 1 (Edge SLM):** **~1.0s – 3.2s** (handles arbitrary linguistic variations).
- **Efficiency Gain:** Tier 0 is **~100,000x faster** than invoking an LLM for deterministic intents.

### 3. Local Model Token Generation (`offgrid bench`)

| Model | Prompt Type | Load Time | Time to First Token | Generation Rate | Prompt Processing |
|---|---|---|---|---|---|
| **Llama 3.2 (1B)** | Short (60 tok) | 3.3 s | **0.7 s** | **16.0 tok/s** | 88.0 tok/s |
| **Llama 3.2 (1B)** | Long (1,176 tok) | 3.3 s | 23.7 s | **13.9 tok/s** | 49.7 tok/s |
| **Llama 3.2 (3B)** | Short (60 tok) | 0.0 s | **1.0 s** | **9.9 tok/s** | 61.5 tok/s |
| **Llama 3.2 (3B)** | Long (1,176 tok) | 0.0 s | 16.5 s | **9.0 tok/s** | 71.8 tok/s |

---

## Repository Layout

```
cmd/offgrid/              CLI entrypoint (models, bench, ask, exec, agent, store, net, queue)
internal/agent/           Tiered orchestrator (Tier 0 rules -> Tier 1 local model fallback)
internal/connectivity/    Connectivity state machine, probers, and monitor goroutine
internal/jobqueue/        Durable job queue: backoff with jitter, leases, dead-letter
internal/router/          Answer now or defer: complexity gate, output validity, connectivity
internal/store/sqlstore/  Event-sourced SQLite engine (immutable log, triggers, rebuild, verify)
internal/tools/           Closed tool registry with strict input schema validation
internal/task/            Task domain models and storage contracts
internal/intent/          Intent schema definitions and validation logic
internal/structured/      Constrained JSON extraction with bounded retry loops
internal/llm/             LLM client interfaces and anti-corruption adapters
internal/ollama/          Streaming HTTP client for local Ollama daemon
scripts/                  Automated test and offline demonstration runners (demo-offline.sh, checkpoint1-demo.sh)
```

---

## Developer Targets

```bash
make check            # Runs go vet, gofmt verification, and full test suite
make test-race        # Executes test suite under Go race detector
make bench-store      # Executes storage append & view rebuild benchmarks
make demo             # Runs end-to-end offline demo (pass PAUSE=2 for recordings)
make demo-checkpoint1 # Runs Checkpoint 1 demo: WiFi off, agent keeps working, queue fills
```

---

## Testing & Quality Guarantees

The codebase adheres to rigorous reliability engineering standards:
- **Contract Testing (`storetest`):** The exact same contract test suite runs against both ephemeral `MemStore` and persistent `sqlstore.Store`.
- **Differential Random-Walk Testing:** `TestSQLiteAgreesWithTheInMemoryStoreOnRandomWorkloads` executes 5,000 operations across 25 random seeds comparing `sqlstore` directly against the in-memory oracle.
- **Race Detection:** Clean under `go test -race ./...`.
- **Adversarial & Tamper Testing:** Unit tests deliberately corrupt SQLite database rows and assert that `Verify` detects the divergence and `Rebuild` restores cryptographic parity.
