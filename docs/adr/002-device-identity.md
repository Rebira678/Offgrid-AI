# ADR 002: Device identity

**Status:** accepted, 2026-11-09 (from the decisions table of the Days 31 to 60 plan)

## Decision

An **ECDSA P-256 key generated on the device, never exported**, with an algorithm tag in every key and signature. One key per device, not per person; attribution is to a device with a human label recorded at enrollment.

## Rejected

Ed25519; today's shared HMAC key.

## What it costs

Key management, enrollment and revocation. A lost phone means revoking a key. An ECDSA signature is not unique per message, so **never use a signature as an identifier**: hash the op.

## What would change it

You never need hardware-backed keys or browsers. Then Ed25519 is simpler.

## Evidence so far, and what it changes

- Why P-256 and not Ed25519: it is the curve WebCrypto offers everywhere, and Day 33 showed a non-extractable P-256 key signing and verifying on a real phone.
- Day 34: `internal/identity`. Mode 600 enforced, algorithm tag in the key file and in every public key (`ecdsa-p256:...`), device ID is `dev_` and 32 hex characters of the SHA-256 of the public key, a signature is 64 bytes raw `r||s` so Go and WebCrypto agree.
- Day 36: Full signed operations integration. The `op_sigs` and `genesis_attestations` tables store the algorithm tag (`ecdsa-p256`) and `device_id` beside every signature. WireOp protocol carries tagged public keys and signatures.

## Where the implementation stands

Implemented and verified on Day 36:
- Device keys loaded or created at `~/.config/offgrid/device.key` (mode 0600 enforced).
- Every CRDT op writes an append-only row to `op_sigs` with algorithm tag, device ID, public key, and 64-byte signature.
- Parallel verification pool scales signature validation across all available CPU cores.
