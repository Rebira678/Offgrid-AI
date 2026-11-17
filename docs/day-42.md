# Day 42: Two designs, no features (a light day)

The roadmap row: write the designs for envelope encryption (for a future hosted relay) and for compaction with a maximum time away; build only what can be tested now; get the designs reviewed by someone else; weekly update; pilot conversations.

## What is here

- **[ADR 009: envelope encryption](adr/009-envelope-encryption.md).** A random content key per op, ChaCha20-Poly1305, the key wrapped to each member by ECDH on their device key, counter-derived nonces, two signatures (plaintext and envelope). States what a relay still sees and what it can still do, and that there is **no forward secrecy**.
- **[ADR 010: compaction](adr/010-compaction.md).** An admin-signed checkpoint plus a snapshot, valid only once every active member has acknowledged the cut or has been away longer than the maximum time away (default 90 days); the membership log is never compacted; a device away longer gets `rebase_required` and re-applies its unsynced edits. Ends with the **alarm to measure in the pilot** (ops, database size, verify time) that decides whether to build it at all.
- **`internal/envelope`: the one testable piece.** Counter-derived nonces: `Nonce(purpose, counter)` and a `Scope` that never hands out the same nonce twice, separates body and wrap by a purpose byte, is safe for concurrent use and refuses past its limit. Four tests; five mutations (counter not advancing, purpose byte dropped, limit removed, byte order changed, lock removed) each fail a test. **Nothing calls it.** It uses only the standard library, so no change to `go.mod`.
- Threat model: R1 now points to ADR 009.

## Honest limits

- **Neither ADR is reviewed.** The roadmap asks for someone else to read them; I cannot do that, and a design reviewed only by its author has the usual blind spots. Each ADR lists the questions I would put to a reviewer. Until someone answers, the status stays "proposed".
- **The designs are untested beyond the nonce scheme.** The wrap format, the checkpoint rules and the interaction of compaction with revocation cutoffs have no code and no simulator model. The compaction ADR in particular is where I would expect a reviewer to find something (it is the first design where the simulator would need an action "compact while a device is away").
- **The numbers in the ADRs are estimates:** 600 bytes per op for 10 members is arithmetic, not a measurement; 90 days, 500,000 ops and 1 GB are guesses to be replaced by the pilot.
- **ChaCha20-Poly1305 needs `golang.org/x/crypto`**, which is not in `go.mod` today. The design chooses it for the Raspberry Pi; add it only when the envelope is built.

## Your part

1. **Send the two ADRs to one person who knows cryptography or distributed systems** and ask the questions at the end of each. Record their answers in the ADR and change its status only then.
2. **Weekly update (draft, edit in your own words):** "Week 6: Offgrid nodes now enrol with invites, revoke lost devices (the rule is checked against every ordering of ops), talk over mutual TLS, and catch up in time that no longer grows with the log (2 ms for 10 new ops at 100,000). Still not done: a run on real devices (Checkpoint 1), an outside review. Looking for one small crew to try it on a real site."
3. **Pilot conversations:** ask each person what they do today when a job needs a record and the network is down, and how long they would keep a pilot going without help. Write down the answers before showing anything.
4. Open items from earlier days remain (Checkpoint 1 on real devices, `go get goleak`, the `main.go` lines).

## Next

Day 43: `offgrid hub`, a relay that stores signed ops per group and serves by cursor, with mutual TLS, per-group rate limits and a forged op refused by the receiving node. It is a real build day.
