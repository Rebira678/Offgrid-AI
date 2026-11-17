# ADR 009: Envelope encryption for a hosted relay

**Status:** proposed, 2026-11-16 (Day 42). **Design only: nothing is built until the pilot shows that someone needs it.** Needs an outside reviewer before it is accepted (see Review).

## Context

Every op is signed, so a relay cannot forge one. A relay can still **read** every op it stores (threat R1, accepted today because the relay is run by the site owner). A relay run by someone else, a hosted one, would turn that accepted risk into a real one: task titles, notes and photos of faults would sit readable on a machine the crew does not control. Signatures say nothing about secrecy.

## Decision

Encrypt each op's payload on the device that makes it, so the relay stores and serves ciphertext it cannot open.

1. **A fresh random 256-bit content key per op.** The payload is sealed with ChaCha20-Poly1305 under that key.
2. **The content key is wrapped to every member who can read the op**, taken from the membership fold at the op's position. One ephemeral P-256 key per op does ECDH with each member's device key; HKDF-SHA256 (info binds the group, the op's node and seq, and the member's device ID) gives a wrap key; ChaCha20-Poly1305 seals the content key. Per member the envelope adds about 8 bytes of key ID plus 48 bytes of wrapped key; one 65-byte ephemeral public key is shared. A crew of 10 adds about 600 bytes to an op.
3. **Associated data binds the envelope to its place:** group ID, node, seq, kind of envelope, and the member list hash. An envelope cannot be moved to another op, another group or another recipient list without failing to open.
4. **Nonces are counters, not random** (`internal/envelope`, the one part already written and tested). The content key is used once, so even a bug could not repeat a (key, nonce) pair under it unless a scope were reused; the scope refuses to hand out the same nonce twice, separates body and wrap by a purpose byte, and refuses past its limit. Wrap keys are fresh per (op, member) because the ephemeral key is fresh per op.
5. **What the relay sees (cleartext):** group, node, seq, the HLC timestamp, the envelope size, which members the op is wrapped to. **What it does not see:** kind, payload, attachments' names.
6. **Two signatures.** The existing signature over the plaintext op stays (nodes check it after decrypting, and the hash chain stays over the plaintext). A second, outer signature by the same device key covers the envelope as stored (ciphertext hash, node, seq, group), so a relay can refuse bytes from a non-member without reading anything, and a node can tell what the relay really held.
7. **Joining and leaving.** A new member cannot read ops wrapped before they joined. They get history from a member, who re-wraps content keys for them (an op the group already holds: `member.rewrap`, signed, small); until then they hold the snapshot only (ADR 010). A revoked member is left out of **later** ops. They keep every key they already unwrapped.

## Rejected

- **One shared group key.** Simple, and the wrong shape: revoking a device means a new key to every other device, and one lost phone opens everything ever written. Per-op keys make revocation a membership fact, which Offgrid already has.
- **TLS to the relay only.** Protects the wire, not the relay's disk or its operator. This is the whole question.
- **A wrapped copy of the whole payload per member.** Cost grows with payload times members; photos make that unaffordable.
- **A library such as age or NaCl box, or the Noise protocol.** More dependencies and a different identity model. The devices already have P-256 keys; the AEAD comes from `golang.org/x/crypto` (ChaCha20-Poly1305 is faster than AES-GCM on a Raspberry Pi without AES instructions) and ECDH and HKDF from the standard library.
- **Random 96-bit nonces.** A collision is improbable, but the whole point of a rule is that a bug cannot silently break it. A counter that refuses to repeat is checkable by a test (`TestAScopeNeverRepeatsANonce`, concurrent and with purposes).

## What it costs, and what it does not give

- **No forward secrecy.** A device that is stolen with its key can unwrap every op wrapped to it. Revocation stops later ops, not past ones.
- **Metadata is visible:** who wrote how many ops of what size, and when. Sizes may be padded to buckets; that would cost bandwidth and is not decided here.
- **The relay can still withhold, delay, reorder or drop ops.** The cursor and the per-node sequence let a node notice a gap; nothing here makes the relay available.
- **A bug in key wrapping is a data-loss bug**, not a leak: an op wrapped to the wrong list is unreadable to someone who needed it. The membership fold used for the list must be the same one the receiver uses; a race at a membership change can produce an op wrapped to a stale list, repaired by `member.rewrap`.
- **Work and size:** about 600 bytes per op for 10 members, one ECDH per member per op (cheap on a laptop, measurable on a Pi: to be benchmarked first), a second signature, and a new envelope format on the wire (protocol v3).
- **Search, rules and the model work on plaintext on the nodes.** None of that moves to the relay.

## What would change it

- The pilot shows nobody uses a relay they do not run: do not build it. The relay stays run by the site owner (R1 stays accepted).
- A crew that needs a hosted relay and cannot accept the missing forward secrecy: a ratchet design, which is a much larger piece of work.
- A benchmark on the Pi that makes per-op wrapping too slow for bulk catch-up: wrap per batch instead of per op (weaker, and a new ADR).

## Review

To be sent to one outside reviewer with the questions: Is binding the HKDF info to (group, node, seq, device ID) enough to stop a wrap being moved? Does signing both the plaintext and the envelope open any confusion between them? Is a shared ephemeral per op acceptable when the same op goes to ten members? **Not yet reviewed.** Until a reviewer has answered, the status stays "proposed".
