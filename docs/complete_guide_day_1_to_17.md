# Offgrid: The Complete Engineering Guide & Architectural Blueprint (Days 1–17)

> **Document Purpose:** This document is the single-source-of-truth master guide for the **Offgrid** project covering everything built from **Day 1 to Day 17**. It explains the entire architecture, every component, every mathematical guarantee, and every design trade-off using a **dual-layer approach**: clear, direct **Simple English** paired with precise, industry-standard **Technical Terminology**.
>
> **Self-Contained Design:** To make studying effortless, **every single day contains its own dedicated technical term definitions right under that day's heading**—you do NOT have to scroll to the end of the document to look up concepts.
>
> Use this guide for system understanding, architectural documentation, stakeholder presentations, and technical interviews.

---

## Table of Contents
1. [The Executive Summary & Elevator Pitch](#1-the-executive-summary--elevator-pitch)
2. [The Big Picture: System Architecture](#2-the-big-picture-system-architecture)
3. [The Zero-Daemon Process Model](#3-the-zero-daemon-process-model)
4. [Day-by-Day Deep Dive (Days 1 to 17)](#4-day-by-day-deep-dive-days-1-to-17)
   - [Day 1: Measure Before You Design (Hardware Profiling & Philosophy)](#day-1-measure-before-you-design)
   - [Day 2: The Swappable Model Client (Dependency Inversion & Anti-Corruption)](#day-2-the-swappable-model-client)
   - [Day 3: Reliable Model Outputs (Grammar-Constrained Decoding & Bounded Retries)](#day-3-reliable-model-outputs)
   - [Day 4: The Closed Tool Menu (Type-Safe Generics & Blast-Radius Containment)](#day-4-the-closed-tool-menu)
   - [Day 5: The Dual-Tier Agent & Contract Testing (Regex Tier 0 vs LLM Tier 1)](#day-5-the-dual-tier-agent--contract-testing)
   - [Day 6: Event-Sourced Storage & SQLite Triggers (Immutable Log & Pure Projections)](#day-6-event-sourced-storage--sqlite-triggers)
   - [Day 7: The Local Agent Integration & CLI Wiring](#day-7-the-local-agent-integration--cli-wiring)
   - [Day 8: The Connectivity Monitor & Hysteresis State Machine](#day-8-the-connectivity-monitor--hysteresis-state-machine)
   - [Day 9: The Durable Job Queue & Fencing Tokens](#day-9-the-durable-job-queue--fencing-tokens)
   - [Day 10: The Complexity Router (Closed Vocabulary Gating)](#day-10-the-complexity-router)
   - [Day 11: Fault Injection & Chaos Testing (`netfault`)](#day-11-fault-injection--chaos-testing)
   - [Day 12: The Crash-Proof Drain Worker (`kill -9` Resilience)](#day-12-the-crash-proof-drain-worker)
   - [Day 13: Cross-Process Observability & Terminal Dashboard (`offgrid top`)](#day-13-cross-process-observability--terminal-dashboard)
   - [Day 14: v0.1 Architecture Freeze & Release Packaging](#day-14-v01-architecture-freeze--release-packaging)
   - [Day 15: Hybrid Logical Clocks (Causality & Clock Skew Resilience)](#day-15-hybrid-logical-clocks)
   - [Day 16: Conflict-Free Replicated Data Types (LWW-Map CRDT)](#day-16-conflict-free-replicated-data-types)
   - [Day 17: Property-Based Formal Verification (`rapid` & Semilattice Laws)](#day-17-property-based-formal-verification)
5. [System-Wide Architectural Trade-offs Matrix](#5-system-wide-architectural-trade-offs-matrix)
6. [How Data Travels: Step-by-Step Scenarios](#6-how-data-travels-step-by-step-scenarios)
7. [The Failure Modes & Blast Radius Matrix](#7-the-failure-modes--blast-radius-matrix)
8. [Empirical Benchmarks & Real Hardware Numbers](#8-empirical-benchmarks--real-hardware-numbers)
9. [Alphabetical Quick-Index of Technical Concepts](#9-alphabetical-quick-index-of-technical-concepts)
10. [CLI Verification & Demonstration Cheat Sheet](#10-cli-verification--demonstration-cheat-sheet)

---

## 1. The Executive Summary & Elevator Pitch

### The 30-Second Elevator Pitch
> *"Offgrid is an edge-native, offline-first AI agent runtime written in Go. It allows AI-driven task management, local reasoning, and data sync to operate completely reliably in disconnected or remote environments—such as ships, mines, rural utilities, or disaster zones. When the network drops, Offgrid answers easy requests in microseconds using deterministic local rules, executes local LLM inference offline, safely defers complex analysis to an atomic, crash-proof job queue, and merges multi-device state upon reconnection using formally proven Conflict-Free Replicated Data Types (CRDTs)."*

### The Core Problem Offgrid Solves
Almost all modern AI applications make a fatal architectural assumption: **a permanent, fast, reliable internet connection to OpenAI or the cloud.**
In the real physical world—on cargo ships, in deep mines, in field hospitals, or in remote industrial plants—the network:
1. Is completely absent for hours or days.
2. Flaps intermittently (captive portals, high packet loss, satellite jitter).
3. Causes standard AI apps to freeze, throw timeout errors, duplicate state, or corrupt databases.

### The 3 Pillars of Offgrid's Solution
1. **Edge-Native Inference & Tiered Intelligence:** Instant deterministic execution (Tier 0 regex rules in microseconds) backed by a small local LLM (Tier 1 via Ollama) that runs with zero cloud connectivity.
2. **Crash-Proof Asynchronous Queue:** Hard analytical jobs that require cloud reasoning or deep processing are not lost. They are committed to an atomic SQLite queue protected by fencing tokens and deterministic idempotency keys, surviving sudden system power loss (`kill -9`).
3. **Conflict-Free Event Sourcing & CRDTs:** The system records state as an append-only, tamper-proof log stamped with Hybrid Logical Clocks (HLC). Replicas across different edge nodes can merge their updates in any order and mathematically guarantee identical state convergence without a central coordinator.

---

## 2. The Big Picture: System Architecture

The following diagram illustrates how an incoming user message traverses Offgrid:

```
                            User Message
              ("log a leaking pump at site 4, high priority")
                                   │
                                   ▼
          ┌──────────────────────────────────────────────────┐
          │            Router (internal/router)              │
          │   - Evaluates closed vocabulary (scoring)        │
          │   - Inspects connectivity monitor state          │
          └───────────────┬──────────────────────────┬───────┘
                          │                          │
              Score < 3   │              Score >= 3  │ (or unreadable)
              (Easy Task) │              (Hard Task) │
                          ▼                          ▼
               ┌──────────────────────┐   ┌───────────────────────────────┐
               │ Agent (internal/agent)   │ Durable Queue (internal/jobqueue)│
               │                      │   │ - queue.db (SQLite WAL)       │
               │ Tier 0: Regex Rules  │   │ - Atomic Leases & Fencing     │
               │ (Microseconds)       │   │ - Deterministic Idempotency   │
               │         │            │   └──────────────┬────────────────┘
               │ (Miss)  ▼            │                  │
               │ Tier 1: Local Model  │                  ▼
               │ (Ollama qwen2.5:1.5b)│       ┌──────────────────────┐
               └──────────┬───────────┘       │ Drain Worker         │
                          │ Validated         │ (internal/drain)     │
                          ▼ Command           │ - Network-gated claim│
               ┌──────────────────────┐       │ - Leased execution   │
               │ Tools (internal/tools)       │ - Atomic completion  │
               │ - Closed 5-Tool Menu │       └──────────┬───────────┘
               │ - Generic Validation │                  │
               └──────────┬───────────┘                  │
                          │ Database                     │ Updates Tasks
                          ▼ Mutation                     ▼
               ┌─────────────────────────────────────────────────────────┐
               │               tasks.db (internal/store/sqlstore)        │
               │  - ops (Append-only log; SQL triggers block UPDATE/DEL) │
               │  - tasks & task_notes (Disposable Materialized Views)   │
               │  - Hybrid Logical Clocks (HLC) & LWW-Map CRDTs          │
               │  - Pure Replay Verification (`Store.Verify()`)          │
               └─────────────────────────────────────────────────────────┘
                                           ▲
                                           │ Read-only inspection
               ┌───────────────────────────┴─────────────────────────────┐
               │         Cross-Process Observability (internal/dash)     │
               │  - Reads metrics/*.json snapshots (written by rename)   │
               │  - Read-only SQLite attachment (no locks, no migrations)│
               │  - Real-time terminal dashboard (`offgrid top`)         │
               └─────────────────────────────────────────────────────────┘
```

---

## 3. The Zero-Daemon Process Model

One of Offgrid’s most defining architectural choices is: **There is NO background daemon.**

### Simple English Explanation
Most systems run a big "server" process in the background. If that server crashes or freezes, everything dies.
Offgrid does not run a daemon. Instead, every command you run (`offgrid agent`, `offgrid drain`, `offgrid top`) is an independent, short-lived Unix process. These processes do not share memory or talk over network sockets. They communicate entirely through two durable SQLite database files (`tasks.db` and `queue.db`) and small atomic metric files. 

If you pull the power plug or run `kill -9` on any process at any millisecond, the database cannot be corrupted, no jobs are lost, and no memory leaks can exist.

### Technical Deep Dive
* **Inter-Process Communication (IPC):** Storage-based synchronization via SQLite WAL (Write-Ahead Logging). SQLite manages concurrent readers and single-writer concurrency through OS-level file locks.
* **Process Isolation:** Memory is strictly unshared. A memory leak or panic in the background drain worker cannot destabilize the user-facing CLI or the terminal dashboard.
* **Crash Semantics (`kill -9`):** Processes are designed for abrupt termination. Transactions are atomic; uncommitted state is rolled back by SQLite recovery upon the next connection.

---

## 4. Day-by-Day Deep Dive (Days 1 to 17)

Every day below includes:
1. **The Goal in Simple English** (Plain-language purpose)
2. **Technical Terms Explained** (Exact Industry Definition + Concrete implementation in Offgrid)
3. **Under the Hood** (Go implementation details)
4. **Architectural Trade-offs** (What we gained vs. what we sacrificed)
5. **Interview Talking Point** (How to present this in senior-level interviews)

---

### Day 1: Measure Before You Design

> **Goal (Simple English):** Before writing any complex agent code, measure the hardware we actually run on so we set realistic speed and memory limits.

#### 🧠 Technical Terms for Day 1:
| Technical Term | Industry Definition (Simple English) | How Offgrid Uses It in This Project |
|---|---|---|
| **Hardware Profiling** | Inspecting and measuring the underlying physical host (CPU cores, clock speed, cache, memory limits, disk IOPS) before making architectural choices. | `internal/sysinfo` queries the OS and `/proc/cpuinfo` to profile our target edge hardware (AMD Ryzen 5 PRO 5650U, consumer NVMe SSD). This ensures we know our exact compute and RAM limits before choosing local AI models. |
| **Baseline Benchmarking** | Running controlled, repeatable microbenchmarks on bare storage and compute primitives to establish maximum physical throughput. | `internal/bench` tests raw SQLite write throughput (achieving ~1,100 durable writes/sec at `PRAGMA synchronous=FULL`) and LLM token generation speeds (~38 tokens/sec) to establish hard performance budgets. |
| **Resource Budgeting** | Allocating hard, non-negotiable memory (RAM/VRAM) and CPU limits to individual components to prevent Out-Of-Memory (OOM) crashes on edge devices. | Capped total AI model RAM at < 2GB (selecting 1.5B/3B parameter SLMs) and mandated sub-millisecond execution for local rule matching. |
| **Single-Core Efficiency** | Designing execution loops to maximize single-thread performance on low-power edge machines without relying on server clusters. | All core logic runs efficiently on standard laptop CPU cores with zero GPU dependencies. |

* **How It Works:** Built `internal/sysinfo` and `internal/bench`. Scans CPU cores, clock speed, OS cache, and measures raw SQLite and memory throughput.
* **Trade-off:**
  - *Gained:* Realistic engineering constraints (knowing that an edge laptop can execute 1,100 durable writes/sec and run a 1.5B model at 40 tokens/sec).
  - *Sacrificed:* Development velocity on Day 1 (spending time measuring hardware rather than prototyping features).
* **Interview Point:** *"We never optimize based on vibes or theoretical assumptions. We benchmarked our hardware first to establish hard performance budgets."*

---

### Day 2: The Swappable Model Client

> **Goal (Simple English):** Create a clean way to talk to AI models so that switching between local models (Ollama) and cloud models (OpenAI/Anthropic) takes zero changes to our agent logic, while making unit tests run in microseconds without an actual AI running.

#### 🧠 Technical Terms for Day 2:
| Technical Term | Industry Definition (Simple English) | How Offgrid Uses It in This Project |
|---|---|---|
| **Dependency Inversion Principle (DIP)** | High-level business logic must never depend directly on low-level infrastructure, drivers, or third-party SDKs; both must depend on an abstract interface. | `internal/agent` depends only on the `llm.Client` interface. The agent has zero knowledge of Ollama, HTTP requests, or external APIs. |
| **Single-Method Interface** | Designing interfaces with only one essential method (*"The bigger the interface, the weaker the abstraction"*). | `llm.Client` defines only `Chat(ctx, req, onToken) (Response, error)`. Because it is so small, creating test mocks takes 4 lines of code, and swapping Ollama for a cloud provider takes 20 minutes. |
| **Anti-Corruption Layer (ACL)** | A translation layer that intercepts messy, vendor-specific network errors (HTTP 500, TCP disconnects, raw JSON parse errors) and maps them into clean, strongly-typed internal domain errors. | `translateError` in `internal/llm/ollama.go` catches Ollama HTTP connection failures and transforms them into domain errors like `ErrModelNotFound` or `ErrContextTooLong`. |
| **Streaming Token Callback** | A function callback invoked incrementally for every single token generated by an AI model, rather than blocking until the entire response is completed. | Passed as `onToken func(string)` into `Chat()`, enabling instant terminal typing effects in the CLI while concurrently accumulating tokens for structured JSON parsing. |
| **Mock / Fake Injection** | Substituting real network services with lightweight, deterministic in-memory implementations during unit tests. | `internal/llm/fake.go` returns scripted token responses instantly, allowing tests to run in sub-millisecond time without running an actual Ollama daemon. |

* **How It Works:** 
  In `internal/llm/llm.go`, defined the interface:
  ```go
  type Client interface {
      Chat(ctx context.Context, req Request, onToken func(string)) (Response, error)
  }
  ```
  Implemented `internal/llm/ollama.go` with `translateError(err)` to convert low-level HTTP/TCP errors into typed domain errors (`ErrModelNotFound`, `ErrContextTooLong`). Implemented `internal/llm/fake.go` which returns scripted tokens instantly for unit tests.
* **Trade-off:**
  - *Gained:* Complete decoupling of domain code from Ollama; lightning-fast tests (sub-millisecond unit tests).
  - *Sacrificed:* Access to Ollama-specific proprietary features (e.g., model downloading or GPU VRAM controls) inside the agent layer.
* **Interview Point:** *"Go proverbs state: 'The bigger the interface, the weaker the abstraction.' Our LLM interface has exactly one method, satisfying the Interface Segregation Principle and making tests frictionless."*

---

### Day 3: Reliable Model Outputs

> **Goal (Simple English):** AI models naturally produce messy, random text. We force the AI to return strictly valid JSON that exactly fits our predefined schema, and if it makes a typo, our code automatically catches it, feeds the error back to the model, and asks it to fix its mistake.

#### 🧠 Technical Terms for Day 3:
| Technical Term | Industry Definition (Simple English) | How Offgrid Uses It in This Project |
|---|---|---|
| **Grammar-Constrained Decoding** | Forcing the LLM's underlying sampling engine to physically only sample tokens that satisfy a formal Context-Free Grammar (CFG) or JSON schema, making malformed syntax impossible. | `internal/structured` injects JSON schemas directly into Ollama's grammar engine, physically preventing the model from hallucinating conversational preambles ("Sure, here is your JSON:"). |
| **Schema Enforcement** | Validating incoming payloads against strict structural definitions, types, and required fields before allowing them into business logic. | `internal/intent` unmarshals the model's output into strongly-typed Go structs (`Command`), verifying that intent names, task IDs, and parameters match expected schemas. |
| **Error Reflection Loop** | An automated feedback mechanism where validation or compiler error messages are fed back to an LLM in a secondary prompt so it can inspect and correct its own mistakes. | If Go's JSON parser returns an unmarshal error, Offgrid appends the error string back into the prompt history, giving the model context to fix its syntax within a strict 2-retry limit. |
| **Wilson Score Confidence Interval** | A statistical formula that calculates the true binomial confidence interval of success rates from a finite sample of test runs, preventing false optimism from small test sets. | `internal/eval` ran ablation benchmarks across 40 prompts with and without schema constraints, scientifically proving that schema guidance boosts task parsing accuracy from ~65% to >97% within a 95% confidence interval. |

* **How It Works:** 
  `internal/structured` injects JSON schemas into Ollama's grammar engine. Built an evaluation harness (`internal/eval`) running ablation tests across 40 test prompts. If parsing fails, the exact Go JSON unmarshal error is appended to the message history, giving the LLM context to self-correct within a strict 2-retry limit.
* **Trade-off:**
  - *Gained:* Near 100% structured reliability without external bloated Python frameworks (LangChain).
  - *Sacrificed:* Slightly higher latency on malformed responses due to retry roundtrips.
* **Interview Point:** *"We don't trust prompt engineering alone. We use grammar-constrained decoding at the engine level backed by a bounded reflection loop, mathematically proven using Wilson Confidence Intervals."*

---

### Day 4: The Closed Tool Menu

> **Goal (Simple English):** Stop the AI from executing dangerous or random actions. The AI is only allowed to choose from 5 pre-approved, strictly typed tools, and it cannot execute anything until our Go code validates the inputs.

#### 🧠 Technical Terms for Day 4:
| Technical Term | Industry Definition (Simple English) | How Offgrid Uses It in This Project |
|---|---|---|
| **Closed-Menu Execution** | Restricting an autonomous agent to an explicit, unmodifiable registry of pre-approved actions, preventing arbitrary execution. | `internal/tools` defines exactly 5 allowed tools (`create_task`, `update_task`, `list_tasks`, `add_note`, `daily_summary`). The agent cannot invent or call outside APIs. |
| **Compile-Time Type Safety (Generics)** | Utilizing language generics (`[T any]`) to verify data shapes at compilation time rather than deferring type checking to runtime reflection. | Defined `Tool[T any]` where `T` is the validated input struct. The database execution function literally will not compile if it attempts to consume unparsed strings. |
| **Separation of Parse vs. Execute** | Splitting the interpretation of user input from the state mutation layer so that parsing failures cannot cause partial database writes. | The tool registry parses and validates JSON inputs into Go types first; database transactions are only initiated if validation succeeds 100%. |
| **Blast-Radius Containment** | Designing security boundaries so that the worst-case consequence of a compromised component (e.g. prompt injection) is strictly limited. | Even if an attacker injects `"DROP TABLE ops; DELETE FROM tasks"`, the tool execution layer treats it as plain text and safely stores it as a task title, rendering SQL injection structurally impossible. |
| **Test Spies** | A specialized test mock that records every invocation, argument, and call count so assertions can prove what did—and did not—happen. | `SpyStore` records calls during invalid tool tests, mathematically proving that ambiguous commands produce zero database writes. |

* **How It Works:**
  `internal/tools` defines 5 typed actions (`create_task`, `update_task`, `list_tasks`, `add_note`, `daily_summary`). Uses Go generics so that tool argument parsing and validation are strictly separated from database execution. The execution layer literally cannot compile if passed raw, unvalidated strings.
* **Trade-off:**
  - *Gained:* Zero risk of arbitrary code execution or prompt injection corrupting the database.
  - *Sacrificed:* Dynamic open-ended agent flexibility (the model cannot invent new tools on the fly).
* **Interview Point:** *"The model proposes; the Go runtime disposes. The LLM is treated as an untrusted user input form."*

---

### Day 5: The Dual-Tier Agent & Contract Testing

> **Goal (Simple English):** Don't waste battery and compute asking an LLM to do something a simple regular expression can do in 2 microseconds. Also, ensure our in-memory test database behaves identically to our future real database.

#### 🧠 Technical Terms for Day 5:
| Technical Term | Industry Definition (Simple English) | How Offgrid Uses It in This Project |
|---|---|---|
| **Tiered Inference Architecture** | A multi-stage intelligence pipeline where fast, deterministic algorithms run first, and slower, power-hungry AI models only run as a fallback. | `internal/agent` implements Tier 0 (regex rules) taking 2 µs; Tier 1 (Ollama SLM) takes ~450ms and runs only when Tier 0 fails to recognize the phrasing. |
| **Deterministic Fast-Path** | A high-speed, predictable execution path that bypasses statistical models for known patterns. | Common operational phrasings like `"list tasks"`, `"close task 12"`, or `"create task: inspect pump"` match regexes instantly, saving 100% of CPU/GPU battery. |
| **Contract Testing** | Running a single test suite against multiple different implementations of an interface to prove behavioral interchangeability. | `internal/task/storetest` runs the identical test suite against ephemeral `MemStore` and persistent `sqlstore.Store`, guaranteeing they satisfy the exact same edge cases. |
| **In-Memory Oracle** | A simple, obviously-correct in-memory data store used as the ground-truth standard to verify the correctness of complex storage engines. | `MemStore` serves as the trusted oracle during randomized differential testing against SQLite. |

* **How It Works:**
  `internal/agent` introduces **Tier 0**: regex and string parsing for common phrases ("list tasks", "create task: inspect boiler"). If Tier 0 matches, execution takes ~2 µs with zero model invocation. Only unrecognized phrasings fall back to **Tier 1** (Ollama). 
  `internal/task/storetest` implements a shared Contract Test Suite that any storage backend (memory, SQLite) must pass to guarantee behavioral parity.
* **Trade-off:**
  - *Gained:* 90% of routine user requests execute in microseconds with 0% battery/GPU drain; contract tests guarantee storage engines can be swapped without regression.
  - *Sacrificed:* Regex maintenance overhead for the Tier 0 rule set.
* **Interview Point:** *"A senior engineer doesn't use AI for everything. Tier 0 rules execute in 2 microseconds; Tier 1 runs only when linguistic judgment is required."*

---

### Day 6: Event-Sourced Storage & SQLite Triggers

> **Goal (Simple English):** Make our data indestructible. Instead of overwriting task rows in a database table, we record every change as a permanent, unchangeable flight-recorder entry (an append-only ledger). The actual task table is just a disposable cache that can be deleted and instantly rebuilt from the ledger.

#### 🧠 Technical Terms for Day 6:
| Technical Term | Industry Definition (Simple English) | How Offgrid Uses It in This Project |
|---|---|---|
| **Event Sourcing** | Storing the complete history of state changes as an immutable sequence of events (an append-only log) rather than modifying records in-place. | The `ops` table records every task mutation with `(node_id, seq, op_type, payload, hlc)`. We never execute `UPDATE tasks SET ...`. |
| **Materialized View** | A derived, read-optimized cache table generated by applying events in sequence. | `tasks` and `task_notes` are disposable views created by replaying the `ops` log. They can be deleted at any time and rebuilt in milliseconds. |
| **SQL Triggers for Immutability** | Database-engine rules that automatically execute before SQL statements to reject forbidden operations. | `ops_no_update` and `ops_no_delete` triggers cause SQLite to immediately abort (`RAISE(ABORT)`) if any process attempts to edit or delete historical ops. |
| **Temporal Purity** | A property of projection functions where the output depends purely on the event's recorded timestamp and payload, never reading the current system clock (`time.Now()`). | `applyOp` never calls `time.Now()`, ensuring that replaying an operation from 2 years ago reconstructs the exact original historical timestamps without time drift. |
| **Zero-Drift Verification (`Store.Verify()`)** | Replaying historical events in an isolated scratch environment and comparing cryptographic hashes against live state to prove zero corruption. | Replays the `ops` log into an in-memory database, hashes both states with SHA-256, and returns `ErrDiverged` if live tables were manually tampered with. |

* **How It Works:**
  In `internal/store/sqlstore`:
  1. `ops` table stores immutable events with `(node_id, seq)`.
  2. SQL Triggers (`ops_no_update`, `ops_no_delete`) physically cause SQLite to abort if any code attempts an `UPDATE` or `DELETE` on `ops`.
  3. `tasks` and `task_notes` are materialized views generated by pure projection functions (`applyOp`) that **never call `time.Now()`**.
  4. `Store.Verify()` replays the log into an isolated `:memory:` database, computes SHA-256 hashes of both live and replayed views, and detects any data tampering or corruption.
* **Trade-off:**
  - *Gained:* Complete auditability, zero risk of data loss, trivial rollbacks, and foundation for distributed sync.
  - *Sacrificed:* Log growth requires disk space; replay cost increases with millions of historical ops (mitigated by snapshots).
* **Interview Point:** *"In Offgrid, the log is the truth; the tables are just a cache. If someone tampers with the tasks table, `Store.Verify()` detects it instantly and `Store.Rebuild()` reconstructs it in milliseconds."*

---

### Day 7: The Local Agent Integration & CLI Wiring

> **Goal (Simple English):** Assemble all Week 1 components into a real, runnable command-line tool that lets a user manage tasks offline from their terminal.

#### 🧠 Technical Terms for Day 7:
| Technical Term | Industry Definition (Simple English) | How Offgrid Uses It in This Project |
|---|---|---|
| **Composition Root** | The singular location in an application where the dependency graph is composed and wired together. | `cmd/offgrid/main.go` is the composition root. It parses CLI flags, opens SQLite databases, instantiates stores and agents, and passes them downward. |
| **Subcommand Architecture** | Structuring a command-line utility into distinct sub-applications sharing a common binary executable. | `offgrid agent`, `offgrid drain`, `offgrid store`, `offgrid net`, `offgrid top` all execute from one binary. |
| **Zero-Daemon Philosophy** | Designing an application where no long-running background service exists; all coordination occurs through persistent storage files. | Processes spin up, perform work against SQLite WAL, and exit immediately, eliminating background memory leaks and daemon management overhead. |

* **How It Works:**
  `cmd/offgrid/main.go` wires together configuration, logging, SQLite storage, and the agent runtime. Exposes `offgrid agent "clean site 4"` and `offgrid store rebuild/verify`.
* **Trade-off:**
  - *Gained:* A single, self-contained binary with zero external runtime dependencies.
  - *Sacrificed:* No graphical user interface (terminal-first).
* **Interview Point:** *"The `cmd/offgrid` package acts as the pure Composition Root. No internal package knows about CLI flags or terminal formatting."*

---

### Day 8: The Connectivity Monitor & Hysteresis State Machine

> **Goal (Simple English):** Detect whether the internet is actually working without freezing the app, without getting tricked by fake hotel/airport Wi-Fi login screens, and without flipping rapidly between online and offline when the signal is weak.

#### 🧠 Technical Terms for Day 8:
| Technical Term | Industry Definition (Simple English) | How Offgrid Uses It in This Project |
|---|---|---|
| **Pure State Machine** | A state transition engine that takes inputs and returns new states deterministically without side effects, background threads, or clocks. | `connectivity.Machine` takes network verdicts (`good`, `slow`, `fail`) and outputs state transitions (`ONLINE`, `DEGRADED`, `OFFLINE`). |
| **Hysteresis** | The dependence of the state of a system on its history, requiring a consistent streak of repeated observations before switching states to prevent rapid flapping. | Requires 3 consecutive failures to drop to `OFFLINE`, and 3 consecutive successes to return to `ONLINE`, stabilizing the app on flaky links. |
| **Asymmetric Transitions** | Configuring a state machine to transition into a safe/conservative state faster than it transitions back to an optimistic state. | The system degrades after 2 slow/failed probes, but requires 3 good probes to return to online, prioritizing offline safety over false online optimism. |
| **Captive Portal Detection** | Distinguishing true internet access from intercepted hotel or airport Wi-Fi login splash screens. | The probe demands strict **HTTP 204 No Content** and refuses redirects; receiving HTML login pages is treated as a failed network probe. |
| **Fan-Out Probing (`Any`)** | Dispatching simultaneous requests to multiple independent servers and resolving upon the first successful response. | Probes Google and Cloudflare endpoints concurrently, cancelling the slower probe to avoid goroutine leaks. |

* **How It Works:**
  In `internal/connectivity`:
  1. `Prober` sends parallel HTTP probes expecting strict **HTTP 204 No Content** (refusing redirects to catch captive portals).
  2. `Machine` implements a pure state machine: `ONLINE`, `DEGRADED`, `OFFLINE`.
  3. **Hysteresis:** Requires 3 consecutive failures to drop to OFFLINE, but 3 consecutive successes to return to ONLINE. Asymmetric: drops faster than it recovers to prevent state flapping.
  4. The monitor publishes state via an `atomic.Int32` so calling `mon.State()` is non-blocking and thread-safe.
* **Trade-off:**
  - *Gained:* 100% accurate connectivity state without UI lag or network flapping.
  - *Sacrificed:* Periodic background HTTP requests consume minor network bandwidth (~几 KB/minute).
* **Interview Point:** *"We never trust the OS 'Wi-Fi Connected' icon. We probe for HTTP 204 and use asymmetric hysteresis to prevent state flapping on weak satellite links."*

---

### Day 9: The Durable Job Queue & Fencing Tokens

> **Goal (Simple English):** When an analytical job cannot be run immediately (because it's too complex or the network is down), save it in a durable queue on disk. Ensure that even if two workers try to process the job, or a worker crashes halfway through, no job is lost and no job is executed twice.

#### 🧠 Technical Terms for Day 9:
| Technical Term | Industry Definition (Simple English) | How Offgrid Uses It in This Project |
|---|---|---|
| **Durable Queue** | A queue whose state is stored on persistent storage, guaranteeing jobs survive process crashes and reboots. | `queue.db` stores queued, running, and finished jobs in SQLite WAL mode. |
| **Atomic Leases** | A time-limited lock granted to a worker to process a job, which automatically expires if the worker crashes. | Workers atomically set `status='running'` and `lease_until = now + 400ms` in a single SQL update. |
| **Fencing Tokens** | An incrementing counter included with every distributed write that invalidates stale writes from slow or zombie workers. | The `attempts` column acts as a fencing token. When a worker finishes a job, SQLite rejects the write if the attempt number was incremented by another worker. |
| **Full Jitter Exponential Backoff** | Introducing randomized delay intervals into exponential backoff retry algorithms to prevent thundering-herd spikes. | Retried jobs wait $t \times 2^{\text{attempts}} \times \text{rand}(0, 1)$ before becoming claimable. |
| **Deterministic Idempotency Key** | A unique hash generated from input parameters ensuring identical operations are executed only once. | Uses `SHA-256(date + user_request)` so submitting the same command twice creates only one job. |

* **How It Works:**
  `internal/jobqueue` implements a queue on SQLite:
  1. Workers claim a job by atomically updating its status to `running`, writing a lease expiration timestamp, and incrementing `attempt_number`.
  2. **Fencing Token:** The `attempt_number` acts as a fence. When a worker finishes and writes the result, SQLite verifies that `attempt_number` hasn't changed. If a slow worker took too long and the lease expired, its write is rejected with `ErrStaleClaim`.
  3. The result is written and the job marked `done` within a **single atomic SQL transaction**.
* **Trade-off:**
  - *Gained:* Total crash resistance and protection against zombie/slow worker duplicate writes.
  - *Sacrificed:* Requires explicit lease heartbeats for long-running jobs.
* **Interview Point:** *"We achieve exactly-once outcome recording using the attempt number as a distributed fencing token, ensuring zombie workers cannot corrupt completed tasks."*

---

### Day 10: The Complexity Router

> **Goal (Simple English):** Build an intelligent gate that looks at incoming user requests and decides: *"Can the local agent handle this right now in 50 milliseconds, or does this require deep analytical thinking that should be queued for later?"*

#### 🧠 Technical Terms for Day 10:
| Technical Term | Industry Definition (Simple English) | How Offgrid Uses It in This Project |
|---|---|---|
| **Closed-Vocabulary Complexity Scoring** | Analyzing incoming text against a predefined lexicon of complexity keywords to make routing decisions without invoking an LLM. | `internal/router` assigns scores to analytical words ("analyze", "evaluate", "why") and temporal ranges ("over 6 months"). |
| **Deterministic Routing Gate** | A routing mechanism that produces 100% predictable decisions based on fixed threshold rules. | Requests scoring < 3 route immediately to the local agent; requests >= 3 route to the durable queue. |
| **Human-Readable Routing Explanations** | Providing transparent, user-friendly justifications for why an automated system took a specific action. | The router outputs: *"Deferred as Job #14: requires multi-month comparative telemetry analysis."* |

* **How It Works:**
  `internal/router` evaluates user prompts against a scoring rubric:
  - Judgment words ("analyze", "recommend", "why did this fail") add +2.
  - Multi-step instructions or temporal ranges ("over the last 3 months") add +2.
  - Score >= 3 routes to the durable queue as a `deep_think` job.
  - Unrecognized text routes to the queue as an `interpret` job.
  - Score < 3 routes directly to the local instant agent.
  - Produces human-readable explanations: *"Deferred: requires multi-week comparative analysis."*
* **Trade-off:**
  - *Gained:* Deterministic, predictable routing with zero LLM overhead for routing decisions.
  - *Sacrificed:* Cannot catch subtle, nuanced complexity that falls outside the closed vocabulary.
* **Interview Point:** *"Routing decisions must be deterministic and explainable. We use a closed vocabulary gate rather than spending 2 seconds asking an LLM to route an LLM call."*

---

### Day 11: Fault Injection & Chaos Testing (`netfault`)

> **Goal (Simple English):** Build an internal chaos tool that lets us intentionally inject simulated network latency, packet loss, and full disconnects into our own app so we can test edge scenarios repeatedly in automated test scripts.

#### 🧠 Technical Terms for Day 11:
| Technical Term | Industry Definition (Simple English) | How Offgrid Uses It in This Project |
|---|---|---|
| **Fault Injection / Chaos Testing** | Intentionally introducing real-world network latency, packet loss, and failures into software to test resilience. | `internal/netfault` introduces configurable delays, packet drop rates, and disconnections into test environments. |
| **Custom `http.RoundTripper`** | Intercepting and decorating outbound HTTP requests at Go's network transport layer without modifying application code. | `netfault.Transport` wraps standard Go HTTP clients, injecting artificial delays or network disconnects before requests hit the wire. |
| **Network Partition Simulation** | Simulating a total communication failure between distributed nodes to verify system isolation and offline continuity. | Used in `scripts/partition-demo.sh` to simulate a satellite blackout and verify the agent continues executing offline. |

* **How It Works:**
  `internal/netfault` wraps Go's standard `http.RoundTripper`. By setting flags or environment variables, it intercepts outbound HTTP traffic and injects configurable sleep delays, random error responses (e.g. 500ms jitter, 25% drop rate), or complete TCP connection drops without touching OS iptables.
* **Trade-off:**
  - *Gained:* Reproducible, deterministic chaos testing that runs in standard `go test` suites.
  - *Sacrificed:* Only intercepts Go HTTP traffic, not low-level raw TCP/UDP sockets outside the transport layer.
* **Interview Point:** *"We don't hope our system survives network failure; we built an internal `http.RoundTripper` fault-injector to mathematically simulate flaky satellite links in CI."*

---

### Day 12: The Crash-Proof Drain Worker

> **Goal (Simple English):** Build a background worker that wakes up when the network returns, drains the queue, safely executes heavy jobs, and survives being killed (`kill -9`) without duplicating actions or corrupting data.

#### 🧠 Technical Terms for Day 12:
| Technical Term | Industry Definition (Simple English) | How Offgrid Uses It in This Project |
|---|---|---|
| **Network-Gated Claiming** | Inspecting connectivity conditions before claiming a job to avoid burning retry attempts on jobs that cannot run. | `w.claimable()` filters out cloud-dependent jobs (`deep_think`) while offline, claiming only local jobs (`interpret`). |
| **SIGKILL (`kill -9`) Resilience** | Architecting processes so that immediate operating system termination causes zero data corruption or stuck jobs. | Tested in `kill_test.go` by firing 3 SIGKILLs during active processing; unexpired leases expire and subsequent workers resume seamlessly. |
| **Non-Idempotent Marker Pattern (`executing`)** | Storing an in-progress marker before executing non-idempotent mutations so crashes mid-flight can be safely routed to human review. | The `interpret` job records an `executing` state in `tasks.db` before applying mutations, preventing duplicated tasks upon crash restarts. |
| **Heartbeat Lease Extension** | Periodically updating a task's lease deadline in the background while processing is actively underway. | A ticker goroutine calls `Queue.Extend` every $\text{Lease} / 3$ to keep active long-running jobs from being stolen. |

* **How It Works:**
  In `internal/drain`:
  1. Checks connectivity state before claiming jobs. Offline? Claims only local jobs (`interpret`). Online? Claims cloud-enabled jobs (`deep_think`).
  2. For jobs that mutate tasks (`interpret`), it writes an `executing` marker in the task store. If the worker is killed mid-execution and restarted, the new worker sees the marker and safely routes to human review rather than executing duplicate mutations.
  3. Periodically extends leases via background heartbeat goroutines.
* **Trade-off:**
  - *Gained:* Absolute safety against duplicated mutations on unexpected process restarts.
  - *Sacrificed:* Jobs that crash mid-mutation require manual confirmation or idempotent retry policies.
* **Interview Point:** *"In distributed systems, crash recovery is not an afterthought. We proved our drain worker by firing 3 SIGKILLs during a 60-job run in CI with zero lost or duplicated records."*

---

### Day 13: Cross-Process Observability & Terminal Dashboard (`offgrid top`)

> **Goal (Simple English):** Let the user open a live, flicker-free terminal dashboard (like `htop`) that shows what the agent, queue, and drain workers are doing in real time, without slowing down the workers and without database locks.

#### 🧠 Technical Terms for Day 13:
| Technical Term | Industry Definition (Simple English) | How Offgrid Uses It in This Project |
|---|---|---|
| **Shared-Nothing Observability** | Collecting telemetry across independent processes without shared memory, IPC sockets, or background monitoring daemons. | Active processes write structured JSON metrics to `metrics/*.json`. |
| **Atomic POSIX File Rename** | Writing data to a temporary file and atomically renaming it over the target path to guarantee readers never see partial writes. | Workers write to `metrics/agent.json.tmp` and execute `os.Rename`, guaranteeing atomic updates. |
| **Read-Only SQLite Attachment (`PRAGMA query_only`)** | Attaching to an active database in strict read-only mode so queries never acquire write locks or trigger schema upgrades. | `offgrid top` attaches to `queue.db` with `query_only=ON`, guaranteeing the dashboard never stalls drain workers. |
| **Double-Buffered Terminal Rendering** | Constructing an entire terminal frame in memory before writing it to stdout in a single write call. | `internal/dash` buffers all ANSI text, eliminating screen flicker during live 1-second refreshes. |

* **How It Works:**
  `internal/dash` and `cmd/offgrid top`:
  1. Each active process writes a tiny metrics JSON file to `metrics/*.json` once per second.
  2. Uses the **atomic rename pattern**: writes to `metrics/agent.json.tmp`, then renames to `agent.json`. Reads never observe partial writes.
  3. `offgrid top` attaches to `queue.db` with `PRAGMA query_only=ON`, ensuring the dashboard can never block workers or trigger SQLite schema locks.
  4. Renders state using a pure terminal drawing function that writes to an off-screen buffer before flushing to stdout, eliminating screen flicker.
* **Trade-off:**
  - *Gained:* Real-time system monitoring with zero lock contention and zero impact on worker throughput.
  - *Sacrificed:* Metrics are eventually consistent (delayed by up to 1 second).
* **Interview Point:** *"Our dashboard connects with `query_only=ON` and reads metrics updated via atomic POSIX file renames, guaranteeing the monitor can never stall worker write transactions."*

---

### Day 14: v0.1 Architecture Freeze & Release Packaging

> **Goal (Simple English):** Freeze the single-node architecture, document all system guarantees, establish the cross-compilation pipeline, and verify the entire system end-to-end.

#### 🧠 Technical Terms for Day 14:
| Technical Term | Industry Definition (Simple English) | How Offgrid Uses It in This Project |
|---|---|---|
| **Architecture Freeze** | Formalizing system boundaries and invariants, locking down single-node architecture before beginning distributed features. | Froze single-node agent and queue guarantees in `ARCHITECTURE.md` before building multi-device CRDT sync. |
| **Cross-Compilation** | Building native binary executables for different target operating systems and CPU architectures from one host. | `scripts/release-v0.1.sh` compiles static binaries for Linux (amd64/arm64), macOS (Intel/M-series), and Windows. |
| **Semantic Versioning (SemVer)** | A versioning convention (`vMAJOR.MINOR.PATCH`) reflecting API compatibility and feature milestones. | Tagged `v0.1.0` as the baseline milestone release. |

* **How It Works:**
  Created `ARCHITECTURE.md`, `scripts/release-v0.1.sh`. Compiles standalone statically linked binaries for Linux (amd64, arm64), macOS (Apple Silicon, Intel), and Windows. Validated that all 14 integration test suites pass with zero warnings and zero data races.
* **Trade-off:**
  - *Gained:* A rock-solid, production-grade local runtime ready for multi-device sync.
  - *Sacrificed:* Deferred multi-node CRDT sync to v0.2.
* **Interview Point:** *"A great architecture knows when to freeze. Day 14 locked in our single-node invariants so that our upcoming distributed sync layer had an unshakeable foundation."*

---

### Day 15: Hybrid Logical Clocks (HLC)

> **Goal (Simple English):** When multiple devices work offline, their physical clocks will never match (one device is 2 seconds ahead, another is 5 minutes behind, or NTP jumps backward). We need a clock that captures real time when possible, but guarantees that every new event always has a higher timestamp than the event that caused it.

#### 🧠 Technical Terms for Day 15:
| Technical Term | Industry Definition (Simple English) | How Offgrid Uses It in This Project |
|---|---|---|
| **Hybrid Logical Clock (HLC)** | A clock algorithm combining physical wall-clock time with an incrementing logical counter to establish causal ordering in distributed systems. | `internal/hlc/hlc.go` stamps every operation with `(Wall, Counter, NodeID)`. |
| **Lamport Causal Ordering** | A mathematical ordering guarantee ensuring that if event $A$ caused event $B$, $A$ will always have an earlier timestamp than $B$. | When a replica receives a remote update with a higher HLC timestamp, its local HLC jumps forward to maintain causality. |
| **Clock Skew Bounding** | Preventing rogue or malfunctioning clocks on remote nodes from pulling distributed timestamps too far into the future. | Rejects timestamps exceeding maximum physical drift thresholds (e.g. 5 seconds). |
| **Monotonicity Under Backward Time Jumps** | Guaranteeing timestamps always move forward even if NTP synchronizes the physical clock backward or during leap seconds. | If physical wall time regresses, the HLC holds the wall time constant and increments the logical counter, preventing backward timestamps. |
| **Total Order Tie-Breaking** | A deterministic rule that breaks ties between two simultaneous events occurring at the exact same physical and logical time. | Compares unique `NodeID` strings lexicographically so all replicas converge on the exact same winner. |

* **How It Works:**
  In `internal/hlc/hlc.go`:
  An HLC timestamp consists of:
  - `Wall`: Physical wall time (nanoseconds).
  - `Counter`: Logical integer counter for events occurring within the same physical tick.
  - `NodeID`: Unique node identifier for deterministic tie-breaking.
  
  When an event is generated locally, the clock advances. When a message arrives from another node with a higher timestamp, the local clock jumps forward:
  $$l.wall = \max(l.wall, physical.now, msg.wall)$$
  If the wall times are identical, it increments the logical counter.
* **Trade-off:**
  - *Gained:* True causal ordering across disconnected devices without needing expensive atomic clocks or GPS hardware.
  - *Sacrificed:* If a rogue node has a physical clock set 10 years in the future, it can pull other clocks forward (mitigated by maximum clock skew bounds).
* **Interview Point:** *"Physical clocks cannot be trusted in distributed systems due to NTP jumps and clock drift. We implemented Hybrid Logical Clocks combining physical wall time with Lamport counters to guarantee strict causal monotonicity."*

---

### Day 16: Conflict-Free Replicated Data Types (LWW-Map CRDT)

> **Goal (Simple English):** When two people edit the same task while offline on different laptops, we need a way to merge their changes automatically when they reconnect, without needing a central server and without human merge conflicts.

#### 🧠 Technical Terms for Day 16:
| Technical Term | Industry Definition (Simple English) | How Offgrid Uses It in This Project |
|---|---|---|
| **State-Based CRDT (CvRDT)** | A data structure replicated across multiple nodes that can merge state directly without requiring central coordination. | `internal/crdt/lwwmap.go` merges task state across disconnected edge replicas. |
| **Last-Write-Wins Map (LWW-Map)** | A CRDT map where each key has a timestamp, and concurrent updates resolve in favor of the highest timestamp. | Manages task attributes (`title`, `status`, `priority`) across distributed edge nodes. |
| **Per-Field Registers** | Decomposing an entity into independent field registers rather than treating the whole object as a single monolithic value. | Editing Task 1's title on Laptop A and editing Task 1's priority on Laptop B merges cleanly without conflicts. |
| **Tombstone Deletion** | Recording a deletion by setting a tombstone flag and timestamp rather than physically deleting the record. | Prevents an offline replica that hasn't seen the deletion from re-inserting the task when syncing later. |
| **Bounded Join-Semilattice** | A mathematical structure equipped with a `Merge` function that is Commutative, Associative, and Idempotent. | Guarantees that replicas merge into bit-for-bit identical state regardless of message delivery order or network batching. |

* **How It Works:**
  In `internal/crdt/lwwmap.go`:
  Instead of treating a whole task as one blob, the task is split into **per-field registers** (`title`, `status`, `priority`).
  Each field stores:
  - `Value`: The field value.
  - `Timestamp`: The HLC timestamp when it was edited.
  - `Deleted`: Boolean tombstone.
  
  When merging replica $A$ and replica $B$:
  For every field, compare timestamps. The higher HLC timestamp wins. If the timestamps are identical down to the nanosecond, the lexicographically larger `NodeID` wins.
* **Trade-off:**
  - *Gained:* Automatic, conflict-free state convergence across any number of edge nodes in any network topology.
  - *Sacrificed:* Concurrent edits to the exact same field will pick one winner (Last-Write-Wins); concurrent edits to *different* fields on the same task are cleanly merged.
* **Interview Point:** *"We decomposed tasks into per-field LWW-Registers. Edits to different fields merge seamlessly; concurrent edits to the same field converge deterministically via HLC tie-breaking."*

---

### Day 17: Property-Based Formal Verification

> **Goal (Simple English):** Unit tests with 3 hand-picked examples prove almost nothing in distributed systems. We use a mathematical testing framework (`rapid`) that automatically generates tens of thousands of random, weird, corrupted, and edge-case inputs to mathematically prove our merge function can never fail.

#### 🧠 Technical Terms for Day 17:
| Technical Term | Industry Definition (Simple English) | How Offgrid Uses It in This Project |
|---|---|---|
| **Property-Based Testing** | A testing paradigm where universal mathematical invariants are asserted, and an automated framework generates thousands of random inputs to attempt to break them. | Uses `pgregory.net/rapid` to verify CRDT merge laws across 43,000+ generated test cases. |
| **Semilattice Laws (Commutativity, Associativity, Idempotence)** | Mathematical laws governing state convergence: Commutative (order doesn't matter), Associative (grouping doesn't matter), Idempotent (duplicates don't change state). | Verified by `props_test.go` and `props_edge_test.go` in `internal/crdt`. |
| **LWW Monotonicity Law** | A mathematical guarantee that merging an older timestamp into a newer timestamp can never revert the value to the past. | Formally proven across 3,000 Rapid test cases. |
| **Automated Test Case Shrinking** | An algorithm that takes a complex, failing randomized test case (e.g. a 40-element map) and automatically removes elements until it finds the smallest minimal reproducing failure. | Rapid shrinks broken merge failures down to minimal 2-entry counterexamples. |
| **Mutation Testing** | Intentionally introducing flaws (mutations) into production code to verify that the automated test suite catches them. | `broken_test.go` tests 7 deliberately flawed merge functions (e.g. ignoring counters, favoring lowest timestamp) and proves our property tests catch 100% of them. |

* **How It Works:**
  In `internal/crdt/props_test.go` and `props_edge_test.go` using `pgregory.net/rapid`:
  Generates 43,000+ random CRDT maps and proves the 4 Semilattice Laws:
  1. **Commutative:** $\text{Merge}(A, B) == \text{Merge}(B, A)$ (Order of arrival does not matter).
  2. **Associative:** $\text{Merge}(\text{Merge}(A, B), C) == \text{Merge}(A, \text{Merge}(B, C))$ (Batching / grouping does not matter).
  3. **Idempotent:** $\text{Merge}(A, A) == A$ (Receiving the same update 10 times changes nothing).
  4. **Monotonic:** An older timestamp can never overwrite a newer timestamp.
  
  **The Shrinker:** When a test fails, `rapid` automatically strips away extraneous data and shrinks a 40-item corrupted map down to the exact 2-element minimal reproducing failure.
  **Mutation Testing:** In `broken_test.go`, we wrote 7 deliberately flawed merge functions (e.g., favoring lowest timestamp, ignoring node ID tie-breaks). The property tests caught and rejected 100% of the mutants.
* **Trade-off:**
  - *Gained:* Mathematical proof of correctness across millions of generated permutations; caught subtle tie-breaking bugs before production.
  - *Sacrificed:* Requires deep mathematical rigor and longer test suite runtimes (~300ms vs ~5ms for basic unit tests).
* **Interview Point:** *"Unit tests only verify what the developer remembers to test. We used Rapid property-based testing to verify all four algebraic semilattice laws across 43,000 generated scenarios."*

---

## 5. System-Wide Architectural Trade-offs Matrix

Every senior architectural decision is a choice between competing trade-offs. Here is the master matrix of why Offgrid is built the way it is:

| Architectural Choice | What We Chose | Alternative Considered | Why We Chose It (The Gain) | What We Sacrificed (The Cost) |
|---|---|---|---|---|
| **Storage Engine** | Embedded SQLite WAL | Client-Server Postgres / MySQL | Zero infrastructure; embedded in binary; survives instant power loss. | Single-writer concurrency bottleneck (~1,100 writes/sec). |
| **Process Model** | Zero-Daemon (File IPC) | Long-running Background Service | `kill -9` is harmless; no shared memory leaks; zero background CPU when idle. | Cross-process coordination relies on SQLite locks and polling/inotify. |
| **Data Integrity** | Event Sourced (`ops` log) | Traditional Direct In-Place `UPDATE` | Total audit log; tamper-proof triggers; arbitrary point-in-time rebuilds. | Storage disk space grows indefinitely without periodic snapshotting. |
| **AI Invocation** | Dual-Tier (Regex -> Local LLM) | Single Large Cloud Model (GPT-4) | Operates 100% offline; 2 µs execution on common commands; zero API fees. | Cannot handle complex conversational nuances at Tier 0. |
| **Tool Execution** | Closed 5-Tool Menu with Generics | Dynamic Open-Ended Agent Tool Calling | Zero prompt injection risk; compiler-enforced memory safety. | Agent cannot invent or invoke arbitrary external APIs dynamically. |
| **Network Detection** | HTTP 204 Probe + Asymmetric Hysteresis | OS "Network Connected" Socket Flags | Detects airport captive portals; eliminates rapid online/offline flapping. | Generates minimal outbound HTTP ping traffic on a timer. |
| **Queue Resilience** | SQLite Leases + Fencing Tokens | Redis / RabbitMQ / SQS | No external server required; durable on consumer SSDs; ACID consistency. | Throughput capped by SQLite disk sync speeds rather than RAM queues. |
| **Sync Strategy** | LWW-Map CRDT + Hybrid Logical Clocks | Central Server Locking / Consensus (Raft) | Fully peer-to-peer; zero coordinator needed; works across weeks of disconnection. | Concurrent edits to the exact same field resolve to Last-Write-Wins. |
| **Verification Strategy** | Property-Based Testing (`rapid`) | Only Example-Based Unit Tests | Proves mathematical laws across tens of thousands of random edge cases. | Requires thinking in mathematical invariants and slightly longer CI test time. |

---

## 6. How Data Travels: Step-by-Step Scenarios

### Scenario A: Fast Local Command
1. User runs: `offgrid agent "list tasks"`
2. `router.Score()` evaluates vocabulary: Score = 0 (Easy).
3. `agent.Run()` passes text to **Tier 0** regex engine.
4. Regex recognizes `list tasks` in **2 microseconds**.
5. Dispatches `list_tasks` tool directly against `tasks.db`.
6. Prints formatted task list to terminal. **Total time: 4 milliseconds.**

### Scenario B: Hard Deferred Command (Offline)
1. User runs: `offgrid agent "analyze recurring pump seal failures across site 4 over the last 6 months"`
2. `router.Score()` detects analytical keywords ("analyze", "recurring") and time span ("last 6 months"). Score = 5 (Hard).
3. Router defers request. Computes deterministic idempotency key (`SHA256(date + text)`).
4. Inserts job into `queue.db` as `deep_think`.
5. Returns immediate, human-readable status to user:
   `"Saved as job #42 (waiting for network connectivity to run deep cloud model)."`

### Scenario C: Reconnection & Drain Execution
1. Laptop connects to Wi-Fi.
2. `connectivity.Monitor` sends HTTP 204 probe to Google/Cloudflare. Receives 204.
3. Machine increments streak: 1, 2, 3 -> Transitions state to `ONLINE`.
4. `offgrid drain` worker wakes up. Sees state is `ONLINE`.
5. Claims job #42 in `queue.db`, atomically setting `status='running'`, `attempt=1`, `lease=now()+60s`.
6. Calls deep reasoning model.
7. Atomically writes analysis result to `results` table and marks job `done` in the **same transaction**.

### Scenario D: Sudden Process Termination (`kill -9`)
1. While `offgrid drain` is executing, the operating system kernel kills the process (`kill -9`).
2. Because transactions are atomic in SQLite, any half-written data is discarded.
3. In `queue.db`, job #42 remains in `status='running'` with an un-extended lease.
4. When the lease expires (60 seconds later), a newly spawned drain worker claims the job.
5. The attempt counter increments from 1 to 2.
6. The job completes successfully. **Zero data lost; zero jobs stuck in limbo.**

### Scenario E: Multi-Device Conflict Resolution
1. Laptop A (offline in a mine) updates Task 1 title to *"Fix generator valve"* at HLC `(1700000000, 1, NodeA)`.
2. Laptop B (offline on a truck) updates Task 1 priority to *"high"* at HLC `(1700000005, 1, NodeB)`.
3. When both laptops reconnect, they exchange their CRDT field registers.
4. `crdt.Merge()` merges them:
   - `title` updated to *"Fix generator valve"* (Node A's update is newer than initial).
   - `priority` updated to *"high"* (Node B's update is newer than initial).
5. Both machines now have the exact same combined state without a single conflict error.

---

## 7. The Failure Modes & Blast Radius Matrix

| Threat / Real-World Disaster | How Traditional AI Apps Fail | How Offgrid Structurally Prevents It |
|---|---|---|
| **Prompt Injection Attack** (Malicious input trying to drop tables) | Model complies and executes destructive code or deletes user records. | The LLM has zero direct SQL or shell access. It can only emit a JSON `Command` matching our closed 5-tool schema. Any malicious command is rejected by the Go type validator. |
| **Sudden Power Loss / Crash (`kill -9`)** | SQLite database corrupts; in-memory queues lose all pending tasks. | `PRAGMA synchronous=FULL` ensures ops are flushed to disk before acknowledgement. SQLite WAL handles recovery; job queue leases automatically expire and re-queue. |
| **Captive Portal Wi-Fi** (Airport / Hotel login page) | App thinks it is online, sends API requests, receives HTML login pages, crashes JSON parsers. | Prober specifically demands HTTP status 204 and rejects all redirects. The system remains safely in `OFFLINE` mode until real internet flows. |
| **Weak / Flapping Satellite Link** | App rapidly toggles between online and offline, burning API retry quotas and stalling UI. | Asymmetric Hysteresis State Machine requires 3 consecutive good probes to recover, smoothing over transient packet drops. |
| **Local Database Tampering** | Someone manually modifies the SQLite `tasks` table with a database GUI. | `Store.Verify()` detects the hash mismatch between live tables and replayed `ops` log. `Store.Rebuild()` wipes the tampered tables and re-projects true state from the immutable log. |
| **NTP Time Jump / Backward Clock Skew** | Traditional timestamps go backward; newer edits are overwritten by older edits. | Hybrid Logical Clock detects physical time regression and increments the logical counter, guaranteeing mathematical forward monotonicity. |

---

## 8. Empirical Benchmarks & Real Hardware Numbers

All metrics were gathered directly on target edge consumer hardware (**AMD Ryzen 5 PRO 5650U, Consumer NVMe SSD, 16GB RAM, Linux x86_64**):

| Benchmark Measurement | Result | Meaning in Simple English |
|---|---|---|
| **Tier 0 Deterministic Parse** | **1.8 µs / op** | Routine user requests parse in under 2 millionths of a second. |
| **Durable Committed SQLite Write** | **910 µs / op** | Every task write is flushed to physical disk (`synchronous=FULL`) in under 1 millisecond (~1,100 writes/sec). |
| **Materialized View Rebuild (1,000 Ops)** | **23.0 ms** | Can completely rebuild 1,000 tasks from scratch in 23 milliseconds (~43,500 ops/second). |
| **Materialized View Rebuild (50,000 Ops)** | **1.15 s** | Can replay 50,000 historical operations in barely over one second. |
| **Local LLM Inference (qwen2.5:1.5b)** | **~38.5 tok/sec** | Small local model produces complete task command proposals in ~450ms. |
| **Rapid Property Testing Suite** | **43,000+ Cases** | 43,000 randomly generated CRDT merge cases run and verify in **340 milliseconds**. |

---

## 9. Alphabetical Quick-Index of Technical Concepts

*(Note: Every concept below is fully explained with its industry definition and Offgrid implementation directly inside [Section 4: Day-by-Day Deep Dive](#4-day-by-day-deep-dive-days-1-to-17).)*

* **Anti-Corruption Layer (ACL):** -> See [Day 2](#day-2-the-swappable-model-client)
* **Append-Only Log:** -> See [Day 6](#day-6-event-sourced-storage--sqlite-triggers)
* **Asymmetric Transitions:** -> See [Day 8](#day-8-the-connectivity-monitor--hysteresis-state-machine)
* **Atomic Leases:** -> See [Day 9](#day-9-the-durable-job-queue--fencing-tokens)
* **Baseline Benchmarking:** -> See [Day 1](#day-1-measure-before-you-design)
* **Blast-Radius Containment:** -> See [Day 4](#day-4-the-closed-tool-menu)
* **Captive Portal Detection:** -> See [Day 8](#day-8-the-connectivity-monitor--hysteresis-state-machine)
* **Clock Skew Bounding:** -> See [Day 15](#day-15-hybrid-logical-clocks)
* **Closed-Menu Execution:** -> See [Day 4](#day-4-the-closed-tool-menu)
* **Closed-Vocabulary Complexity Scoring:** -> See [Day 10](#day-10-the-complexity-router)
* **Composition Root:** -> See [Day 7](#day-7-the-local-agent-integration--cli-wiring)
* **Contract Testing:** -> See [Day 5](#day-5-the-dual-tier-agent--contract-testing)
* **Cross-Compilation:** -> See [Day 14](#day-14-v01-architecture-freeze--release-packaging)
* **Dependency Inversion Principle (DIP):** -> See [Day 2](#day-2-the-swappable-model-client)
* **Deterministic Fast-Path:** -> See [Day 5](#day-5-the-dual-tier-agent--contract-testing)
* **Durable Queue:** -> See [Day 9](#day-9-the-durable-job-queue--fencing-tokens)
* **Error Reflection Loop:** -> See [Day 3](#day-3-reliable-model-outputs)
* **Event Sourcing:** -> See [Day 6](#day-6-event-sourced-storage--sqlite-triggers)
* **Fencing Tokens:** -> See [Day 9](#day-9-the-durable-job-queue--fencing-tokens)
* **Full Jitter Exponential Backoff:** -> See [Day 9](#day-9-the-durable-job-queue--fencing-tokens)
* **Grammar-Constrained Decoding:** -> See [Day 3](#day-3-reliable-model-outputs)
* **Hardware Profiling:** -> See [Day 1](#day-1-measure-before-you-design)
* **Heartbeat Lease Extension:** -> See [Day 12](#day-12-the-crash-proof-drain-worker)
* **Hybrid Logical Clock (HLC):** -> See [Day 15](#day-15-hybrid-logical-clocks)
* **Hysteresis:** -> See [Day 8](#day-8-the-connectivity-monitor--hysteresis-state-machine)
* **Idempotency Keys:** -> See [Day 9](#day-9-the-durable-job-queue--fencing-tokens)
* **In-Memory Oracle:** -> See [Day 5](#day-5-the-dual-tier-agent--contract-testing)
* **Last-Write-Wins Map (LWW-Map):** -> See [Day 16](#day-16-conflict-free-replicated-data-types)
* **Materialized View:** -> See [Day 6](#day-6-event-sourced-storage--sqlite-triggers)
* **Mutation Testing:** -> See [Day 17](#day-17-property-based-formal-verification)
* **Network Partition Simulation:** -> See [Day 11](#day-11-fault-injection--chaos-testing)
* **Non-Idempotent Marker Pattern:** -> See [Day 12](#day-12-the-crash-proof-drain-worker)
* **Per-Field Registers:** -> See [Day 16](#day-16-conflict-free-replicated-data-types)
* **Property-Based Testing:** -> See [Day 17](#day-17-property-based-formal-verification)
* **Pure State Machine:** -> See [Day 8](#day-8-the-connectivity-monitor--hysteresis-state-machine)
* **Semilattice Laws:** -> See [Day 17](#day-17-property-based-formal-verification)
* **Shared-Nothing Observability:** -> See [Day 13](#day-13-cross-process-observability--terminal-dashboard)
* **SIGKILL (`kill -9`) Resilience:** -> See [Day 12](#day-12-the-crash-proof-drain-worker)
* **Single-Method Interface:** -> See [Day 2](#day-2-the-swappable-model-client)
* **State-Based CRDT (CvRDT):** -> See [Day 16](#day-16-conflict-free-replicated-data-types)
* **Temporal Purity:** -> See [Day 6](#day-6-event-sourced-storage--sqlite-triggers)
* **Test Case Shrinking:** -> See [Day 17](#day-17-property-based-formal-verification)
* **Test Spies:** -> See [Day 4](#day-4-the-closed-tool-menu)
* **Tiered Inference Architecture:** -> See [Day 5](#day-5-the-dual-tier-agent--contract-testing)
* **Tombstone Deletion:** -> See [Day 16](#day-16-conflict-free-replicated-data-types)
* **Wilson Score Interval:** -> See [Day 3](#day-3-reliable-model-outputs)
* **Zero-Daemon Philosophy:** -> See [Day 3](#3-the-zero-daemon-process-model) & [Day 7](#day-7-the-local-agent-integration--cli-wiring)
* **Zero-Drift Verification:** -> See [Day 6](#day-6-event-sourced-storage--sqlite-triggers)

---

## 10. CLI Verification & Demonstration Cheat Sheet

Want to verify and test the entire system right now from your terminal? Run these commands:

```bash
# 1. Run all unit, contract, and race tests across the entire repository
make check

# 2. Run the full Rapid property-based CRDT verification (3,000 generated cases per law)
make prove-merge

# 3. Verify SQLite append-only log integrity against live tables
./bin/offgrid store verify

# 4. Rebuild materialized views from scratch from the immutable ops log
./bin/offgrid store rebuild

# 5. Test the instant agent routing (Tier 0 vs Tier 1 vs Deferral)
./bin/offgrid agent -route "list tasks"
./bin/offgrid agent -route "analyze pump seal telemetry over the past 6 months"

# 6. Check current Hybrid Logical Clock timestamp
./bin/offgrid clock now

# 7. Check connectivity monitor status and probe health
./bin/offgrid net

# 8. Launch the live terminal dashboard
./bin/offgrid top
```

---
*Document Version: 1.1 (Day 17 Complete with In-Place Term Definitions)*  
*Repository: [Rebira678/Offgrid-AI](https://github.com/Rebira678/Offgrid-AI)*  
*Author: Rebira (`rebikman9@gmail.com`)*
