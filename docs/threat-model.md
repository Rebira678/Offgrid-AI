# Threat model v2

Written 2026-11-09 (Day 35), against `1.0.0-beta.1` plus the Day 34 work. Method: STRIDE over four parts (phone, node, relay, update path), against three attackers. Every threat is marked **mitigated**, **planned** or **accepted**:

- **mitigated**: code exists and a test or fuzz target I can name shows it. It says where the mitigation stops.
- **planned**: not done; the day it is meant to be done is given, or "not scheduled" if the roadmap has no day for it.
- **accepted**: we are not going to defend against it in the beta, and the docs say so. Each one says what would make us change our mind.

This file is meant to be argued with. A threat that is missing is a bug in this file; please open an issue (or, if it is a vulnerability, follow `SECURITY.md`).

## What is being protected

| Asset | Why it matters |
|---|---|
| The op log and the task view built from it | The work itself: tasks, notes, who did what. Integrity first (a wrong task is worse than a missing one), then confidentiality |
| Device keys (Day 34) | Each one is a device's identity. A copied key is that device |
| The sync key | Today the only thing that says "this peer is one of us" |
| The CA key (Day 33) | Lets its holder mint certificates for private names |
| The node's availability | The point of Offgrid is that it keeps working when nothing else does |
| The release binary | What the user actually runs |

## The parts

```
 phone (browser, PWA)  --HTTPS, private CA-->  site node  <--HMAC-signed HTTP-->  other node
                                                laptop or Pi: SQLite replica, sync server,           |
                                                dashboard on localhost only, optional local model    |
                                                      \--- (planned) relay: a server the site owner runs ---/
 update path: tag -> GitHub Actions build and test -> tarball + SHA256SUMS on a GitHub release -> user downloads
```

Not drawn because it does not exist: any self-update, any cloud service, any account system.

## The attackers

1. **A stranger on the WiFi.** On the same network, no key, no certificate. Can listen, send packets, run a rogue access point or a look-alike page.
2. **A stolen device.** A phone, laptop or Pi in someone else's hands, possibly unlocked, possibly with its disk read offline.
3. **A curious relay operator.** Runs the relay (a hosted one, or a friend's). Sees every byte that passes and can drop, delay or repeat messages. Does not hold device keys.

Two more matter in practice and are in the tables below: **a member who holds the sync key** (a crew member's laptop, a former employee) and **a web page in the user's own browser** (the dashboard on localhost is a target for DNS rebinding and cross-site requests).

## Threats

Status values are exactly `mitigated`, `planned` or `accepted`.

### Node (laptop or Pi: replica, sync server, dashboard, model)

| # | STRIDE | Threat | Attacker | Status | What exists, or what is planned, and where it stops |
|---|---|---|---|---|---|
| N1 | Spoofing | Someone pretends to be a peer and pushes ops | Stranger | mitigated | Every sync request carries an HMAC-SHA-256 over method, path, node, time and body; a wrong key changes nothing (`TestAWrongKeyIsRefusedAndNothingChanges`, `TestRequestsThatAreUnsignedTamperedOrStaleAreRefused`; `FuzzSyncAuth` checks that nothing is accepted without the right MAC over exactly those bytes). **Stops at**: whoever holds the key |
| N2 | Spoofing | A holder of the shared key pretends to be a specific other device | Member with the key | planned | The key is shared by every replica, so it identifies "us", not "which device". Device keys exist since Day 34 but are not yet used; signed ops are Day 36, the shared key is deprecated on Day 37 |
| N3 | Tampering | Ops edited in flight | Stranger | mitigated | The MAC covers the body; responses are signed too (`FuzzSyncRequestBody` checks a 2xx carries a valid signature). Not encrypted, see N8 |
| N4 | Tampering | The database file or an op edited on disk | Stolen device, local user | mitigated (detect only) | Hash chain per replica; `offgrid store verify` recomputes it and the view (`TestEveryOpIsChainedToTheOneBeforeFromTheSameReplica`, `scripts/tamper-demo.sh`). **Stops at**: it detects an edit, it does not prevent one, nobody is told unless `verify` is run (`doctor -deep` runs it), and an attacker who rewrites the whole chain from the edit onward is not caught by a local check. Signed ops (Day 36) are what close that |
| N5 | Tampering | A malformed or hostile op breaks the store | Member with the key | mitigated | Ops are validated before they touch the database; a refused op changes nothing and fails with a typed error. `FuzzApplyRemoteOp` found nothing in 10 minutes (about 407,000 inputs). **Stops at**: a *valid-looking* op from a key holder is accepted, which is N2 again |
| N6 | Tampering | Replay of a captured request | Stranger | accepted | The five-minute window limits how long a captured request can be replayed (`TestTheTimeWindowIncludesItsEdgeAndNothingBeyond`) and applying an op twice is a no-op, so the effect of a replay is nil. Request IDs and replay protection are Day 40 (the transport day). Would change if: a request ever has a non-idempotent effect |
| N7 | Repudiation | A device denies making an op, or nobody can tell who did | Member with the key | mitigated (when signing is on) | Since Day 36 every op written with a device key carries a signature over its chain hash (`TestSignedOpCarriesVerifiableSignature`), and `store verify` names the signing device. **Stops at**: signing is opt-in until Day 37; ops written before are unsigned and only attested ("trusted as of the migration"); which device belongs to which replica is trust on first use until Day 38 |
| N8 | Information disclosure | Sync traffic read on the network | Stranger | planned | Sync is signed, not encrypted (stated in the README). Mutual TLS 1.3 is the transport decision (ADR 006), Day 40 |
| N9 | Information disclosure | Another local user reads the database | Local user, stolen device with the disk read offline | mitigated (new databases only) | **Found in today's review**: a new database was created `-rw-r--r--`. Fixed: a new database is now created mode 0600 and its `-wal` and `-shm` follow (`TestNewDatabaseAndItsSidecarsAreOwnerOnly`). **Stops at**: databases made before today stay as they are (Offgrid does not change modes on files it did not create; `chmod 600` them), and a disk read offline is not stopped by file modes at all, see N10 |
| N10 | Information disclosure | The data and the keys are read from a stolen disk | Stolen device | accepted | Nothing is encrypted at rest: the database and `device.key` are plain files, and the sync key file's permissions are not even checked (found Day 34, to fix with the Day 36 work). Full-disk encryption is the user's defence. Would change if: a pilot site has devices that leave the building and no disk encryption |
| N11 | Information disclosure | Key bytes end up in a log or an error | Anyone who reads logs | mitigated | The device key is never printed, logged or marshalled, and errors do not repeat file contents (`TestNoKeyMaterialInAnyOutput`, `TestIdentityCommandNeverPrintsTheKey`). **Stops at**: the paths those tests cover; the sync key has no equivalent test (`LoadKey` says it never prints it, which is a claim, not a test); obs snapshots are written mode 0644 and I did not audit every field they hold |
| N12 | Denial of service | A stranger makes the node buffer large requests | Stranger | accepted | Headers are capped (16 KiB), timeouts are set, and a body is capped at 16 MiB. **Found in today's review**: the body is read *before* the signature is checked (the MAC needs the body), and there is no cap on concurrent requests, so a stranger can hold many 16 MiB buffers at once. Cheap fixes exist (refuse a request with a missing header or an out-of-window time before reading the body; cap concurrent requests). Accepted for a trusted network in the beta; do it before the node is reachable from anywhere but the site WiFi |
| N13 | Denial of service | A peer with a wrong clock cannot sync | Nobody (an accident) | mitigated | The error says by how much the clocks differ (`clock_skew`); `doctor` checks the clock and caught an 18-hour freeze on real hardware on Day 32 |
| N14 | Denial of service | Power loss corrupts the log | Nobody (an accident) | mitigated (partly) | `synchronous=FULL`; a 30-round kill -9 test lost nothing. **Stops at**: kill -9 is not a power cut; the real power-pull test is not done (Day 32 report) |
| N15 | Elevation | A web page makes the dashboard act, or reads it (DNS rebinding, cross-site requests) | Web page in the user's browser | mitigated | Listens on localhost only, refuses any `Host` that is not this machine, checks `Origin` and `Sec-Fetch-Site` on writes, caps bodies, sends CSP and `X-Frame-Options` (`TestEdgeDNSRebindingVariants`, `TestEveryResponseCarriesTheSecurityHeaders`, `TestLocalHostAndRequireLoopback`) |
| N16 | Elevation | Another user on the same machine uses the dashboard | Local user | accepted | The dashboard has no login (stated in the README); anyone who can reach `localhost` can use it. Would change if: a pilot machine is shared between people who must not see each other's tasks |
| N17 | Elevation | Text in a task makes the model run something it should not ("prompt injection") | A task author, anyone who can write a task | mitigated (as documented) | The model proposes and the code disposes: a closed menu of tools and every proposed command re-validated (README; `internal/tools`, `internal/intent`). I did not re-audit this today. **Stops at**: whatever is in the menu; a model can still propose a *wrong* menu item, which a person then has to refuse |

### Phone (a web page in a browser, on the site network)

| # | STRIDE | Threat | Attacker | Status | What exists, or what is planned, and where it stops |
|---|---|---|---|---|---|
| P1 | Spoofing | A rogue access point serves a look-alike page | Stranger | mitigated | The page is HTTPS from a private CA that is **name-constrained** to private addresses and `.local`, `.lan`, `.offgrid`, so a certificate for a bank cannot be made from it even with the CA key (`internal/pki` tests, including a forged certificate). Real Android Chrome showed no warning with the CA installed (Day 33). **Stops at**: a user who taps through a warning |
| P2 | Spoofing | The phone's CA is used to impersonate other sites | Stranger who steals `ca.key` | mitigated | Same name constraint; and the CA key is meant to live off the node. **Stops at**: private names on that site network |
| P3 | Tampering | Page or service worker replaced | Stranger | mitigated | HTTPS only, a service worker needs a secure context. **Stops at**: a compromised node serves a compromised page; no signed web bundle |
| P4 | Information disclosure | Unlocked or stolen phone: the cached tasks and the offline queue are read | Stolen device | accepted | IndexedDB is not encrypted by the app; the phone's screen lock is the defence. The device key is non-extractable (tested on a real phone), but code on the phone can still *use* it. Revocation (cut off a lost phone) is Day 39 |
| P5 | Information disclosure | A page script reads or sends data elsewhere | Stranger (XSS) | mitigated (for the spike page) | Content-Security-Policy with no inline script. **Stops at**: the spike page; the real app page has to keep the rule, and no test enforces that yet |
| P6 | Denial of service | The browser clears stored data under pressure, and unsynced edits are lost | Nobody (an accident) | planned | Real finding from Day 33: persistent storage came back as "not granted" on the test phone. Plan: request persistence, "add to home screen", warn when edits have been queued for too long. **Not scheduled** |
| P7 | Denial of service | The phone's clock is wrong, so sync is refused | Nobody (an accident) | mitigated | The page checks the clock against the node and says so (Day 33 check, 0 s on the test phone) |
| P8 | Repudiation | An edit made on a phone cannot be tied to a person | Anyone | planned | A key per device with a human label recorded at enrollment (Day 38); attribution is to a device, not a person (ADR 002) |
| P9 | any | iPhone: the install, the trust step and the storage rules are unverified | n/a | accepted | **iOS is untested.** The pilot targets Android and laptops until someone tests it |

### Relay (planned; nothing exists yet, so these are design threats)

| # | STRIDE | Threat | Attacker | Status | Notes |
|---|---|---|---|---|---|
| R1 | Information disclosure | The relay operator reads every task | Relay operator | accepted | The decision is a relay run by the site owner, over TLS, that can read data (decisions table). Whoever runs it can read tasks, and the docs must say so. Would change if: a hosted relay is offered to other people; then build envelope encryption first |
| R2 | Tampering | The relay forges or edits ops | Relay operator | mitigated (for signed ops) | An edited op or a swapped signature is refused with the op named (`TestForgedSignatureIsRefusedAndNamesTheOp`, `TestEditedOpIsRefusedAndNamesTheOp`); a relay can still drop ops, and can still forge an op for a replica that has never signed. **Stops at**: legacy unsigned history, and a replica whose first signed op has not arrived yet (trust on first use) |
| R3 | Tampering | The relay replays old ops or reorders them | Relay operator | planned | Ops are idempotent and chained, so replay is a no-op; replay protection on requests is Day 40 |
| R4 | Denial of service | The relay drops or delays ops | Relay operator | accepted | A hostile relay can always refuse service. Peers detect a gap (summaries and cursors) but cannot force delivery; direct LAN sync still works |
| R5 | Information disclosure | Metadata: who syncs when, how much | Relay operator, stranger | accepted | Visible to whoever sees the traffic; not hidden in v1 |
| R6 | Elevation | A TLS-terminating corporate proxy sits between peers | Network owner | planned | The reason phones use tokens over TLS and not client certificates (ADR 006) |

### Update path (how a binary reaches a user)

| # | STRIDE | Threat | Attacker | Status | Notes |
|---|---|---|---|---|---|
| U1 | Tampering | A release tarball is swapped or corrupted | Stranger on the download path | mitigated (against corruption) | HTTPS from GitHub plus a `SHA256SUMS` file in the same release. **Stops at**: the checksums come from the same place as the files, so they do not protect against a compromised release or account |
| U2 | Tampering | A compromised maintainer account or CI publishes a bad binary | Anyone who gets in | accepted | No release signature, actions are pinned by tag not by commit, and the release runs on tags. Accepted for the beta; revisit **before the pilot** (a signing step costs a day). Would change if: anyone outside the maintainer installs it for real work |
| U3 | Tampering | A vulnerable dependency or standard-library release ships | Any attacker | planned | `govulncheck` runs on push and weekly (Day 34); it has **not run yet**, the first run is the evidence. `go.sum` pins modules |
| U4 | Elevation | A malicious auto-update | Anyone | mitigated (by absence) | There is no self-update and no phone-home; a user installs by hand. This stops being true the day an update command is added, and that change needs its own review |
| U5 | Tampering | An upgrade loses or corrupts data | Nobody (an accident) | mitigated | A database written by the Day 30 program is checked in (`testdata/day30-golden.sqlite`); `TestGoldenDay30DatabaseMigratesToSignedSchema` migrates it and checks the view hash, every chain head and the chain are unchanged. **Stops at**: one golden file, not every past version |
| U6 | Information disclosure | A user posts a real database in a bug report | A well-meaning user | mitigated (by instruction) | The bug-report guide says not to; nothing technical stops it |

## What we will not defend against

Said plainly, because it is the useful half of a threat model:

- **A compromised device that holds valid keys.** If someone runs code as the user on an enrolled laptop, they are that device. Revocation limits the damage after the fact; nothing prevents it.
- **A stolen admin key.** The recovery is to re-found the group (decisions table: trust root). Until a second admin exists, losing the one admin key is unrecoverable.
- **Someone with the disk and no disk encryption.** Tasks and keys are plain files at rest.
- **A hostile relay operator reading tasks.** By decision.
- **Traffic analysis.** Who talks to whom, when, how much.
- **A shared machine with mutually distrusting users.** The dashboard has no login.
- **Anything on iOS**, until it is tested.

## Found while writing this

| Finding | Where it stands |
|---|---|
| New databases were created world-readable | **Fixed today** (N9), with a test |
| `sync once`, `sync summary` and `sync run` ignored an error computing the state hash and printed an empty one with exit 0; two such failures would look equal when comparing machines | **Fixed today** for `once` and `summary`, with tests; the `run` loop now prints `UNAVAILABLE (...)` (not covered by a test) |
| The sync server reads a body of up to 16 MiB before authenticating, with no cap on concurrent requests (N12) | **Not fixed**, recorded as accepted for the beta, with the cheap fixes named |
| The sync key file's permissions are not checked (N10) | Not fixed (Day 34 finding) |
| Existing databases keep their old mode (N9) | Not fixed; Offgrid does not change modes on files it did not create. A `doctor` warning is the right fix |
| A data race in a test (two goroutines incrementing one counter in `drain`), seen once under `-race` | **Fixed today** (test code only) |

## Summary

38 threats: **21 mitigated**, **7 planned**, **10 accepted** (three moved from planned on Day 36). Several "mitigated" entries say where the mitigation stops; read those before relying on one.

Review this file every Monday, and whenever a new way in is added (a relay, an update command, a new port).
