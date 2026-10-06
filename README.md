# Offgrid

**An AI agent runtime that keeps working when the internet doesn't.**

A small local model makes instant decisions, a durable queue holds the hard
thinking until a connection returns, and CRDT sync merges offline and online
work without conflicts. Everything runs on one laptop with no GPU and costs $0.

> Status: **Day 1.** Measuring what a CPU-only laptop can really do, before
> designing anything around it. The agent, queue and sync come next.

## Quickstart

```bash
# 1. Install Ollama (runs models locally) and pull two small models
curl -fsSL https://ollama.com/install.sh | sh
scripts/pull-models.sh

# 2. Build and test
make build && make test

# 3. Measure your machine
./bin/offgrid models                   # what is installed
./bin/offgrid bench                    # tokens/sec for each model
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
cmd/offgrid/        the CLI (models, bench)
internal/ollama/    small HTTP client for Ollama: streaming, typed errors
internal/bench/     benchmark method and report writers
internal/sysinfo/   reads CPU and RAM so every result names its hardware
docs/day-01.md      the concepts behind today's code, explained
```

## Roadmap

See `docs/day-01.md` for today. The full 30-day plan is in the roadmap file.
