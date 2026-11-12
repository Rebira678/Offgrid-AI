# ADR 003: What is signed

**Status:** accepted, 2026-11-09 (from the decisions table of the Days 31 to 60 plan)

## Decision

**Each op's chain hash.** One signature per op and one verification per received op.

## Rejected

Signing whole batches.

## What it costs

64 bytes and one verification per op. Measured on Day 36: see `docs/day-36.md` (sandbox numbers; the Pi and laptop rows are yours to fill).

## What would change it

Verification dominates catch-up time. Then verify in parallel, or sign checkpoints.

## Evidence so far, and what it changes

- Why per op: a relay can forward ops without breaking any signature (a batch signature would break the moment a relay re-batches).
- The hash chain already exists (`offgrid store verify`), so the signature commits to everything before the op: editing history changes every later hash.
- Old history was never signed; the roadmap bridges it with a one-time genesis attestation and says plainly that old ops are trusted as of that moment.

## Where the implementation stands

Done on Day 36: migration 5 (`op_sigs`, `device_keys`, `node_devices`), `Options.Signer`, verification in `ApplyRemote` in a pool bounded by `GOMAXPROCS`, `history.attested` for old ops, `store verify` and `store attest`. The signed bytes are `"offgrid-opsig-v1\x00" + chain hash`. Not yet: the device-to-node binding is trust on first use until enrollment (Day 38), and signing is opt-in until Day 37.
