# Day 40: Mutual TLS 1.3, request IDs, replay protection

The roadmap row: mTLS 1.3 between enrolled devices, each side pinning the other's key from the membership log; request IDs and replay protection; an unenrolled or revoked client cannot finish the handshake; timeouts on the server; one connection reused.

## What was built

- **Certificates from device keys** (`internal/identity/cert.go`). A device makes a self-signed certificate with its own key; there is no CA and no chain. `DeviceOfCertificate` accepts only a non-CA certificate for an ECDSA P-256 key, valid now, signed by that same key. `Fingerprint` is the full SHA-256 of the key, of which the device ID is the first half.
- **Pinning, not chains** (`internal/syncer/tlsconf.go`). Both sides skip the usual CA and host-name check and instead check the **key** in `VerifyPeerCertificate`: the server asks the membership log (`MembershipAuthorizer`: enrolled, not revoked, key matches); the client uses `PinDevice` (a device ID), `PinFingerprint`, `PinMembers` (its own log) or `AllOf`.
- **TLS 1.3 only**, a client certificate required, ALPN `h2`. A device that is not enrolled, is revoked, or has no certificate fails the handshake, and no request reaches a handler.
- **Revocation reaches open connections.** The authorizer is asked again on every request and caches an answer for 5 s (`AuthorizerTTL`), so a revoked device is cut off within 5 s even on a reused HTTP/2 connection.
- **One connection, many requests.** HTTP/2 with a pooled transport; a test counts server-side connections for a multi-request sync: 1.
- **Server timeouts** (`NewHTTPServer`): header, read, write and idle timeouts and a header size cap, all set. A test opens a valid TLS connection, starts a request and stops: the server drops it.
- **Request IDs and replay.** Every request carries a random 128-bit ID and the time. The server refuses a missing or malformed ID (400), a time outside the window (401), and an ID already seen (409 `replay`). The cache is bounded and, when full, refuses new IDs rather than forgetting old ones (503 `busy`).
- **Enrollment gets its own listener** (`sync serve -enroll-addr`): TLS 1.3, no client certificate (a new device has none anyone knows), serving only `/v1/enroll`. The new device pins the admin's device ID and the **fingerprint carried in the invite** before it sends the invite. This removes the chicken-and-egg: the first connection is pinned before any membership exists.
- **The shared key is off once a membership exists.** `sync serve` on a node with a membership serves mutual TLS only; `sync once` and `sync run` refuse an `http://` peer for such a node. A node with no membership still uses the shared key, with its deprecation warning, so the earlier days' tests and setups keep working.
- **Commands.** `sync serve -enroll-addr ADDR`; `sync once|run -peer https://HOST:PORT [-peer-device DEV]`; `sync enroll -peer https://HOST:ENROLLPORT -invite LINK -label NAME` (plain `http://` is refused); `store invite` now prints a link with the node's fingerprint.

## Verification

Tests first at each layer. Identity: certificate round trip, the fingerprint, expired, not yet valid, wrong signer, wrong curve, CA certificate, bad validity. Syncer: sync over TLS 1.3 and HTTP/2 with no shared key; one connection for many requests; unenrolled and certless clients; TLS 1.2 and 1.1 refused even from an enrolled device; revocation on an open connection; wrong pin; pinning by the membership log; replayed, missing, malformed and stale request IDs; the bounded cache; timeouts and a slow client; the enrollment listener with a wrong device pin, a wrong fingerprint, the right pins, and that it serves nothing else; the sync listener not serving enrollment. CLI: the whole flow over TLS including a second admin and a revoked device being cut off, an enrollment answered with another admin's receipt, a server holding the wrong key, `http://` refused, HMAC refused for a group with a membership, a first sync needing `-peer-device`. Mutation checks on the new code: the survivors are equivalent (Go itself already refuses a missing client certificate when one is required, and the certificate cache's first-use check is unreachable because it is filled at start-up). A live probe against a real listener: `openssl s_client -tls1_2` gets "protocol version"; `-tls1_3` with no client certificate gets "certificate required" with ALPN `h2`; plain `http://` to the TLS port is answered 400.

One test from Day 37 (`TestTwoProcessesWithTheirOwnKeysSyncSignedOpsAndSampleThemselves`) failed once in the full suite: it read the server's stderr right after the first stdout line, and the two pipes are not ordered. It now waits up to 3 s for the warning; it passed five repeats in a row.

## Honest limits

- **A group with no membership is unchanged**: shared key, not encrypted. Found a membership to turn mutual TLS on.
- **The enrollment request depends on the invite's pin.** The invite is read by the new device from a channel you chose; if someone reads the link they can enroll first (Day 38's limit), but they can no longer read it from the network, and a node that is not the admin cannot take it, because the new device checks the admin's key before sending it.
- **Up to 5 seconds of grace** after a revocation.
- **The replay cache is in memory.** After a restart, a request recorded within the last window can be replayed once if the attacker also breaks TLS; recorded TLS traffic itself cannot be replayed.
- **Certificates are minted on demand and last 24 hours.** There is nothing to renew by hand, and nothing to revoke: the membership log is the only authority, and a certificate is only an envelope for a key.
- **Phones are not on this path.** They still reach the node through the Day 33 PWA, and the node signs for them. A phone token flow is still to do.
- **Clocks matter.** A certificate must be valid and a request fresh by the local clock; a device whose clock is far off fails the handshake with a certificate error rather than the clear "clock skew" message that the HMAC path gave.
- The `http://` legacy path is still there and unencrypted for groups with no membership. No flag was added that turns mutual TLS off for a group that has a membership.

## Your part

1. On the laptop, with a membership founded: `offgrid sync serve -db FILE -addr 0.0.0.0:7878 -enroll-addr 0.0.0.0:7879`, and make an invite (`store invite`). On the Pi: `sync enroll -peer https://LAPTOP:7879 -invite '<link>' -label Pi`, then `sync once -db FILE -create -peer https://LAPTOP:7878 -peer-device <admin id>`. Stop `-enroll-addr` (restart serve without it) once you are done enrolling.
2. Revoke the Pi from the laptop and check that its next `sync once` fails with a certificate error within a few seconds.
3. If your phone or any device has a wrong clock, expect a certificate error: fix the clock first.
4. Checkpoint 1 is still open, and the earlier Day 37 steps.
