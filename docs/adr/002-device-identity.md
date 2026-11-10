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
- **Gap to close on Day 36:** this ADR says the tag is in every *signature*. `identity.Sign` returns the bare 64 bytes. The `op_sigs` table must store the algorithm beside each signature, or the promise is only half kept.

## Where the implementation stands

Done: key, ID, file format, signing and verifying. Not yet: anything uses the key, and signatures are untagged.
