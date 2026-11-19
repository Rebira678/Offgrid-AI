# Day 44: Checkpoint 2, three devices under attack

The roadmap row: three devices (and a relay); revoke one and watch it refused; add hostile actions to the seeded simulator (a forged op, a revoked sender past its cutoff, a replayed request); the gate is that security properties are tested under failure, not only on the happy path. Done when 3,000 schedules pass with the new rules, revoked and forged ops are refused in every schedule, and a report is written. This is that report.

## What was built

- **A `Secure` mode for the simulator** (`internal/sim/hostile.go`, `Config{Secure: true}`). The replicas are signed (`RequireSigned`), r0 founds the group and enrols the others. Plain schedules are generated exactly as before; secure ones add four action kinds:
  - `revoke`: r0 revokes one device (exactly one per schedule, at a random point).
  - `forge`: five variants, one of each in every schedule: an honest op with its payload altered; an op from a device nobody enrolled; an honest op wearing a stranger's signature; an enrolled device's op with one bit of the signature flipped; an unsigned op. The receiver must refuse it with an error and leave its log, summary and state hash exactly as they were.
  - `rogue`: the revoked device keeps creating work and a peer pulls it.
  - `replay`: a batch that was delivered earlier is delivered again, before and after the revocation. If the receiver holds all of it, nothing may change and nothing may fail.
- **New checks at the end of every schedule** (`checkHostile`): nothing from a forger is in any log; `Verify` is clean and every op is signed; the revoked device shows as revoked in the converged membership; **on every replica the void set is exactly the revoked device's ops past its cutoff** (not more, not fewer); no task created by void work is visible. The old convergence checks, the oracle and the fresh-replica witness still run.
- **A failing seed still replays exactly.** The schedule is a list of printed actions that `ParseAction` reads back; device ids in the trace were different on every run (keys cannot be seeded), so they are now masked as `dev#1`, `dev#2`... in traces and failure text. `TestAHostileScheduleReplaysExactlyFromItsSeed` runs seeds twice and from their parsed text and compares traces and hashes.

## What the simulation found (two real bugs, both fixed)

1. **Honest work built on void work was refused as "corrupt".** A device that had not heard of a revocation noted, changed, suggested on or deleted a task created by the revoked device past its cutoff. After the revocation arrived, that task was void and skipped, and the honest ops that referred to it made `applyOp` fail with "refers to task ..., which does not exist". Every replica holding them then refused to sync (ErrBadOp): anyone who could get revoked at the wrong moment could stall a group. Now an op whose referent is void (a task, a note, a suggestion) stays in the log and the chain, has no effect, and is not itself void (`internal/store/sqlstore/dangling.go`); a referent that is missing for any other reason is still corruption. Test first: `TestWorkBuiltOnAVoidTaskIsInertAndDoesNotBlockSync` (failed with the exact error above).
2. **A revoked device that knew it was revoked could keep writing, and its own view then differed from everyone else's.** Its ops went void on every other replica but stayed in its own view. Now a local write by a device that holds its own revocation is refused with `ErrRevoked`; a device that has not heard yet still writes (it cannot know), and refolds when the news arrives. `TestADeviceThatKnowsItIsRevokedCannotWriteAndOneThatDoesNotStillCan`.

Three simulator mistakes were fixed on the way (a witness that treated "not enrolled yet" as fatal, an oracle that counted honest ops on void tasks, a rogue action that treated clock-drift refusals as failures).

## Result

3,000 schedules (seeds 1 to 3,000, 80 steps, 3 replicas), all pass, about 195 s on 4 cores. Totals: 3,000 revocations; 22,483 forgeries tried, 20,676 refused, 1,807 "ignored" (see limits), **0 accepted**; 12,623 rogue actions; 13,467 replays (11,847 of a batch already fully held); 24,641 void ops ended up in logs. The test asserts coverage **per schedule**, not in total: exactly one revocation, all five forge variants tried, every forgery refused or ignored, at least two refused, at least two replays, at least one rogue action.

Mutations the simulation kills (each applied to the store, the simulation run on 60 seeds): signature verification disabled (killed only after the "damaged signature" variant was added: the first four variants were also caught by the chain and enrolment checks, so the signature check alone had no test); authorization of unenrolled signers disabled; void set emptied; the revoked-write guard removed; the dangling-reference rule removed or inverted (store tests).

## Honest limits

- **No relay in this checkpoint.** The cut point allowed it: the simulation is peer to peer at the store level. The hub's own security (founder pinning, group isolation, revocation cutting a device off, forged ops refused by the receiving node) is tested in Day 43's `internal/hub` tests, not under random schedules.
- **Request-level replay is not in the simulation.** Replaying a captured signed *request* is stopped by Day 40's nonce store and tested there; the simulation replays *batches of ops* at the store.
- **"Ignored" forgeries (8%).** When the forged op is one the replica already holds, it is recognised by place and hash as a duplicate; the signature is not rechecked and nothing is stored. That is harmless but it is not a refusal; the test counts it separately and requires state to be unchanged.
- **A revoked device that does not know it is revoked still writes**, and peers that have not heard take its work in and drop it later. That is inherent to being offline; the bound is "until the revocation reaches them".
- **The oracle and the store share the void rule** (inert dependents). If the rule is wrong, both are. The rule is stated in `dangling.go` and in the test names.
- **Keys are random per run**, so only ids differ between runs of one seed and they are masked; a failing seed's messages are exact, the key bytes are not.
- Three replicas, 80 steps: it does not explore long histories or four-way revocation races (the membership fold has its own permutation tests, Day 39).
- Not run on real devices; Checkpoint 1's real-device run is still yours to do.

## Your part

1. Merge the zip; `make test && make wasm-test && make lint`. The 3,000-schedule test takes about 3 minutes (about 30 s with `-short`, which runs 300).
2. To replay a failing seed: `sim.Run(SEED, sim.Config{Secure: true, Steps: 80})`; the test prints it.
3. Still open from before: Checkpoint 1 real-device run and report, `go get go.uber.org/goleak@v1.3.0 && go mod tidy`, the `main.go` cases (`ca`, `identity`, `hub`), Day 41 migration (upgrade all devices together, `offgrid store verify` once), ADR 009/010 outside review.
