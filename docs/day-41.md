# Day 41: Sync protocol v2, and a catch-up that does not grow with the log

The roadmap row: version negotiation, streaming batches, compression, resumable cursors, back-off on errors; make catch-up cost follow the number of new ops, not the log length, with an incrementally maintained summary table; keep JSON for payloads.

## The finding first: where the time went

Day 25 found that syncing 10 new ops cost more as the log grew. Measured again before changing anything (`BenchmarkCatchUp10`: two replicas holding the same N ops, 10 new ones written on one, time to move them over HTTP-free store calls: summary, `OpsSince`, `ApplyRemote`):

| Log length | Before | After |
|---|---|---|
| 10,000 ops | 6.1 ms | 2.0 ms |
| 100,000 ops | 36.0 ms | 2.0 ms |

The allocation count did not move (about 2,100 per round, 104 KB), so the growth was SQLite reading rows, not Go code. `EXPLAIN QUERY PLAN` showed two culprits that read the whole log on every round:

1. `SELECT node_id, MAX(seq) FROM ops GROUP BY node_id` (the version summary) scanned the whole `(node_id, seq)` index.
2. `SELECT ... FROM ops ORDER BY ts DESC, counter DESC, node_id DESC, seq DESC LIMIT 1` (the newest op, which `ApplyRemote` needs to decide between "apply on top" and "rebuild") scanned the log and sorted it in a temporary B-tree. The existing `ops_hlc` index covers only `(ts, counter)`.

A CPU profile was not useful here: the time is inside cgo SQLite, which pprof attributes to one frame. The query plans were the evidence. (A "before" profile run was started and abandoned because it competed with the benchmark for the CPU; I did not get a profile and am not claiming one.)

## What was built

- **Migration 8 (`op_summary`).** One row per replica with its highest `seq`, filled from the log when the database is upgraded and advanced by an `AFTER INSERT` trigger on `ops`, so it moves in the same transaction as every insert (local write, remote apply, a failed batch that rolls back). `Summary` now reads it. It is derived data: `Rebuild` refills it, and `Verify` compares it with the log and fails with `ErrDiverged` if they disagree (`Store.CheckSummary` names the replicas). Migration 8 also adds the index `ops_newest (ts DESC, counter DESC, node_id DESC, seq DESC)` so the newest op is one index entry.
- **The hot queries are named constants** (`querySummary`, `queryNewest`) and a test asks SQLite for their plan: no scan of `ops`, no temporary sort (`TestHotQueriesDoNotScanTheLog`). A future query that brings the scan back fails a test instead of a benchmark nobody runs.
- **Version negotiation.** `ProtocolVersion` is 2, `MinProtocol` 2. Every request carries `X-Offgrid-Proto`. A v1 client never sent it, so the server answers it, signed, with **426 `upgrade_required`** and a message that says what to do ("... you sent no version (an old Offgrid): upgrade Offgrid on the calling device"). The summary answer now says `min_protocol`. A client refuses a peer below its minimum ("upgrade Offgrid on the peer") and a peer that needs newer than it speaks ("upgrade Offgrid on this device"), but still talks to a newer peer whose minimum it meets. `errors.Is(err, ErrProtocol)` is true for all three, including the 426.
- **Streaming pull with a MAC chain** (`internal/syncer/stream.go`). The answer to `/sync/ops` is lines of `<mac> <json>`: frames of up to 100 ops and an end marker with the count and `more`. Each frame's MAC covers the previous frame's MAC, starting from the request's own signature, so a frame cannot be altered, dropped from the middle, reordered, repeated, or lifted from another answer; an answer that stops before its marker is recognised as cut. The receiver checks each frame and **applies it at once**. The writer reuses one buffer and one encoder (12 allocations for a frame of 100 ops, asserted with `testing.AllocsPerRun` as a ceiling of 20). Over mutual TLS the MAC is `-`; the channel protects the bytes and the end marker still marks completeness.
- **Resumable cursor.** The cursor is the replica's own summary: a replica's ops have no gaps, so "the highest seq I hold per node" is exactly where to continue, and frames are applied as they arrive. Test: 250 ops, the connection is cut after two frames; the first round keeps 200, the next round pulls exactly the missing 50, with no duplicates, and the replicas end identical.
- **Compression.** A client asks for gzip on pulls; the server compresses the stream (flushing per frame, so streaming still works) when it is asked. Request bodies over 1 KiB are gzip-compressed (only if smaller) and the signature covers the bytes as sent. The server unpacks after checking the signature and bounds the unpacked size (a gzip bomb gets 400). A test measures the pull answer at under a third of its plain size for repetitive task data.
- **Back-off** (`internal/syncer/backoff.go`, `sync run -backoff-max`, default 30 s). After failed rounds the wait doubles from `-every` up to the cap, jittered between half the cap and the cap; a success resets it. A failure that waiting cannot fix (wrong key, revoked device, another protocol, a tampered frame, a fork) waits the full cap at once but is still retried, so fixing the cause needs no restart. A busy peer's `Retry-After` is a floor. A simulated partition from `-fault` does not back off, so fault tests stay deterministic. The run line says "Next try in 2.4s." while backing off.
- **JSON kept for payloads.** Nothing in the payload encoding changed; the hash still ignores whitespace. A binary encoding would be a new decision made on a measurement of the wire, and this day's measurement says the wire was not the bottleneck: the time was in the database.

## Verification

Failing tests first for the store side (the plan test failed against the old queries; the consistency, drift and migration tests were written before the migration). The syncer tests were written against the new wire: an old client (both paths: summary and pull) gets 426 and the real client with its header removed gets a `RemoteError` that is also `ErrProtocol`; a peer on v1, a peer needing newer, a newer peer that still takes us; cut-and-resume; a missing end marker; an altered byte, a dropped, swapped or repeated frame and a removed MAC are refused with `ErrBadFrame`, and a frame replayed from an earlier answer too; a lying end marker; compression both ways and the bomb; `Retry-After`; the allocation ceiling; the back-off schedule, the cap, jitter, overflow after 500 failures, and the permanent-versus-transient split; the CLI backing off against a dead peer. Existing tests that spoke the old wire were updated: the raw helpers send the version header, the hand-made peers answer with frames, and the schema version is 8.

Mutation checks (each change must fail a test): the summary query back to a scan; the trigger not advancing; `Rebuild` not refilling; `Verify` not checking; the version check; MAC chaining on either side; the version header; the end-marker count; the back-off cap, the `Retry-After` floor and the sanity limit on that hint; the `Retry-After` header. Two survivors, both equivalent in outcome: the `LimitReader` before the unpacked-size check (the check after it gives the same 400; the limit bounds memory, which a test cannot assert reliably) and the EOF special case in the frame reader (the default branch returns the same error).

Full suite with `-race`: 32 packages ok. golangci-lint on a copy: 0 issues (it found four unchecked buffered writes in the new frame writer and an unused field; fixed). `make wasm`: ok. gofmt and vet clean.

## Honest limits

- **One batch is still one query.** The server reads up to `MaxBatch` ops from the store, then writes them as frames. Memory is bounded by the batch, not by a frame. The win of frames is resumption and checking as you go, not a smaller server footprint.
- **The wire gain is not measured.** The benchmark is store-level; I did not measure bytes or time over a real network, and gzip costs CPU on a Pi. The test shows a third of the size on repetitive text; real task data will differ. The cut-and-resume test uses a deliberately cut connection, not a real flaky link.
- **Push is not streamed.** A push is one compressed request body, bounded by `maxBody` (16 MiB) and 4 batches of ops. A cut push is retried whole by the next round (already-held ops count as duplicates).
- **Frames protect integrity, not secrecy.** Over HMAC the ops still cross the wire readable (threat N8 still applies to groups with no membership).
- **Downgrade is closed only by the minimum.** A peer that lies about its protocol with a valid key could send one JSON body; this client would fail to read it as frames and stop. It cannot make the client accept an unchecked body.
- **The summary table is trusted by sync.** A hand edit would be found by `store verify` (and repaired by `Rebuild`), but until then a replica with a table that is too low would offer old ops again (harmless: duplicates), and one that is too high would skip ops until the next verify. That is the cost of one more table that must stay consistent.
- **Not done here:** `signal`-level cancellation of in-flight streams beyond the request context, and `io.Pipe` plus `errgroup` backpressure: with frames applied on arrival, the sender is already limited by the receiver's reads, so a pipe would add a goroutine without adding a limit. If a later benchmark shows the apply step stalling the network, that is the place to add it.

## Your part

1. `go test ./... && make wasm-test && make lint` as before; the Day 37/38/39/40 steps are still open (goleak, `main.go` lines for `ca` and `identity`, Checkpoint 1 on real devices).
2. **Upgrade both machines together.** A v1 build can no longer sync with this one: it gets "upgrade Offgrid on the calling device". Migration 8 runs on the first open of each database (it fills the summary from the log; on 100,000 ops this is one pass). A database opened by this build cannot be opened by Day 40's build, which refuses a newer schema.
3. Run `go test ./internal/store/sqlstore -run xxx -bench CatchUp10 -benchtime 20x` on the laptop and the Pi and put the numbers in the `docs/day-36.md` table next to the others (setup takes a few minutes at 100,000 ops). The ratio between the two rows is the finding, not the absolute time.
4. `offgrid store verify` now also checks the summary table; on any database you care about, run it once after upgrading.
