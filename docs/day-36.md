# Day 36: signed ops

> **Correction, found on Day 37.** The Day 36 zip signed ops and verified them in the store, but the syncer's own wire struct (`WireOp`) had no signature field, so **signed ops crossed the network unsigned** and the receiver stored them as legacy history. The Day 36 tests drove the store directly and never went through the syncer. Fixed on Day 37 (`TestSignaturesSurviveTheWire`, `TestTwoSignedReplicasSyncOverHTTP...`), found by running two real processes. Day 36 is only complete with the Day 37 zip applied.

**Done when** (the roadmap's list), and where each stands:

| Criterion | State |
|---|---|
| Forged or edited signature refused with the op named | Done: `ErrBadSignature` naming `node/seq` (`TestForgedSignatureIsRefusedAndNamesTheOp`, `TestEditedOpIsRefusedAndNamesTheOp`, `TestFirstBadSignatureInTheBatchIsTheOneNamed`) |
| Numbers recorded with hardware | Sandbox numbers below, labelled as such. **The Pi and laptop rows are yours.** |
| Migration test with a golden Day 30 database | Done: `testdata/day30-golden.sqlite` was written by the Day 30 program before any Day 36 change |
| Genesis attestation | Done as an attestation **op** (`offgrid store attest`), see below |

## What it is

- **Migration 5**: `op_sigs(node_id, seq, alg, device_id, sig)`, `device_keys(device_id, public_key)`, `node_devices(node_id, device_id)`, all append-only (triggers). `ops` does not change shape.
- **What is signed**: `"offgrid-opsig-v1\x00" + chain hash`. The chain hash already covers replica, sequence number, time, kind, payload and the previous op's hash, so a signature also vouches for the history before it. The prefix stops a signature from being replayed in any other use of the key.
- **Per op, not per batch**: a relay can forward any subset of ops in any grouping. Cost: 64 bytes and one verification per op. Wire form: `op.sig = {alg, device, sig, key?}`; the public key is sent once per device per batch and checked: the device ID must be the hash of the key.
- **On receive** (`ApplyRemote`): the chain hashes are computed from this replica's own copy of each replica's earlier ops, never taken from the sender; signatures are verified in a pool bounded by `GOMAXPROCS`, **before** anything is written or the clock moves; the inserts stay in the one single-writer transaction. When several ops are bad, the lowest-numbered one is named, whatever order the workers finish in.
- **Rules**: a replica is bound to the device of its first signed op (`ErrKeyChange` otherwise, also at `Open`); once a replica has signed, an unsigned op from it is refused (`ErrUnsigned`), locally too, so a lost key file cannot quietly turn signing off. `-require-signed` refuses all unsigned remote ops. An attestation without a signature is refused.
- **Algorithm tag** stored beside every signature (closes the gap noted in ADR 002).
- **CLI**: every command signs when `device.key` exists (`$OFFGRID_IDENTITY_KEY` or the default path). A damaged key (bad mode, corrupt) stops the command. No key: unsigned as before, and the sync commands print a warning. The syncer maps the new errors to 422 (`bad_signature`, `key_change`, `unsigned`).

## Old history: the genesis attestation

Nobody signed the old ops when they were written, and nothing can change that. `offgrid store attest` signs, as one op (`history.attested`), the chain head of every replica the database holds at that moment. `store verify` then reports those ops as **"trusted as of the migration, not proven"**, with who attested and when. It is valid only while the heads it names still match the stored chain, and it does **not** cover ops that arrive later. Ops with no attestation are listed as unsigned with a warning.

Deviation from the roadmap sentence ("the admin signs every replica's head at migration"): there is no admin until Day 38, so any device key can attest today. After enrollment only an admin's attestation should count; the report already shows who made each one.

## Numbers (sandbox: 2 vCPU Xeon, Go 1.24, temp-dir disk; NOT your hardware)

`go test -run '^$' -bench 'SignOp|VerifyOp|VerifyPool|Record|ApplyRemote1000' -benchmem -benchtime 2s ./internal/store/sqlstore`

| Benchmark | Sandbox | Laptop (EliteBook 845 G8) | Raspberry Pi |
|---|---|---|---|
| Sign one op | 31 µs | 34.1 µs | _fill in_ |
| Verify one op, one core | 66 µs | 74.7 µs | _fill in_ |
| Verify 1,000 ops, pool of GOMAXPROCS (2 here) | 45.6 ms | 16.9 ms (12 cores) | _fill in_ |
| Local write, unsigned / signed | 519 µs / 627 µs (+21%) | 1.11 ms / 1.18 ms (+6.9%) | _fill in_ |
| Apply 1,000 remote ops, unsigned / signed | 55.6 ms / 120.4 ms (2.2x) | 81.2 ms / 126.0 ms (1.55x) | _fill in_ |

Reading them: verification is about 66 µs per op, so a 100,000-op catch-up costs roughly 6.6 s of CPU, about 3.3 s on two cores. That is the "verification dominates catch-up" trigger in ADR 003 only on a slow device; the Pi row decides it. The fsync cost of `synchronous=FULL` on a real disk is larger than here, which makes the relative price of signing smaller, not larger.

## Honest limits

- **Trust on first use**: the first signed op of a replica binds it to that device. A stranger who reaches a replica first can claim it. Day 38 (enrollment) replaces this with the membership log.
- **Opt-in until Day 37**: no key, no signatures. The shared HMAC key still gates sync.
- **Old binaries** reject the new `sig` field (strict decoding) and the new op kind: upgrade both ends. This is a beta wire break.
- **`-require-signed`** cannot receive unsigned legacy history, so a new replica joining an old network cannot use it yet.
- A relay can still drop ops, and attestation is a statement about the past, not a proof of it.
- The early key-change check in `verifyFresh` is repeated at storage time, so removing it leaves behaviour the same (an equivalent mutant); the other 10 mutations were caught.
- `govulncheck` still cannot run here.

## Tests first, and checked

Written before the code and failing to compile/run: `sigs_test.go` (20 tests), the CLI `signed_test.go` (5). Two old tests had to change because they faked a version 4 database or asserted `schema version 4`: they now drop the three new tables and expect 5. Mutation-checked: 11 deliberate breaks (skip verification, no domain prefix, no key-change check, no unsigned-after-signed rule, ignore `-require-signed`, attestation accepts any head, report the last bad op instead of the first, ignore the algorithm tag, skip the device-ID check, drop the stored-unsigned rule, skip verification in `ApplyRemote`); 10 caught, 1 equivalent.

## Your part

1. `make test && make wasm-test && make lint`.
2. Run the benchmark command on the laptop and the Pi and fill the table.
3. `offgrid identity init` on each device (if not done), then `offgrid store -db FILE attest` on a database with history, and `offgrid store -db FILE verify` to see the report.
4. Old items still open: post the update (delete "week 7"), the pilot messages, `chmod 600` old databases, private vulnerability reporting, `*.key` in `.gitignore`, merge `main.go` lines.
