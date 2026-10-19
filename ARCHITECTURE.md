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

Not in the picture because it does not exist yet: the hybrid logical clock (Day 15), the CRDT state (LWW-Map and OR-Set), and sync between two replicas. The `ts` column of the op log is a placeholder until then.

## 2. Process model

There is one binary, `offgrid`, and **no daemon**. What looks like a system is separate processes over two SQLite files:

| Process | Role | Reads / writes |
|---|---|---|
| `offgrid agent -route` | decides per message: answer now, or defer | `tasks.db` (answers), `queue.db` (enqueue), `metrics/agent.json` |
| `offgrid drain` | empties the queue when it can | `queue.db`, `tasks.db`, `metrics/drain.json` |
| `offgrid top` | shows the whole system | `metrics/*.json`, `queue.db` (**read-only**) |
| `offgrid queue`, `store`, `net`, `fault` | inspect and operate | the files above |

They share **no memory**. The files are the interface, which is what makes `kill -9` on any of them a normal event instead of a special case.

