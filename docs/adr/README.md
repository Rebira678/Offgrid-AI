# Architecture decision records

One file per decision that is expensive to reverse: what was chosen, what was rejected, what it costs, and **what would make us change it**. The last part matters most: a decision with no stated way out is a belief.

| ADR | Decision | Implementation |
|---|---|---|
| [001](001-where-the-replica-lives.md) | Where the replica lives: a site node (full or light); phones are thin clients | node exists; Pi untested |
| [002](002-device-identity.md) | Device identity: ECDSA P-256, per device, never exported, algorithm-tagged | done on Day 34; used for signing since Day 36 (algorithm stored per signature) |
| [003](003-what-is-signed.md) | What is signed: each op's chain hash | Day 36 (implemented) |
| [004](004-authorization.md) | Authorization: admin-signed membership, grow-only revocation with a (sequence, hash) cutoff | Day 38: membership and enrollment done; Day 39: revocation and `Authorize` |
| [005](005-trust-root.md) | Trust root: a founder key; a second admin before the pilot | Day 38 (founding and a second admin implemented; `store members` warns while there is one admin) |
| [006](006-transport.md) | Transport: mutual TLS 1.3 pinned from the membership log; phones use tokens | Day 40 (node-to-node done; phone tokens not) |
| [008](008-sync-protocol.md) | Sync protocol v2: version header and "upgrade required", MAC-chained frames, gzip, JSON kept, a maintained summary table | Day 41 (done; 007 is kept for the phone web client) |
| [009](009-envelope-encryption.md) | Envelope encryption for a hosted relay: a key per op, wrapped to each member | Day 42 (proposed, design only, not reviewed) |
| [010](010-compaction.md) | Compaction: snapshot plus signed checkpoint, with a maximum time away | Day 42 (proposed, design only, not reviewed) |
| [011](011-relay.md) | Relay: a hub run by the site owner that hosts one replica per group and can read, not forge | Day 43 (built; one hub, no Pi test) |
| [012](012-group-commit-and-driver.md) | Storage: one writer goroutine with group commit; stay on the cgo driver | Day 46 (built; real power pull and modernc numbers are the user's part) |
| [013](013-attachments.md) | Attachments: content-addressed blobs, only a reference in the log | Day 47 (built; fetching blobs from peers is Day 48) |

## Decisions in the plan that have no record yet

Storage engine, durability, log growth, attachments, phone web client, clocks on offline devices, history before signing, licence. The phone web client has the most new evidence (the Day 33 spike) and should be **ADR 007** next.

## Dependencies added after the beta

The rule is: no new dependency without a line here. One line each: what, why, what it costs, where it runs, what would remove it.

| Dependency | Added | Why | Where it runs | Would be removed if |
|---|---|---|---|---|
| `go.uber.org/goleak` v1.3.0 (MIT) | Day 37 | Fails a test run when a goroutine outlives its test and prints the stack that leaked. The roadmap names it for the sync tests, and a hand-made checker would be less trusted and less complete. | **Tests only**: it is imported from `_test.go` files, so it is not linked into the `offgrid` binary (check: `go version -m ./bin/offgrid`). It has no dependencies of its own beyond the standard library and its test helpers. | It stops being maintained, or a test needs a goroutine it cannot be told to ignore. |

## Format

Status and date, Decision, Rejected, What it costs, What would change it, Evidence so far, Where the implementation stands. To change a decision, write a new ADR that says it supersedes the old one; do not edit history.
