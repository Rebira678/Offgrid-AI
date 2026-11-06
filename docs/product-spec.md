# Offgrid product spec (v1.0 beta)

One page. It says who this is for, what it must do, what it will not do, and how we will know it works. Numbers marked **(set)** are targets to fill in from measurements on real devices (Day 32) before they count.

## Who it is for

Small crews working on a **site with a local network but no dependable internet**: a depot, farm, ship, clinic, mine camp. One person (the site lead) sets it up; others use it from a phone or laptop on the site network.

## The job to be done

"When the internet is down, my crew can still record and update work, hard requests are not lost, and when everything reconnects we all see the same list, without anyone having to fix it by hand."

## Three must-have flows

1. **Record work offline.** Add, edit and finish tasks and notes on any device on the site network. Easy requests ("new task: check the fuel line, site 2, high") are understood on the device, with no cloud.
2. **Reconnect and converge.** Devices that edited apart merge automatically and end with the same data. Nothing the person was told was saved is lost.
3. **Review AI suggestions.** A local model proposes changes next to the task. A person accepts or dismisses. A suggestion made stale by a human edit is refused.

## What v1 will not do

- Native phone apps; a phone out of range can only queue edits.
- Windows or macOS as supported nodes (Linux only until tested).
- Multi-company hosting, billing, or any data types beyond tasks, notes and photos.
- Hide data from the person who runs the relay. Signed, not secret.

## Service levels (what "works" means)

| Area | Target | How it is checked |
|---|---|---|
| Durability | Zero acknowledged writes lost across `kill -9` and a power pull | Crash and power-pull tests, 72-hour soak |
| Convergence | 100% of soak runs and simulator schedules end with identical state hashes on all devices | Seeded simulator, real-device soak |
| Offline use | Every must-have flow works with the network off | Flow tests with the connection cut |
| Catching up | A reconnected device catches up within **(set)** seconds for 10 new ops on the site network | Benchmark and real-device test (Day 32, Day 41) |
| Joining | A new person goes from install to first sync in 30 minutes or less, from the docs alone | Trial with someone who did not build it |
| Memory | Light node within **(set)** MiB, full node within **(set)** MiB, flat over 24 hours | Soak with named hardware |
| Upgrades | Upgrading from every earlier release keeps all data | Golden-database tests |
| Integrity | An edited log is detected and the first bad op is named | `store verify`, tamper tests |

Baseline already measured, **in a cloud sandbox, not on real hardware:** about 860 durable appends per second; a new replica caught up on 10,000 ops in about 2.3 s. Treat these as placeholders until Day 32 and Day 45 replace them.

## How we will know the pilot worked

- At least one real person uses it on their own devices for 7 days or more.
- They keep using it without being reminded, and can say what it saved them.
- Zero data-loss bugs and zero divergence found during the pilot.
- Every problem they report becomes an issue within a day.

## Assumptions to test, not believe

1. A site network exists and is usable (not just a phone with no signal).
2. A private certificate installed on Android phones works for the web app (spike, Day 33).
3. A cheap node (Pi or old PC) is reliable enough with an SSD or a good card and a correct clock (Day 32).
4. Photos matter more than any other missing feature (watch the pilot).
5. People who run sites will trust a small open-source project if its limits are written down.

If an assumption fails, the plan changes; the spec does not get defended.
