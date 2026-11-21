# ADR 012: One writer goroutine with group commit; stay on the cgo SQLite driver

**Status:** accepted, 2026-11-20 (Day 46 of the Days 31 to 60 plan)

## Decision

1. **All writes go through one writer goroutine** (`groupcommit.go`). It takes the requests that are waiting, runs each inside a SAVEPOINT of **one** transaction, commits once (one fsync), and only then tells the callers. `synchronous=FULL` and WAL are unchanged: a caller gets `nil` only after the COMMIT that holds its write returned.
2. **A failure stays with its owner.** A request that returns an error or panics has its savepoint rolled back; the others in the batch commit. If the COMMIT itself fails, every request in the batch gets the error and nothing is stored.
3. **No waiting by default.** `FlushWindow` is 0: batches form while the previous commit is on its way to disk. Setting it (for a slow SD card with many writers) trades a latency floor of up to the window for fewer fsyncs. `MaxBatch` is 128.
4. **Prepared statements on two levels.** Per transaction (`stmtCache`, Day 45) and store-wide (`stmtPool`: statements prepared on the database handle, borrowed into a transaction with `tx.StmtContext`, filled by the writer after commits). Only single INSERT/UPDATE/DELETE/SELECT statements are cached: a prepared statement runs only the first statement of a multi-statement text, which broke `resetProjection` (found by a test).
5. **The pool is sized on purpose.** Writer connection: exactly 1 (`SetMaxOpenConns(1)`), because SQLite has one writer and a second connection could only queue on the file lock. Readers: 4 read-only connections (`rdb`, file databases only) used by `Get` and `List`, so a read never waits behind a commit. `:memory:` has one connection and no reader pool (separate connections would see separate databases).
6. **WAL checkpointing stays at SQLite's default** (autocheckpoint at 1000 pages). With one writer connection there is never a reader holding the checkpoint back; the measured WAL stayed at about 4 MiB through 24,000 writes from 16 writers. No tuning added.
7. **The driver stays `mattn/go-sqlite3` (cgo).** The pure-Go port (`modernc.org/sqlite`) was to be measured "for the record". It could not be downloaded in the build sandbox, so **no modernc number exists yet**. `bench/purego` is a separate module that measures either driver with the same schema, pragmas and workloads; run it on the laptop and the Pi and paste the lines into the table below. The rule stands: change driver only if a build target is blocked (a platform with no C toolchain), not for speed.

## Rejected

- **`synchronous=NORMAL`** (faster, can lose the last commits on power loss): a write the caller was told succeeded must survive. Group commit gets throughput without giving that up.
- **`FlushWindow` on by default**: it taxes every lone write to help a load that may never come. Opt in.
- **Several writer connections**: SQLite has one writer; they would only contend on the lock and busy-wait.
- **Switching to modernc now**: no blocked target, and no measurement to justify it.

## Numbers

2-vCPU cloud container, fsync cheap (~0.15 ms), `BenchmarkAppendConcurrent` (`appends/s`, medians of 3):

| writers | before | after |
|---|---|---|
| 1 | 2,001 | 2,434 |
| 4 | 1,898 | 2,711 |
| 16 | 1,826 | 3,771 |
| 64 | 1,750 | 4,413 |

Raw driver harness (`bench/purego`, mattn, same container):

| measurement | mattn | modernc |
|---|---|---|
| insert, one tx each | 7,116 rows/s | _not measured: run `go run -tags modernc .`_ |
| insert, 16 writers sharing a commit | 48,759 rows/s | _not measured_ |
| point read | 315,578 reads/s | _not measured_ |
| full scan | 845,844 rows/s | _not measured_ |

## What it costs

- **A writer goroutine and a hand-off per write**: one channel send and one wake-up. With no window and no company a lone write costs about what it did (it was 481 µs before, 410 µs after, because prepared statements are now reused).
- **A latency floor if `FlushWindow` is set**, measured in a test: a lone write takes at least the window.
- **More Go allocations per write** (367 to 423 for a lone writer) from the statement-borrowing wrappers; CPU time and the C-side preparing fell more than that cost.
- **A request function must not call `write` itself**: the writer would wait for itself. None does; it would hang a test.
- **Cancelling** a request that was not yet picked up returns `ctx.Err()` and it never runs; once picked up it finishes.
- `ApplyRemote`, `Rebuild`, `Verify` and other reads (`Summary`, `OpsSince`) still use the writer connection and queue behind commits.

## What would change it

- A real-disk measurement showing the batch size cap or window hurts a target. Then tune the defaults, not the design.
- A target without a C toolchain: switch driver behind the same `database/sql` code, and re-run the kill -9 and power tests.

## Evidence so far

`groupcommit_test.go` (14 tests: sharing, isolation by savepoint, no ack before commit, commit failure, cancellation, panic, close with writers in flight, window floor, pragmas, WAL bound, readers, memory store), `kill9_test.go` (SIGKILL a child writer at random moments over many rounds; every acknowledged write present, hash chain and Verify pass, no per-writer gaps), `cmd/powerpull` for the real power pull. Mutation checks: acknowledging before COMMIT hangs the no-ack test; removing the savepoint fails the partial-write test.

**Not yet evidenced:** a real power pull (SIGKILL leaves the kernel's page cache intact, so it proves the process side, not the disk side).

## Where the implementation stands

Done and tested in `internal/store/sqlstore`. The power-pull run on real hardware is the user's part (`docs/day-46.md`).
