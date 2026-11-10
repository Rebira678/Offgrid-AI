# Day 35 (light): threat model v2, ADRs, and a review of the week

**Done when** (the roadmap's list), and where each stands:

| Criterion | State |
|---|---|
| Threat model with each threat marked mitigated, planned or accepted | Done: [`threat-model.md`](threat-model.md), 38 threats (18 mitigated, 10 planned, 10 accepted) over phone, node, relay and update path, against a stranger on the WiFi, a stolen device and a curious relay operator |
| ADRs merged | Done: [`adr/`](adr/README.md) 001 to 006, the first six rows of the decisions table, each with what would change it and where the implementation stands |
| Review the week for TODOs and swallowed errors; `go vet`, `staticcheck`, race detector | Done, below |
| Update posted | **Yours.** Drafts are in the chat (LinkedIn, X thread) |
| At least 3 conversations about a pilot | **Yours.** A cold invite and a follow-up for each Day 30 contact are drafted in the chat. I cannot send them or see replies |

## The review

- **TODO / FIXME**: none in the code.
- **Swallowed errors**: 69 `_ =` / `, _ :=` sites outside tests; I read the ones that are not `Close`, `Fprint`, `Rollback`, `Sync` or `Remove`. Almost all are fine (encoding a plain map cannot fail; a body that failed to write to a client has nowhere to report). **Two were real**:
  - `sync summary`, `sync once` and `sync run` ignored the error from computing the state hash and printed an empty hash with exit 0. The hash is what two machines compare to say they converged (the real-device report does exactly that), and two empty strings look equal. Now an error with exit 1 (`once`, `summary`), and `UNAVAILABLE (...)` in the `run` loop.
  - The `doctor` row count ignores its query error (an informational number; left).
- **Found by looking at a real file instead of the code**: a new database was created mode 0644, so any local user could read the tasks, and the `-wal` and `-shm` files inherit that. A new database is now created 0600. Existing databases are not touched.
- **`go vet`, `golangci-lint` (0 issues, which includes `staticcheck`), `go test -race`**: clean. The race detector reported one **data race in a test** on one full run (`drain`, two worker goroutines incrementing one plain counter); I could not make it happen again in isolation, but the cause is plain and the counters are now atomic. 25 consecutive `-race` runs of that test pass.

Tests first: `privatefile_test.go` (fails before: all three files 0644) and `statehash_test.go` (fails before: exit 0 and an empty `state hash:`). Mutation-checked: 5 deliberate breaks (ignoring the error in `once` and in `summary`, creating 0644, not calling the create, chmod-ing existing files), all caught. Not covered by a test: the `sync run` loop's new `UNAVAILABLE` text.

## Corrections to my earlier days

I had the roadmap's day numbers wrong in the Day 34 files and said so wrongly in the Day 34 summary. The truth: **Day 36 signed ops, Day 37 checkpoint 1, Day 38 enrollment, Day 39 revocation, Day 40 mutual TLS and replay protection.** I wrote "Day 35" for signed ops and "Days 36 to 38" for the transport. `SECURITY.md` and `docs/day-34.md` are corrected in this zip.

## What I found and did not fix

- The sync server reads a body of up to 16 MiB **before** it checks the signature, and has no cap on concurrent requests (threat N12). Cheap fixes are named in the threat model.
- The sync key file's permissions are not checked (N10).
- Existing databases keep the old mode (N9); a `doctor` warning is the right fix.

## Your part

1. `make test && make lint`, then push.
2. **Post the update.** Delete "week 7" from the LinkedIn and X drafts: I wrote it without a basis. Check every claim against what you saw.
3. Send the three-plus pilot messages and follow up with each Day 30 contact once. Replace `[Name]` and `[date]`.
4. Run `chmod 600` on any database you made before today (`~/*.db`, the demo databases).
5. Enable GitHub private vulnerability reporting (from Day 34), and merge the `identity` case into `main.go` if you have not.
