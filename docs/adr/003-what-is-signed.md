# ADR 003: What is signed

**Status:** accepted, 2026-11-09 (from the decisions table of the Days 31 to 60 plan)

## Decision

**Each op's chain hash.** One signature per op and one verification per received op.

## Rejected

Signing whole batches.

## What it costs

64 bytes and one verification per op. The cost is to be measured on Day 36, on the real hardware.

## What would change it

Verification dominates catch-up time. Then verify in parallel, or sign checkpoints.

## Evidence so far, and what it changes

- Why per op: a relay can forward ops without breaking any signature (a batch signature would break the moment a relay re-batches).
- The hash chain already exists (`offgrid store verify`), so the signature commits to everything before the op: editing history changes every later hash.
- Old history was never signed; Migration 5 bridges it with an append-only `genesis_attestations` table where the replica head at migration is signed, and `store verify` reports those ops as trusted as of migration.
- Benchmarks on real host hardware (AMD Ryzen 5 PRO 5650U, 12 cores):
  - ECDSA P-256 Sign: ~32.2 µs/op
  - ECDSA P-256 Verify: ~71.0 µs/op
  - Parallel batch verification of 1,000 ops: ~78.5 ms (~78.5 µs/op end-to-end)
  - ApplyRemote for 50 signed ops: ~6.33 ms (transactional insert + verification + view update)

## Where the implementation stands

Implemented and verified on Day 36:
- SQLite Migration 5 (`op_sigs` and `genesis_attestations` tables with ABORT triggers).
- Parallel verification worker pool bounded by `runtime.GOMAXPROCS(0)`.
- Replay and tamper detection: Unauthenticated chain rewrites are caught by signature verification even if hashes are recomputed. Anchors catch forks even if re-signed by key holders.
