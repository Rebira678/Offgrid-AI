# Day 39: Revocation and Authorize

The roadmap row: revocation with a cutoff, a pure `Authorize(state, op)`, an `op_void` table, a property test over permutations, and `store verify` listing void ops.

## What was built

- **`member.revoke`** (`internal/store/sqlstore/authorize.go`, `revoke.go`). Signed by an admin. It names the device and, for each replica the device signed on, a **cutoff**: the last (sequence, chain hash) of that chain the admin held. Ops the device signed up to the cutoff stay valid. Ops beyond it, or on a replica the cutoffs do not name, are **void**. A cutoff is a (sequence, hash), not a time, because a time can be backdated and a hash cannot (each op commits to everything before it). If the hash disagrees with the chain this replica holds, the device equivocated and none of that replica's ops count.
- **`Authorize(state, op)`**: a pure function of the membership state and the op. No membership founded: allowed (as before). Unsigned op: not attributable, not judged. Signer not enrolled: `ErrNotAuthorized`. Signer revoked and past its cutoff: `ErrRevoked` (wraps `ErrNotAuthorized`). `member.add` and `member.revoke` need an admin.
- **Void ops are kept, not refused.** The log must stay chained with no gaps, and the ops are evidence. They are stored, recorded in **`op_void`** (migration 7, append-only by trigger), and left out when the view is refolded. A replica that had already applied such an op refolds when the revocation arrives (`Rebuilt` is true). Ops from a device that was never enrolled are *refused* once the membership is known, and voided if they arrived before it was.
- **The current void set is recomputed from the membership ops every time**; `op_void` is a record, not an authority. `store verify` lists the void ops with reasons, and says so if a recorded op is no longer void (the membership changed under it).
- **One answer on every replica.** The fold is a pure function of the set of ops, with a fixpoint for the case where voiding an op changes who was an admin. `TestPermutationsOfARandomLogGiveTheSameMembershipAndTheSameVoidSet` shuffles 150 random logs 25 times each, comparing the membership, the void set and `Authorize` verdicts for 25 ops.
- **A flaw the tests found, and the rule that fixes it.** With only the cutoff rule, two admins revoking each other concurrently voided each other's revocation, so a compromised admin could undo its own removal. Now **an admin can only revoke an admin who was enrolled after it**. The enrollment order is fixed by the group's own log, not by anyone's clock (`TestACompromisedLaterAdminCannotCancelItsOwnRemovalByBackdatingARevoke`). Consequences: the founder can never be revoked (a stolen founder key means founding a new group), an admin cannot revoke itself, and a junior admin cannot remove a senior one.
- **Commands.** `offgrid store -db F revoke -device dev_...` (admin only; prints the cutoffs); `store members` marks `REVOKED`; `store verify` lists void ops.

## Verification

Tests first, then the code. Store: revoked device's later ops void on the admin and on a third replica (same state hash); refold on arrival; admin-only, not the founder, not a stranger, not twice; a revoked key never re-enrolls; a revoked admin's later enrollments are void while earlier ones stand; a stranger's ops refused, and void if they arrived before the membership; `op_void` is append-only and records each op once; bad cutoff hash; malformed cutoffs; the `Authorize` table; the two-admin cases; the permutation property. A refused revocation leaves nothing in the log. Syncer over HTTP: a stranger's push gets 403 `not_authorized`; a revoked device's sync succeeds and its work is void. CLI: the whole flow. Mutation checks on the new code; the survivors were the founder case inside the fold (equivalent: the rank rule already forbids it, the explicit case only gives a clearer message) and the cosmetic sorting of cutoffs (equivalent).

## Measured benchmarks (AMD Ryzen 5 PRO 5650U, 12 cores)

| Benchmark | Latency | Throughput / Cost |
|---|---|---|
| `BenchmarkAuthorize` | 29.8 ns/op | ~33.5M ops/sec (pure in-memory evaluation against cutoff vectors) |
| `BenchmarkAuthorize1000` | 35.0 µs/op | ~35 ns/op evaluating a batch of 1,000 ops sequentially |
| `BenchmarkRevoke` | 9.39 ms/op | Full transaction: multi-replica cutoff calculation, member.revoke op, and view refold |
| `BenchmarkRefoldView` | 1.88 ms/op | Complete atomic view recomputation skipping voided ops |

## Honest limits

- **Unsigned ops are not judged.** A revoked device that writes without a signature is not attributable. Run nodes with `-require-signed` once every device has a key. Without it, revocation does not stop unsigned writes.
- **The shared sync key is not rotated by a revocation.** A revoked device that still has it can keep connecting; its signed work is void but it can still read what the server sends. mTLS (Day 40) is what removes the connection itself.
- **Work lost.** What the device wrote after the cutoff and had not synced to the revoking node does not count. Re-enter it by hand if it matters. There is no "adopt" command.
- **A cutoff is only as good as what the admin held.** An admin that revokes before syncing with the device may cut off real work.
- **A void op's effect on other ops is not tracked**: if a revoked device created a task and a good device later edited it, the edit stays and the task does not exist; the edit is a no-op on a missing task, as for any edit of a missing task.
- **Who outranks whom is enrollment order**, so a compromised *senior* admin can remove everyone junior. That is the trust model: the first admins are the root.
- **Concurrent foundings of two separate groups** still resolve by the Day 38 tie-break; an op void under one may stop being void under the other. `store verify` reports it.
- Enrolling and revoking are still done with the CLI on the node holding the admin key; there is no phone flow.

## Your part

1. On two real devices: enroll the Pi (Day 38), write a task from it, sync, then on the laptop `offgrid store -db FILE revoke -device <Pi's dev_ id>`. Write another task on the Pi, sync, and check `store verify` on the laptop lists it as void and `exec list_tasks` does not show it.
2. Once all your devices have keys, start `sync serve`/`run` with `-require-signed`.
3. Checkpoint 1 (Day 37) is still open.
