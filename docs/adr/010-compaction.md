# ADR 010: Compaction, with a maximum time away

**Status:** proposed, 2026-11-16 (Day 42). **Design only.** The pilot measures log growth first; build only if the alarm below fires. Needs an outside reviewer before it is accepted.

## Context

The log only grows (Day 19, 23 and the article say so). A million ops is years of a small crew's work, so growth is probably not the first problem; but a log that cannot be trimmed is a promise that has to be re-examined. The danger is in how it is trimmed: tombstones and old ops exist so that a device that was away still merges correctly. Deleting them breaks a device that returns after a long absence, and breaks the checks that need old ops: per-op signatures, the per-replica hash chain, and revocation cutoffs (`member.revoke` names `{node, seq, hash}` of ops that may be old).

## Decision

**Snapshot plus signed checkpoint, and a declared maximum time away.**

1. **Checkpoint.** An admin signs: an epoch number, for every replica the cut `{node, seq, hash}`, the state hash of the view at the cut, the membership log head, the time, and the maximum time away (default **90 days**). A checkpoint is itself an op in the log, so it syncs like anything else.
2. **Safe to compact only when everyone has it.** A cut is valid only if every non-revoked member has acknowledged every replica's ops up to it (their summary, which `op_summary` makes cheap to read, is the acknowledgement), or has been silent longer than the maximum time away. Devices silent that long are declared lapsed in the checkpoint.
3. **Snapshot.** The view at the cut (tasks, notes, rules state, with the CRDT metadata needed to merge later ops), canonical and hashed. Ops at or below the cut are removed from `ops`, except what must be kept (4).
4. **Never compacted:** the membership ops (they are small and every authorization decision needs them), and per replica the `{seq, hash}` at the cut, which becomes the chain's new anchor so the next op's `prev` still verifies. Revocation cutoffs below a cut are checked against the checkpoint's recorded hash, not a removed op.
5. **A device away longer than the maximum time away** is refused on sync with `rebase_required` (a new code, like `upgrade_required`), told to export its unsynced local ops, take the snapshot and re-apply them as new ops. Its edits are not lost; they are redone against today's state and may conflict in the ordinary, visible way.
6. **Old signatures.** After a cut, ops below it can no longer be re-verified individually; the admin's signature on the checkpoint stands for them, and the checkpoint's state hash lets any node verify the snapshot it was given. The tradeoff is the same as Day 36's genesis attestation: old history is trusted as of the checkpoint.

## Rejected

- **Deleting tombstones on a timer.** Breaks any device away longer than the timer, silently. The design makes that case explicit and loud.
- **Per-device compaction.** Two devices that cut at different points can no longer agree on a state hash; a cut has to be a group fact.
- **Never compacting.** Acceptable for the pilot; rejected as a permanent answer only if the alarm fires.
- **A shorter maximum time away.** A site closed for a holiday and a phone left in a drawer are normal; 90 days is a guess to be replaced by what the pilot shows.

## What it costs

- A new admin action and a new op kind; a new way to be refused (`rebase_required`) that the person must understand.
- Verification gets weaker below the cut (see 6) and the simulator must learn the hardest action it has had: compact while someone is away.
- Time: the roadmap's estimate is two days to build, after a pilot measurement. This note is the cheap part.

## The alarm (what to measure in the pilot)

Write down, per site, weekly: ops in the log, database size, and the age of the oldest op still needed by every member. Build only if **ops pass 500,000, or the database passes 1 GB, or `store verify` or startup takes longer than 30 seconds** on the reference Pi. These numbers are guesses to be replaced by the first month of measurements.

## What would change it

The pilot shows growth is far slower than feared (do nothing) or a crew whose devices are away for months at a time (the maximum time away has to be longer, or `rebase_required` needs a gentler path).

## Review

**Not yet reviewed.** Questions for the reviewer: can a valid cut ever be signed while a member who has not acknowledged it is still active? Does anchoring the chain at the cut open a fork the current fork detection would miss? What happens to a `member.revoke` whose cutoff is below a cut?
