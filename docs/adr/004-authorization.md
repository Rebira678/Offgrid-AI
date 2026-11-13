# ADR 004: Authorization

**Status:** accepted, 2026-11-09 (from the decisions table of the Days 31 to 60 plan)

**Implementation (Day 38):** membership is a log of signed `member.add` ops, folded by a pure function; adds are enforced when ops arrive. Revocation and the pure `Authorize(state, op)` are Day 39.

## Decision

**Membership is admin-signed enroll ops plus a grow-only set of revoked keys.** Roles are fixed at enrollment. A revoke op carries the **cutoff**: the last sequence number and hash the admin saw from that device.

## Rejected

Timestamp-based revocation; a central auth server; editing roles in place.

## What it costs

Unsynced work from a revoked device beyond the cutoff is lost unless an admin re-submits it. A new role means a new enrollment. Losing every admin key means re-founding the group.

## What would change it

Pilot users need frequent role changes. Then add admin-signed role ops with last-writer-wins.

## Evidence so far, and what it changes

- Why a (sequence, hash) cutoff and not a time: a timestamp can be backdated, a cutoff cannot, because each op commits to everything before it. Validity depends only on the set of ops, so every replica reaches the same answer in any order.
- Revoked keys never come back; re-enroll with a new key.
- A property test (any permutation of the same op set gives the same valid set) is what would show this is right.

## Where the implementation stands

Not started. Enrollment is Day 38, revocation and `Authorize` Day 39.
