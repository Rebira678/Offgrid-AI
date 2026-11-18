# ADR 011: The relay is a hub that hosts replicas

**Status:** accepted, 2026-11-17 (Day 43 of the Days 31 to 60 plan)

## Decision

A **hub** run by the site owner: `offgrid hub serve`. It hosts one ordinary replica per group (one SQLite file each) behind `https://HOST/g/GROUP/`, speaks mutual TLS 1.3, and nodes sync with it with the same client, frames and rules they use with each other (`-peer https://HOST/g/GROUP -peer-device HUB_DEVICE_ID`). Two sites that cannot reach each other both reach the hub and meet there. **The hub can read every task and cannot forge one**: it holds no member's key, every op is signed, and each node checks signatures, chains and membership itself, so a hub that lies is caught by the node it lies to.

- **Who may use a group:** its members, once it has any. Until its founding op arrives, only the **founder's device ID named when the operator added the group**; otherwise whoever connected first could found it.
- **A group that is missing and one that is not yours look the same** (404, same body), so a member of one group cannot probe for another.
- **Limits:** a per-group token bucket (default 20 requests per second, burst 40) answering 429 with Retry-After, which the client's back-off already honours; every body bounded (`http.MaxBytesReader`, default 16 MiB); at most 64 groups; the syncer's server timeouts; `signal.NotifyContext` for shutdown.
- **What it refuses on its own account:** unsigned ops, a tampered op, a signer who is not a member or is revoked past their cutoff, and a chain that does not fit. This is the existing replica logic, so there is one rule, not two.

## Rejected

- **A clever blind relay** (it carries ciphertext and holds nothing readable). That is ADR 009, designed and not built: it needs a key protocol and a new wire format. The simple hub can be built in a day and is honest about what it is. The cost is stated below.
- **One shared database with a group column.** It would mean new tables and a new set of rules that must match the replica's. One replica per group reuses the code that already carries the security properties, at the price of one file per group.
- **The hub holding the membership as a root of trust.** The hub is only a replica of the membership log, as every node is.
- **`golang.org/x/time/rate`.** The hub needs a token bucket and nothing else; 30 lines against an injected clock let the tests move time without sleeping, and `go.mod` does not grow. `rate.Limiter` is the drop-in if reservations or contexts are ever needed.

## What it costs

- **The operator reads the data.** Whoever runs the hub, or has its disk, can read every task and note in every group it hosts. The docs and the `hub` help say so. If the owner of a site is not the person to trust with that, use ADR 009's design or no hub.
- **A hub is a place that must be kept up.** It can withhold, delay and drop ops (threat R4); nodes still sync peer to peer when they can reach each other.
- **A new thing to run and back up:** one directory of SQLite files and `groups.json`; losing it loses nothing the nodes do not also hold, but a group has to be re-added and pushed again.
- **Adding a group while the hub runs** needs a SIGHUP (or restart); two processes writing `groups.json` at the same moment are not coordinated.

## What would change it

A pilot that needs a hub run by someone other than the people whose data it is (build ADR 009); more than a few dozen groups on one hub (a shared store and real accounting); a device that must reach the hub from behind NAT without inbound connections (a different shape: long polling is already what `sync run` does, so probably nothing).

## Where the implementation stands

**Built on Day 43** (`internal/hub`, `offgrid hub`). Tested in one process with simulated sites; not run across two real networks or on the Pi.
