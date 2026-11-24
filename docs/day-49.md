# Day 49: review week (light)

Nothing new was built to add features. Four things happened: the numbers were written down in one place, a growth alarm replaced the plan to build compaction ([ADR 015](adr/015-measure-growth-before-compacting.md)), every kept optimization was matched with a benchmark, and the race detector and leak checks were run over the whole repository.

**Hardware for every number below:** a 2-vCPU cloud container, Intel Xeon (the `cpu:` line in `perf/baseline-day45-*.txt`), Go 1.24.7, Linux 6.18, cgo SQLite. Not a laptop, not a Pi, not a phone. Medians of `-count=10` unless it says otherwise. The last two columns are for you to fill in from `scripts/perf/bench.sh` on your own machines.

## What I measured

### Before and after

| What | Before | After | Day | Laptop | Pi |
|---|---|---|---|---|---|
| Catch up 10 ops, log of 10,000 | 6.1 ms | 2.0 ms | 41 | | |
| Catch up 10 ops, log of 100,000 | 36.0 ms | 2.0 ms | 41 | | |
| Rebuild the view, 10,000 ops | 347.7 ms | 179.6 ms | 45 | | |
| Apply 1,000 remote ops, unsigned | 62.1 ms | 29.0 ms | 45 | | |
| Apply 1,000 remote ops, signed | 128.8 ms | 87.2 ms | 45 | | |
| Apply a batch of 500 | 36.3 ms | 23.5 ms | 45 | | |
| Verify the chain, 10,000 ops: allocations | 289,053 | 219,257 | 45 | | |
| Sync round trip | 708 µs | 582 µs | 45 | | |
| Appends per second, 1 writer | 2,001 | 2,434 | 46 | | |
| Appends per second, 16 writers | 1,826 | 3,771 | 46 | | |
| Appends per second, 64 writers | 1,750 | 4,413 | 46 | | |
| List while 16 writers write | 28.5 ms | 4.8 ms | 46 | | |

"Catch up" being flat at 2.0 ms from 10,000 to 100,000 ops is the one that matters for a crew that never stops using it: the cost of syncing a few new ops no longer depends on how long the log is.

### New this week (no "before": there was no feature before)

| What | Number | Where |
|---|---|---|
| Take in a 4 MiB photo (stream, hash, fsync, rename) | 11.5 ms, 364 MB/s | `BenchmarkPut` |
| Finish a cut transfer, second half of 4 MiB | 10.6 ms | `BenchmarkResume` |
| Database bytes per op, unsigned (task, note, status mix) | **565 B** (payload is 68) | `TestWhatOneOpCostsOnDisk` |
| Database bytes per op, signed | **767 B** | same |

Honest reading of the resume number: finishing the second half costs about as much as taking the whole file in, because the commit hashes the whole file again. Resume saves the **network**, which is what is scarce on a site's link, not the CPU. That is the right trade (a wrong hash must never be accepted) and it is written down so nobody expects resume to be fast on a slow disk.

### What a crew-year costs

Signed log, 767 bytes per op. Replace the ops-per-day column with the pilot's real number (`offgrid store -db F growth` prints it after a week of use).

| Ops a day, whole crew | Log added per year | Years to 500,000 ops (ADR 010's trigger) |
|---|---|---|
| 50 | 14 MB | 27 |
| 200 | 56 MB | 6.8 |
| 1,000 | 280 MB | 1.4 |

| Photos a day (3 MB each) | Blobs added per year |
|---|---|
| 1 | 1.1 GB |
| 5 | 5.5 GB |

The log is not the problem. Photos are, and for them the tools already exist (`gc`, the free-space guard). This is why compaction was not built.

**Limits of these numbers.** 3,000 synthetic ops from one writer on one machine; real use has other mixes (longer notes, more edits) and a new database carries about 0.1 MiB of fixed schema. The estimate "from the pilot's own use" needs the pilot's database; until then it is a model, and the command refuses to project from under 7 days of history rather than extrapolate a burst.

## The growth alarm

```
offgrid store -db FILE growth                         numbers, cost per op, one-year projection
offgrid store -db FILE growth -record                 also keep this sample in FILE.growth.json
offgrid store -db FILE growth -max-ops 500000 -max-db 1024 -max-blobs 8192 \
        -max-db-per-day 5 -max-blobs-per-day 200 -year-db 1024 -year-blobs 16384
scripts/growth-alarm.sh                               the cron wrapper (daily); writes FILE.growth.alarm and runs $ALARM_CMD
```

Exit 3 means an alarm. The limits are ADR 010's triggers (500,000 ops, 1 GB) plus a blob ceiling and a growth-rate check over the last 7 days; sizes are MiB. A projected-size alarm needs at least 7 days of history, so a first week cannot page anyone. The history file is advisory: lose it and only the rate alarm goes quiet.

## Every kept optimization has a benchmark

| Optimization | Benchmark that justifies it |
|---|---|
| Statement cache and store-wide pool | `ApplyRemote*`, `Rebuild`, `VerifyChain10k` (Day 45 table) |
| Single-pass `OpHash` | `OpHash` against `OpHashReference`, 600 against 745 ns |
| Group commit, one writer | `AppendConcurrent` |
| Reader pool | `ListUnderWrites` (now in `bench.sh`; it was not before) |
| Index on the clock for catch-up | `CatchUp10/log-10000` and `-100000` |
| Batch apply | `ApplyRemoteBatch500` |
| Streaming blob store, resume by offset | `BenchmarkPut`, `BenchmarkResume` (new, in `bench.sh`) |

Two gaps found and closed: `ListUnderWrites` was measured on Day 46 but not in the suite, so the perf gate could not have caught its regression; and the blob path had a heap test but no benchmark.

## Deleting what did not earn its keep

A scan of every exported function in `internal/` for use outside tests found two with none: `intent.Priorities()` and `intent.Whens()`. Both are used by the eval test and Day 50's eval set needs them, so they stay. `bench/purego` is a separate module you run on purpose; it stays until you have pasted its numbers into ADR 012. **Nothing deleted this week.** I would rather say that than invent a deletion.

## Race and leak checks across the repo

`go test -race -short ./...` is clean. `goleak` now guards `hub`, `web`, `blobstore` and `sqlstore` as well as `syncer` and `cmd/offgrid`; a deliberately leaked goroutine in a scratch test failed both the blobstore and the sqlstore runs, so the guard is real. Every store in a test is closed, which is what the sqlstore guard proves.

## Mutation checks

Twelve mutations of the growth arithmetic (`>` to `>=` on each limit, the 7-day gate removed, the one-day floor, the active-days scale, baseline choice, history cap, file mode, projected total) plus one on the ops limit: all caught.

## Your part

1. Run `offgrid store -db <your real database> growth` and paste the output into ADR 015's evidence section. If the log spans under 7 days, it will tell you so.
2. Put `scripts/growth-alarm.sh` in cron on the Pi and the laptop. Set `ALARM_CMD` to anything that reaches you.
3. Run `scripts/perf/bench.sh perf/<name>.txt` on the laptop and the Pi and fill the last two columns of the first table.
4. Send the pilot check-in below, and post the weekly update with your real numbers swapped in.
5. `go get go.uber.org/goleak@v1.3.0 && go mod tidy` if you have not (the new `main_test.go` files need it).

## Weekly update (draft, post after you add your own numbers)

> **Week 7: what I measured.** Syncing 10 new ops takes 2 ms whether the log has 10,000 ops or 100,000 (it was 36 ms). Adding a photo and sending it over a flaky link now resumes where it stopped. A signed op costs 767 bytes on disk, so a crew doing 200 ops a day adds 56 MB a year; the photos are what fill a card (about 1 GB a year per photo a day). So I did not build compaction. I wrote an alarm instead, and I will build it only if the alarm fires. All numbers are from a 2-vCPU cloud box; Pi and laptop numbers next week.

## Pilot check-in (draft message)

> Hi <name>, it has been a week since you started. Three questions, short answers are fine: (1) Did anything lose or duplicate something you wrote? (2) When you were offline, did anything surprise you when you came back? (3) Roughly how many tasks and photos did you add on a typical day? I will use the third to work out how big your data gets in a year. Nothing to install: please run `offgrid store -db <file> growth` once and send me the output, it contains sizes and counts only, no task text.

## Posts (the review-day posts owed since Nov 3)

**LinkedIn**

> Review week on Offgrid. I did not build the thing I planned to build, and I can show why.
>
> The plan was log compaction. Then I measured: a signed operation costs 767 bytes on disk. A crew making 200 changes a day grows 56 MB a year. Photos, not the log, are what fill a small device's card: one photo a day is about a gigabyte a year.
>
> So the log stays append-only, and I wrote an alarm that fires when ops, database size, blob size, or growth rate cross a limit. If it fires on real pilot data, I build compaction. If not, I saved myself the riskiest change in the project.
>
> Also this week: syncing 10 new changes takes 2 ms at 100,000 ops (it was 36), and a cut photo transfer resumes where it stopped. All numbers are from a 2-vCPU cloud box; Pi numbers next.

**X**

> Planned log compaction. Measured first: a signed op = 767 bytes on disk, 200 ops/day = 56 MB/yr. Photos are the real growth (~1 GB/yr per photo/day).
> Built an alarm instead. Compaction only if it fires.
> Also: sync of 10 new ops is 2 ms at 100k ops (was 36 ms). 2-vCPU box; Pi next.
