# Offgrid Agent Runtime

**An AI agent runtime that keeps working when the internet doesn't.**

A small local model makes instant decisions, a durable queue holds the hard
thinking until a connection returns, and CRDT sync merges offline and online
work without conflicts. Everything runs on one laptop with no GPU and costs $0.

> Status: **Day 6.** Event-sourced SQLite storage: immutable append-only op log, disposable materialized view, zero-drift verification, and rebuild.

## Quickstart

```bash
# 1. Install Ollama (runs models locally) and pull two small models
curl -fsSL https://ollama.com/install.sh | sh
scripts/pull-models.sh

# 2. Build and test
make build && make test

# 3. Persistent event-sourced storage
./bin/offgrid exec -db offgrid.db -seed "what's still open?"
./bin/offgrid store -db offgrid.db info
./bin/offgrid store -db offgrid.db verify
./bin/offgrid store -db offgrid.db rebuild

# 4. Measure your machine
./bin/offgrid models                   # what is installed
./bin/offgrid bench                    # tokens/sec for each model
./bin/offgrid ask "what is a CRDT?"    # ask a local model; works with WiFi off
```

Needs Go 1.22 or newer.

## Benchmarks

Measured with `offgrid bench`. Numbers are medians of 3 runs after a warm-up.
Replace this section with the table from `docs/benchmarks.md` after you run
`make bench`.

| Model | Prompt | Load (s) | Time to first token (s) | Generation (tok/s) | Prompt processing (tok/s) |
|---|---|---|---|---|---|
| _run `make bench`_ | | | | | |

**Hardware:** HP EliteBook 845 G8, AMD Ryzen 5 PRO 5650U (6 cores / 12 threads),
16 GB RAM, no GPU used for inference. Zorin OS 18.1.

## Layout

```
cmd/offgrid/              the CLI (models, bench, ask, exec, tools, agent, store)
internal/agent/           tiered agent loop (Tier 0 regex rules, Tier 1 LLM fallback)
internal/store/sqlstore/  event-sourced SQLite storage (append-only log, rebuilt views)
internal/tools/           closed tool registry & parameter parsing
internal/task/            task domain models and storage interfaces
internal/intent/          intent classification schema and validation
internal/structured/      constrained JSON decoding & bounded retry loops
internal/llm/             LLM client interfaces and anti-corruption adapters
internal/ollama/          streaming HTTP client for local Ollama
docs/day-06.md            event-sourced storage architecture & operational requirements
```

## Roadmap

See `docs/day-01.md` for today. The full 30-day plan is in the roadmap file.
