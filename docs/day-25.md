# Day 25: benchmarks anyone can reproduce

**Done when:** the benchmarks are reproducible by anyone. `make bench` measures the system and rewrites the README tables, and the hardware is printed next to every number.

```bash
make bench            # system costs, then tokens/sec if Ollama is reachable
make bench-quick      # a few seconds, to see it work
make bench-system     # only the system costs (no model needed)
DIR=/mnt/ssd make bench-system   # put the files on the disk you care about
```

## What is measured

| Area | What | Why this way |
|---|---|---|
| Store | 1,000 durable appends; open a log of 100 / 1,000 / 10,000 ops; rebuild the view from it | `synchronous=FULL`, so every append is on disk when it returns. That is the real cost of "it survived the crash". |
| Sync | a new replica catching up on 100 / 1,000 / 10,000 ops, in process and over HMAC-signed HTTP on localhost; also 10 new ops on a log that is already that long | the catch-up number is the first-sync story. The 10-op number is the everyday one. |
| Queue | 100 and 1,000 jobs that do nothing, 1 or 4 at a time: enqueue time and drain time | jobs do nothing on purpose, so this is the queue's own cost, not the model's. |
| Memory | peak Go heap (sampled every 10 ms) and peak RSS of the whole process, per row | RSS counter is reset before each row (`/proc/self/clear_refs`), so one row does not inherit another's peak. |
| Tokens/sec | `offgrid bench -models ...` (Day 1), now also rewrites the model table in the README | needs Ollama. `make bench-models` skips with a message if Ollama is not reachable. |

## Why you can trust a number

- **Fixed seed, exact sizes.** The workload is seeded, and a test counts that it writes exactly N ops.
- **Wrong runs fail instead of reporting a time.** After every sync the two replicas must have the same state hash. A drain that did not drain every job is an error.
- **Median of 3, with min and max.** Every time is `median [min to max]`, so you can see the noise.
- **Hardware and run recorded.** CPU, threads, RAM, OS, Go version, date, seed, filesystem.
- **Only marked blocks are rewritten.** `make bench` replaces text between `<!-- perf:start -->` / `<!-- perf:end -->` (and `bench:` for the model table). If a marker is missing, duplicated or reversed, it refuses and the README is untouched.
- Tests, plus 16 mutants (a bug put into the code, tests must fail): all caught.

## Results from my sandbox, NOT your laptop

Intel Xeon 2.80GHz, 2 threads, 7.8 GiB, ext4, Ubuntu 24.04. These are in the README only until you run `make bench`, which replaces them with your EliteBook's numbers.

| | 100 ops | 1,000 ops | 10,000 ops |
|---|---|---|---|
| new replica, in process | 15 ms | 186 ms | 2.14 s |
| new replica, HTTP (localhost) | 25 ms | 198 ms | 2.19 s |
| rebuild the view | 25 ms | 186 ms | 1.47 s |
| 10 new ops on a log this long, HTTP | 4.4 ms | 6.4 ms | 20 ms |

Appends: about 860 per second (fsync-bound). Queue drain: 520 to 650 jobs per second. Whole-process peak RSS: 18 to 25 MiB.

**What I noticed.** Catching up on 10 new ops costs more as the log grows: 4 ms, 6 ms, 20 ms. The data moved is the same 10 ops, so the cost is in finding what is new (the summary and the checks over the whole log). Not a problem at these sizes, and a clear first thing to optimise if logs reach the hundreds of thousands. I did not change it today; a benchmark day should not change the thing it measures.

Opening a log takes about 1 ms at any size: opening does not replay. Rebuilding does, and that is the 1.47 s.

## What this does not show

- **Not your laptop.** These are from a cloud sandbox. The chart says so on its face. Yours will differ, probably faster disk, same shape.
- **No tokens/sec.** There is no Ollama in my sandbox. Your Day 1 table stays until you rerun `make bench`.
- **HTTP is localhost.** It measures protocol, signing and storage, not WiFi.
- **Memory is the process, not Ollama.** A loaded model takes gigabytes on top; that is `ollama ps`, not this table.
- Heap sampling every 10 ms can miss a very short spike. RSS is exact (kernel high-water mark).
- Queue jobs do nothing. A real job is dominated by the model.

## The post (Day 25)

Attach `docs/day-25-benchmarks.png`, but **only after you have run `make bench` on your laptop and regenerated it** (`make bench` rewrites the SVG; export it to PNG). The one in this zip says "Xeon, 2 threads"; do not post it as yours.

> Benchmarks for my offline AI runtime, on an HP EliteBook 845 G8: Ryzen 5 PRO 5650U, 15 GB, no GPU.
>
> A new replica catches up on 10,000 operations in <your number>. Peak memory of the whole process: <your number> MiB. Queue drains <your number> jobs a second. A 3B model on this laptop does <your tokens/sec> tokens a second.
>
> Everything is one command: `make bench`. It prints the hardware next to every number, runs each test 3 times and shows min and max, and fails instead of reporting a time if the two replicas did not end identical.
>
> What surprised me: syncing 10 new ops gets slower as the log gets longer (4 ms to 20 ms from 100 to 10,000 ops). Now I know where to look first.

Hook for CTOs: *you can run my benchmark and check my claim.*

## Checklist

- [ ] `go get pgregory.net/rapid@v1.2.0`, commit `go.mod` and `go.sum`.
- [ ] `make bench` on the EliteBook (about 3 to 4 minutes; Ollama running for the model part). Commit the README, `docs/perf.*`, `docs/benchmarks.*`.
- [ ] `make bench` also rewrites `docs/day-25-benchmarks.svg` with your numbers; export it to PNG for the post (the PNG in this zip is the sandbox one).
- [ ] Quote your numbers, never the sandbox's.
- [ ] Still open from earlier: the 7-8B row (`STANDIN=0 make two-brains-demo`, then `offgrid bench -models qwen2.5:7b`) and `ollama --version`.
- [ ] 15 minutes of outreach.
