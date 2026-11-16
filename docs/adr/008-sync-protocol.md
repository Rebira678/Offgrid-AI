# ADR 008: Sync protocol v2

**Status:** accepted, 2026-11-15 (Day 41 of the Days 31 to 60 plan)

## Decision

Keep HTTP with **JSON payloads**. Add: a protocol **version header** on every request and a hard minimum (an older caller gets 426 `upgrade_required` with advice); a pull answer sent as **MAC-chained frames** that the receiver applies as they arrive; **gzip** for pulls and for request bodies over 1 KiB; **back-off with jitter** in `sync run`; and a **maintained summary table** (`op_summary`, kept by a trigger) so a round's cost follows the new ops, not the log.

## Rejected

- **A binary encoding** (protobuf, CBOR). The measurement that would justify it says the wire was not the bottleneck: catching up 10 ops cost 36 ms at 100,000 ops and 6 ms at 10,000 because SQLite scanned the log, and 2 ms at either size once it did not. JSON is readable in a capture and the hash already ignores whitespace.
- **A cursor token separate from the summary.** A replica's ops have no gaps, so its own summary is a complete cursor. A second token would be a second thing to keep in step.
- **One MAC over the whole stream (a trailer).** It would let a receiver apply data before it had checked it, or make it wait for the end and lose the benefit of resuming.
- **Supporting v1 callers in parallel.** It would keep the unframed, unversioned path alive and its ambiguities with it. The group is small enough to upgrade together; the cost is that one device left behind stops syncing, and says why.

## What it costs

One more table that must agree with the log (a trigger keeps it in step; `Verify` checks it; `Rebuild` repairs it); migration 8, after which Day 40's build cannot open the database; every device must upgrade; a little CPU for gzip on a small device.

## What would change it

- A benchmark over a real link that shows bytes on the wire, not database time, dominate: then a binary payload becomes a decision with a number behind it.
- A relay (ADR for Day 43) that must buffer or inspect streams: frames may need a different framing.
- A device that cannot afford gzip: make compression opt-out (the server already honours the client's `Accept-Encoding`).

## Evidence so far

`BenchmarkCatchUp10`: 6.1 ms and 36.0 ms before; 2.0 ms and 2.0 ms after (10,000 and 100,000 ops). Cut-and-resume, tamper and downgrade tests are in `internal/syncer/v2_test.go`; the plan test for the hot queries is `TestHotQueriesDoNotScanTheLog`.

## Where the implementation stands

**Implemented on Day 41.** Not measured: a real network, the Pi.
