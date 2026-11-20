# Day 45: a performance baseline you can compare against

The roadmap row: profile the hot paths with pprof; make the benchmark suite one command with a benchstat comparison in CI that fails on a regression unless someone overrides it on purpose; write budgets for named hardware; fix the top three allocation sites with before and after; try profile-guided optimisation (PGO) and keep it only if it wins.

## What was built

| | |
|---|---|
| `scripts/perf/bench.sh OUT.txt` | The suite, one command: 41 benchmarks across crdt, hlc, jobqueue, syncer, agent, sim and the store, `-count=10 -benchmem`. `PROFILE=dir` also writes cpu, memory and mutex profiles per package (profiles are taken in their own pass, never in a timed one). `ROOT=` runs the script against another checkout, which is how CI measures the base branch. |
| `scripts/perf/compare.sh OLD NEW` | benchstat's table if installed, then the gate. |
| `cmd/perfgate`, `internal/perfgate` | The gate. Reads `go test -bench` text. A metric is a **regression** only if it is worse than a threshold (10% time, 10% bytes, 5% allocs) **and** the Mann-Whitney test says the difference is real (p < 0.05, medians, as benchstat does). Runs from different hardware are never compared. Standard library only, so `go.mod` does not change. |
| `perf/budgets.json` | Ceilings for named hardware, each with a reason. Enforced only when the run's `cpu:` line contains the file's `hardware` text; on any other machine they are reported as "not enforced", never as passed or failed. |
| Override | `Perf-Override: <reason in words>` in the pull request description (CI) or `PERF_OVERRIDE="..."` (local). A bare "ok" is refused (3 words, 15 characters at least); the reason is printed in the report. An override with nothing failing says it was not needed. |
| `.github/workflows/perf.yml` | On a pull request: measures the base and the head **one after the other on the same runner** (shared runners differ from day to day, so a stored baseline would measure the machine), prints benchstat, runs the gate, writes the report to the job summary. The description is passed as an environment variable, never pasted into the shell. |

Two things in the existing benchmarks were broken and are fixed: `BenchmarkDeleteTask` called a helper with a nil `*testing.T` and crashed the whole run, and `BenchmarkCatchUp10`, `BenchmarkRebuild`, `BenchmarkFields` and `BenchmarkVerifyChain10k` rebuilt their whole log with durable writes **on every call** of the benchmark function (and Go calls it many times: b.N = 1, then larger, then once per `-count`). `CatchUp10` took 540 s per count; the suite could not be run ten times. Fixtures are now built once per test process and copied (`fixtureDB`). The timed region is unchanged.

## What the profiles said

Measured on the 2-vCPU cloud container (`Intel(R) Xeon(R) Processor`), Go 1.24.

- **About a fifth of all CPU** in Rebuild and ApplyRemote was `sqlite3_prepare_v2`: `database/sql` prepares a statement for every call on a transaction and throws it away, and one op runs the same four or five statements (select task, insert mark, count removals, insert note, insert op, append to the chain, store signature) over and over.
- `OpHash` (run for every op written, received and verified) allocated 6 times and 312 bytes per call: a fresh hasher, seven byte-slice conversions, a buffer.
- The rest of the time is inside SQLite (cgo) and the driver's per-column string allocation. Those are not ours to change cheaply; the real lever there is fewer statements per op, which is Day 46's work (group commit, batching).
- Mutex profile: nothing contended in the store suite (single writer by design).

## The three fixes

1. **A statement cache per transaction** (`stmtcache.go`): each distinct statement is prepared once per transaction and reused, in `replay`, `replayCanonical` and the apply-on-top loop. Bounded at 64 statements; closed with the transaction; a `*sql.DB` is never wrapped.
2. **`OpHash`** builds its message in one reused buffer and hashes it in one call: 6 allocations and 312 B become 1 and 64 B, about 20% faster (`BenchmarkOpHash` against `BenchmarkOpHashReference`: 600 ns against 745 ns). The bytes hashed are identical: the Day 27 construction is kept in the test file as the reference, 2,000 random ops must hash the same, and one value is pinned.
3. **`ApplyRemote`'s per-op inserts** (op row, chain append, signature) use the same cache.

### Before and after

Same machine, same session, `count=10`, medians; every row has p < 0.001 (gate output in `docs/day-45-report.md`).

| benchmark | before | after | change |
|---|---|---|---|
| Rebuild, 10,000 ops | 347.7 ms | 179.6 ms | **-48%** |
| Rebuild, 1,000 ops | 34.2 ms | 17.3 ms | -49% |
| ApplyRemote, 1,000 ops, unsigned | 62.1 ms | 29.0 ms | -53% |
| ApplyRemote, 1,000 ops, signed | 128.8 ms | 87.2 ms | -32% |
| ApplyRemote, batch of 500 | 36.3 ms | 23.5 ms | -35% |
| Catch up 10 ops (log of 10,000) | 1.44 ms | 1.16 ms | -19% |
| Sync round trip (syncer) | 708 µs | 582 µs | -18% |
| VerifyChain, 10,000 ops: allocations | 289,053 | 219,257 | -24% |
| VerifyChain, 10,000 ops: bytes | 8.62 MiB | 6.15 MiB | -29% |
| ApplyRemote, 1,000 ops, signed: allocations | 258,800 | 227,115 | -12% |

Everything else (crdt, hlc, jobqueue, agent, sim, a single write) is within noise, as it should be. I did not measure the three fixes one by one except OpHash; the table is all three together.

## PGO: tried, rejected

A profile merged from the store and syncer suites (`default.pgo`, 82 KB) was used to build the store's test binary (`-pgo=file`) and compared with `-pgo=off`, 10 samples each, 11 benchmarks. Times moved between -7% and +6% with no pattern and none significant; **allocations got worse** (`OpHash` 1 to 3 allocations, `VerifyChain` +9%), because PGO's inlining changes escape analysis. The cost is a committed 82 KB profile that goes stale with every change. No `default.pgo` is shipped. The store is bound by SQLite and the driver, which PGO cannot see into. Worth trying again only on a build of `cmd/offgrid` that is bound by Go code (the agent loop), and with the numbers in this table as the bar.

## Budgets

`perf/budgets.json`: eight ceilings, for the reference container, each with its reason (a write under a 16 ms frame; rebuild of 10,000 ops under 0.4 s; catch-up of 10 ops flat in the log size; 1,000 signed ops under 0.2 s; full verify of 10,000 ops under 0.1 s; allocations per signed write; one sync exchange; clock read). The ceilings are 2 to 10 times the measured medians. **They are mine, set from measurement and from what a person waiting would accept; the "(set)" targets in the product spec are still yours to decide.** Tested both ways: all hold on the real run; lowering one makes the gate fail and print the reason.

## Honest limits

- **Not your hardware.** Every number is from the container. Append is 0.47 ms here; on a laptop SSD it is often several milliseconds because fsync costs more. That is why the budgets name their hardware and are not enforced elsewhere.
- **benchstat could not be installed in this container** (the module proxy for golang.org/x/perf is not reachable here). The CI step installs it with `@latest`, unpinned and `continue-on-error`, because it is only the readable table; the gate does not depend on it. I have not seen benchstat's output on these files.
- **The workflow has not run on GitHub.** The YAML parses and every step's logic (the gate, the override, the base/head scripts) was run locally, but the first real pull request is the first real run. Expect to adjust the runner time (the suite takes about 25 minutes at count=10 here; CI uses count=6 for each of base and head).
- **A shared runner is noisy.** The gate uses medians, a significance test and a 10% threshold, and measures base and head back to back; it will still, occasionally, fail something that is not real. That is what the override is for.
- **No unit test pins "statements are prepared once".** The suite and the gate do: remove `cached(...)` and Rebuild is +90% slower (the reversed comparison in the report shows exactly that). The cache's behaviour (same answers as plain calls, bounded, not applied to a `*sql.DB`) is unit tested.
- **Not fixed, found:** a local write returns the whole task, so it reads every note: a write to a task with hundreds of notes costs about 1 ms (`RemoveNote`: 4,482 allocations per call). Each statement also costs the driver one string allocation per column. Day 46 (group commit) is the place for both.
- The suite leaves out `Rebuild/50000` and `CatchUp10/log-100000`, which take minutes to build; `docs/day-36.md` has them.

## Your part

1. Merge the zip. `make test && make lint`.
2. On your laptop (quiet, plugged in): `scripts/perf/bench.sh perf/laptop.txt` (about 25 minutes), then `cp perf/budgets.json perf/budgets.laptop.json`, set `"hardware"` to the `cpu:` text at the top of `perf/laptop.txt`, set each `max` from what the machine gives, and run `BUDGETS=perf/budgets.laptop.json scripts/perf/compare.sh perf/laptop.txt perf/laptop.txt` to see them hold. Do the same on the Pi.
3. Open one pull request with a deliberate slowdown to see CI fail, and again with `Perf-Override: testing the override path` in the description to see it pass with the reason on the record.
4. Still open from before: Checkpoint 1 real-device run and report, `go get go.uber.org/goleak@v1.3.0 && go mod tidy`, the `main.go` cases (`ca`, `identity`, `hub`), Day 41 migration (upgrade all devices together, then `offgrid store verify` once), ADR 009/010 outside review.
