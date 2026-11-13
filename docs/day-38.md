# Day 38: Enrollment

The roadmap row: an admin makes a one-time invite; a new device presents it and is added by a signed `member.add` op; membership is a log of signed ops; a second admin before the pilot. **Done means:** a new device joins with an invite only once; expired or reused invites are refused; a second admin can be added; tests for each.

## What was built

- **Membership is a fold over signed ops** (`internal/store/sqlstore/members.go`). `member.add` carries the device ID, its public key, a role (`admin` or `member`) and a human label. The state is a pure function of the set of ops, ordered by (time, counter, node, seq), so every replica that holds the same ops agrees, in any arrival order (`TestFoldIsAFunctionOfTheSetNotTheOrder`, 300 shuffles). The first add is the **founding**: an admin adding itself. Later adds need an admin signer. A device is added once; roles are fixed (no promotion yet).
- **Enforced on arrival.** `ApplyRemote` refuses a `member.add` that the rules do not allow (`ErrNotAuthorized`), before the clock moves; `store verify` fails if a stored one no longer holds. A `member.add` or `history.attested` op without a signature is refused outright.
- **Invites** (`invites.go`, migration 6). A token is 15 random bytes (24 base32 characters): the first 8 are a public ID, the last 16 are an 80-bit secret. The database holds a random 16-byte salt and SHA-256(salt + token), never the token. Compared with `crypto/subtle`. Lifetime at most 24 h (15 min by default). **Single use is enforced twice**: redemption is one transaction that marks the row used only `WHERE redeemed_at IS NULL`, and a trigger refuses any later edit or delete. Eight goroutines racing for one invite: exactly one wins.
- **No oracle.** Unknown, wrong-secret, used and expired invites all return the same `invite_invalid` (HTTP 403) with the same text. A bad proof of key possession or an already-enrolled device does **not** burn the invite.
- **Proof and receipt.** The new device signs its request with its own key (it proves it holds the key it is enrolling). The admin node answers with a signed receipt; the device accepts it only if the admin key hashes to the **device ID pinned in the invite** and the receipt names this device, role and label (`CheckEnrollment`).
- **`POST /v1/enroll`** (`internal/syncer/enroll.go`): the one endpoint outside the shared-key HMAC, since a new device has none. 8 KiB body cap; 10 failures a minute per node, then 429 even for a valid invite. Only a node whose store can redeem (an admin's node) answers; others return 404.
- **Commands.** `offgrid store -db F found -label L`; `store invite -role member|admin -label L -ttl 15m` (prints a typed code, the admin's device ID and a one-string link); `store members` (and a membership line in `store verify`); `offgrid sync enroll -peer URL -invite LINK -label L` on the new device. Both `members` and `verify` **warn while there is only one admin**.

## Verification

Tests came first for each layer. Store: founding, adding, refusal of non-admin adds (also when they arrive by sync), unsigned adds, payload checks, duplicates, fold determinism, tie-breaks, `verify`. Invites: hashed at rest, TTL bounds, expiry boundary (refused at the deadline, accepted one second before), reuse, wrong tokens, races, second admin. Syncer over real HTTP: success then sync, no oracle, rate limit and its window, 404 without an enroller, oversize body, the device refusing a wrong admin, a replayed answer for another device, a mismatched admin field. CLI: the whole flow including a second admin, the missing-key/label/invite errors, a lying node. Mutation checks on the new code were run; two survivors in the syncer and two in the CLI were turned into tests and then killed. One CLI survivor remains: `sopt.Signer = sg` in `store`, which is equivalent because the store opener loads the device key itself.

## Honest limits

- **Membership does not yet stop a non-member from writing or syncing.** Only `member.add` is checked. The pure `Authorize(state, op)` for every op is Day 39, mutual TLS is Day 40. Today the shared key still gates the connection.
- **The founder is trust on first use.** Nothing proves who founded; you check the founder's device ID once (`found` prints it, the invite carries it as the pinned admin).
- **The invite has no certificate fingerprint yet** (the field is accepted and empty); it arrives with mTLS on Day 40. The enroll request is not encrypted, so a network observer can read the invite while it is redeemed. It is single use, so they could only race you; but they could win that race.
- **A stolen unexpired invite is a stolen enrollment.** That is the trade-off of the design; the answer is short life and single use.
- **Invites live on the node that made them** and are not synced; redeem at that node.
- **No QR image is generated.** The link is one string; `qrencode -t ANSI '<link>'` shows it. Adding a QR library is a dependency decision I did not make.
- **The rate limit is per node, not per address:** a guesser can make an honest enrollment wait up to a minute.
- **A stored add can become invalid** if an earlier-stamped op arrives later (an add is judged at its place in time order). `store verify` reports it; it is not repaired automatically.
- **Roles are fixed and there is no removal**: that is Day 39.
- Enrolling does not give the device the shared sync key. That still travels out of band until Day 40.

## Your part

1. Between your two real devices: `offgrid identity init` on both, then on the laptop `offgrid store -db FILE found -label laptop`, `store invite -label Pi`, start `sync serve`, and on the Pi `sync enroll -peer http://LAPTOP:7878 -invite '<link>' -label Pi`, then `sync once -create`. `store members` should list both on both.
2. Add a **second admin** (`invite -role admin`) and check the warning disappears. The roadmap asks for this before the pilot.
3. Check that the founder ID printed by `found` matches what you see on the laptop (`offgrid identity show`).
4. The Checkpoint 1 report from Day 37 is still open.
