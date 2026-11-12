# Architecture decision records

One file per decision that is expensive to reverse: what was chosen, what was rejected, what it costs, and **what would make us change it**. The last part matters most: a decision with no stated way out is a belief.

| ADR | Decision | Implementation |
|---|---|---|
| [001](001-where-the-replica-lives.md) | Where the replica lives: a site node (full or light); phones are thin clients | node exists; Pi untested |
| [002](002-device-identity.md) | Device identity: ECDSA P-256, per device, never exported, algorithm-tagged | done on Day 34; signatures untagged, not yet used |
| [003](003-what-is-signed.md) | What is signed: each op's chain hash | Day 36 (implemented) |
| [004](004-authorization.md) | Authorization: admin-signed membership, grow-only revocation with a (sequence, hash) cutoff | Days 38 and 39 |
| [005](005-trust-root.md) | Trust root: a founder key; a second admin before the pilot | Day 38 |
| [006](006-transport.md) | Transport: mutual TLS 1.3 pinned from the membership log; phones use tokens | Day 40 |

## Decisions in the plan that have no record yet

Relay, storage engine, durability, log growth, attachments, phone web client, clocks on offline devices, history before signing, licence. The phone web client has the most new evidence (the Day 33 spike) and should be **ADR 007** next.

## Format

Status and date, Decision, Rejected, What it costs, What would change it, Evidence so far, Where the implementation stands. To change a decision, write a new ADR that says it supersedes the old one; do not edit history.
