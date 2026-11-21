# Day 46: durability and throughput of the store

The roadmap row: acknowledged writes survive `kill -9` and a real power pull; appends per second before and after; an ADR with both drivers' numbers. Cost to state: one writer goroutine and a small latency floor.

## What was built

- **Group commit** (`internal/store/sqlstore/groupcommit.go`): one writer goroutine, one transaction and one fsync per batch, acknowledgement only after COMMIT, a SAVEPOINT per request so one failure does not hurt its neighbours. `Options.FlushWindow` (default 0) and `Options.MaxBatch` (default 128). `Store.CommitStats()` reports batches, writes, failures, biggest batch and cancellations.
- **Statement pool**: statements prepared once on the database handle and borrowed into each transaction; restricted to single data statements.
- **Reader pool**: `Get` and `List` use 4 read-only connections and no longer wait behind a commit.
- **Tests**: `groupcommit_test.go` (14), `kill9_test.go`, `cmd/powerpull` tests, updated `stmtcache_test.go`.
- **`cmd/powerpull`**: the tool for the real power pull.
- **`bench/purego`**: a separate module that benchmarks either SQLite driver.
- **ADR 012** with the decisions, the costs and the numbers.

## What I measured

2-vCPU cloud container, Go 1.24, fsync cheap (about 0.15 ms).

| appends per second | 1 writer | 4 | 16 | 64 |
|---|---|---|---|---|
| before | 2,001 | 1,898 | 1,826 | 1,750 |
| after | 2,434 | 2,711 | 3,771 | 4,413 |

- Before, more writers made it **slightly worse**: they only queued for the one connection. After, more writers share commits: 2.5x at 64.
- `List` of 500 tasks while 16 writers hammer the store: 28.5 ms before, 4.8 ms with the reader pool.
- WAL under 16 writers and 24,000 writes: the file peaked at about 4.3 MiB; autocheckpoint is enough.
- kill -9: 12 rounds of SIGKILL at random moments, 2,000+ acknowledged writes, none lost, chain and `Verify` pass after every reopen.

## Honest limits

- **The container's disk is fast.** On a laptop SSD or a Pi's SD card an fsync costs 1 to 20 ms, so sharing it should help much more than here. That is expected, not measured.
- **No real power pull was done here.** SIGKILL does not clear the kernel's cache; only unplugging a real machine tests the disk and the filesystem. That is your part.
- **No modernc number.** The sandbox cannot download it; the harness is ready.
- Go-side allocations per write went up (367 to 423 per lone write) while time went down.
- `ApplyRemote`, `Rebuild`, `Verify`, `Summary`, `OpsSince` still use the writer connection.
- A request function that calls `write` would hang (documented, none does).

## Your part

1. **Power pull** (laptop, then the Pi, on a normal filesystem, not tmpfs):
   - On a second machine: `ssh other 'cat > acks.txt'` is the sink. On the test machine: `go build -o powerpull ./cmd/powerpull && ./powerpull write /path/on/real/disk/test.db | ssh other 'cat > acks.txt'`.
   - After a few seconds, **pull the plug** (or hold the power button on a laptop with the battery out; a VM does not count).
   - Boot, then copy `acks.txt` back and run `./powerpull check /path/test.db acks.txt`. It prints PASS or the lost writes. Repeat at least five times, and once with `-window 5ms`. Record the results (machine, disk, filesystem, mount options, PASS/FAIL) in `docs/real-device-report.md`.
   - If a write is ever lost, stop and send me the output: that is a durability bug or a disk that lies about fsync.
2. **Driver numbers**: in `bench/purego` run `go run . -dir /path/on/real/disk` and then `go get modernc.org/sqlite@latest && go run -tags modernc . -dir ...`; paste both into the table in ADR 012. Also run `scripts/perf/bench.sh perf/<name>.txt` for `AppendConcurrent` on the laptop and the Pi.
3. The older open items still stand (Checkpoint 1 real-device run, goleak, `main.go` merge, benchmarks on named hardware, weekly update).
