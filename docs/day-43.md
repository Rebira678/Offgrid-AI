# Day 43: `offgrid hub`, a relay the site owner runs

The roadmap row: a server run by the site owner that speaks mutual TLS, stores signed ops per group and serves them by cursor; reads data (v1) but cannot forge any op. One binary, one small SQLite file; per-group rate limits; `signal.NotifyContext`; `http.MaxBytesReader` on every body. Done when two sites on different networks sync only through the hub, and a forged op is refused by the receiving node, not just by the hub.

## What was built

- **`internal/hub`.** A hub hosts one ordinary replica per group (its own SQLite file in the hub's folder, plus `groups.json`) behind `/g/GROUP/...`. It is the existing sync handler, mounted per group, so nodes need no new client: `-peer https://HOST:PORT/g/crew -peer-device HUB_ID`.
- **Founder pinning.** `hub group add -group crew -founder dev_...`. Until the group has members, only that device can reach it (otherwise whoever connected first could found it). After that, the group's membership log decides, with the existing revocation cache.
- **Same answer for "not there" and "not yours"** (404 `no_such_group`), so a member of one group cannot probe for another; a stranger cannot finish the TLS handshake.
- **Per-group rate limit** (token bucket, default 20/s, burst 40, `-rate`, `-burst`): 429 with Retry-After, which `sync run`'s back-off from Day 41 already honours. Each group has its own budget.
- **Bounded bodies:** a declared length over `-max-body` is a 413 before anything is read; a chunked body is cut by `http.MaxBytesReader` and is a 413 too.
- **Signed ops only.** The hub's replicas run with `RequireSigned`, so an unsigned op or a tampered one is refused by the hub; this is the replica's own rule, not a second one.
- **`offgrid hub serve | group add | group list`**, with graceful shutdown on SIGINT/SIGTERM and a re-read of the group list on SIGHUP.

## Verification

- *Two sites meet only through the hub* (`TestTwoSitesMeetOnlyThroughTheHub`): neither listens on anything; a member the hub has not heard of is refused until the founder has pushed; then the two sites exchange tasks through the hub and agree on the state hash.
- *Forged ops are refused by the receiving node* (`TestAForgedOpIsRefusedByTheReceivingNodeNotJustTheHub`): the real hub is made dishonest by a transport that (1) alters a task title in the answer and (2) invents an op from a device that was never enrolled. The receiving node refuses both with nothing applied, and recovers from the honest hub. The hub's own checks are not involved, which is the point.
- *The hub refuses what it can judge* (`TestTheHubRefusesAForgedOrUnsignedPush`): a push altered in flight and an unsigned op are refused, and the hub stores nothing.
- Founder pinning (and after a restart), group isolation, the rate limit with refill and per-group budgets, body limits (declared and chunked), revocation cutting a device off at the hub, name and limit validation, and the CLI (serve on `:0`, a group added while it runs and picked up on SIGHUP, shutdown).
- Mutation checks: founder rule, group isolation, handshake authorizer, rate limit, both body checks (the declared-length check is equivalent in outcome, see below), `RequireSigned`, the path rewrite, the registry write, the limiter's cap and its clock-step rule, SIGHUP reload. One survivor, equivalent: removing the `ContentLength` pre-check gives the same 413, because `MaxBytesReader` catches it at the first read; the pre-check only saves reading.
- Full suite with `-race`: 33 packages ok on the final run. golangci-lint on a copy: 0 issues; `make wasm` ok; gofmt and vet clean. One unrelated test, `TestAgentRulesAnswerWithoutAnyModel`, failed once under load because it asserted the timing is shown in microseconds (` µs]`); it now accepts µs or ms (`cmd/offgrid/agent_test.go` is in the zip for that).

## Honest limits

- **The operator can read everything** (ADR 011, threat R1). The help text says so; so should anything you tell a crew.
- **Not run across two real networks.** The test is one process with simulated sites; TLS, ports and clocks are real, networks are not. Nothing here has run on the Pi.
- **Connections from non-members cost a handshake** and are not rate limited (threat N27 says where the limit stops). Put a firewall or a connection limit in front of a hub on the internet.
- **`x/time/rate` was not used** (ADR 011 says why: stdlib token bucket with an injectable clock, no `go.mod` change).
- **One file per group, not one file.** The roadmap said one small SQLite file; reusing the replica per group kept one set of rules. The hub's folder is the unit to back up.
- **Enrollment does not go through the hub.** A new device still enrols with an admin's node (`-enroll-addr`), then syncs through the hub.
- **Adding a group while the hub runs needs SIGHUP**, and two `group add` at once are not coordinated.
- **`main.go`:** the new command needs one case in your `main.go` (below). Without it, `offgrid hub` is "unknown command"; the tests call the command directly and pass without it.

## Your part

1. Merge the zip, then add to the `switch` in `cmd/offgrid/main.go`, next to `case "identity"`:
   ```go
   case "hub":
   	return cmdHub(args[1:], stdout, stderr)
   ```
   (plus the `ca` and `identity` cases and usage lines from earlier days, if you have not yet).
2. Try it on two machines: on the hub machine `offgrid identity init`, `offgrid hub group add -dir ~/hub -group crew -founder DEVICE_ID_OF_YOUR_FOUNDER_NODE`, `offgrid hub serve -dir ~/hub -addr :8443`; on the founder's node `offgrid sync once -peer https://HUB:8443/g/crew -peer-device HUB_DEVICE_ID`; then a second device (enrolled as before) does the same. Check the clocks (TLS certificate time is checked within a window).
3. Tell whoever runs the hub that it can read the tasks.
4. Open items from earlier days remain (Checkpoint 1 on real devices, `go get goleak`, the Day 41 migration warning, ADR review).

## Next

Day 44: Checkpoint 2. Three devices and a relay; revoke one and watch it refused; add hostile actions (forged op, revoked sender past its cutoff, replayed request) to the seeded simulator; 3,000 schedules pass with the new rules. It is a gate, not a feature, and it has a cut point: if behind, drop the relay and stay peer to peer.
