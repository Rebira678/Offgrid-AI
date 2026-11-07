# Real-device report

**Status: PARTIAL RUN.** Real hardware sync (smoke test) and physical network partition recovery were verified across two physical machines over a local Wi-Fi LAN. Power-pull and cold-boot clock tests on real hardware have not yet been run.

Protocol: [`docs/day-32.md`](day-32.md). Scripts: `scripts/real-device/`.

## Hardware

| | Laptop (Node 1) | Second machine (Node 2) |
|---|---|---|
| Model | HP EliteBook 845 G8 Notebook PC | Dell desktop (`DESKTOP-932K097`) |
| CPU, RAM | AMD Ryzen 5 PRO 5650U (6c/12t), 15.0 GiB RAM | Intel Core i5 x86_64, 8 GiB+ RAM |
| Storage | NVMe SSD (213.1 GiB free) | Virtual disk (WSL2 VHDX on Windows SSD) |
| OS | Zorin OS 17 (Ubuntu 22.04 LTS base) | Windows host, WSL2 Ubuntu 22.04 LTS |
| Architecture | amd64 | amd64 |
| Hardware clock with battery? | RTC device present; battery not verified | unknown (WSL2 VM; Windows owns clock, cache, power) |
| Network | Wi-Fi LAN (10.240.72.88, subnet 10.240.72.0/24) | Wi-Fi LAN (same 10.240.72.x subnet) |
| `offgrid version` | offgrid 1.0.0-beta.1 | offgrid 1.0.0-beta.1 |
| Date of the runs | 2026-11-06 / 2026-11-07 | 2026-11-06 / 2026-11-07 |

## `offgrid doctor -deep`

### Laptop (Node 1)
```
offgrid doctor (changes nothing)
  PASS  version   offgrid 1.0.0-beta.1, go1.26.5 linux/amd64, sqlite 3.45.1
  PASS  clock     2026-11-07T08:14:37Z, not behind the newest edit in the log
  PASS  disk      213.1 GiB free in .
  PASS  port      127.0.0.1:7878 is free
  PASS  port      127.0.0.1:8787 is free
  PASS  database  SQLite integrity check passed (5 ops)
  PASS  chain     hash chain intact over 5 ops
  PASS  view      the task view matches the log (5 ops, hash c6d25e697da6)

8 passed, 0 warnings, 0 failed
```

### Second machine (Node 2, Dell desktop with Windows / WSL2)
```
offgrid doctor (changes nothing)
  PASS  version   offgrid 1.0.0-beta.1, go1.26.5 linux/amd64, sqlite 3.45.1
  WARN  clock     the system clock is not synchronized to a time server
        fix: expected on an offline site, but then use a battery-backed clock or one node that serves time to the others
  PASS  disk      free disk space verified in WSL2
  PASS  port      127.0.0.1:7878 is free
  PASS  port      127.0.0.1:8787 is free

4 passed, 1 warnings, 0 failed
```

*Note on cold-boot clock drift caught on Node 2:*
Before manual clock synchronization, `offgrid doctor` caught an 18-hour VM clock freeze resulting from system sleep:
`FAIL clock: the clock says 2026-11-06T18:57:42Z, but this program was installed on 2026-11-07...`
Once updated with `sudo date -s`, the hard failure cleared to the expected edge-site `WARN` (no time server active in WSL2).

## Results

| Test | What was run | Result | Numbers | Notes and surprises |
|---|---|---|---|---|
| A. Sync over real WiFi | Bidirectional sync over local Wi-Fi LAN (`10.240.72.88:7878`) | **PASS (smoke test)**; 5,000 bulk op test **not run** | pulled 1, pushed 1; hashes equal: `c6d25e697da62f8a17a27e738a660ed7d1bc4f3e34a07075a9221a3271c2f342` | First time Offgrid synced across two physical machines over Wi-Fi. 5,000 bulk op benchmark with timing still open. |
| B. Network partition | Desktop Wi-Fi toggled off, concurrent offline edits on both nodes, Wi-Fi restored, `sync once` | **PASS (partition & heal)**; router power cut mid-sync **not run** | 3 tasks and 2 notes merged, 0 lost writes | Laptop added notes/tasks (T1, T2); desktop added T3 while offline. Reconnected and converged to matching hash. Pulling router power mid-sync is still open. |
| C. Power pull mid-write, 10 times | `kill9-test` rehearsal run on laptop; hardware power pull **not run** | **kill -9 rehearsal passed; power pull not run** | acked 2678, present 2691, lost 0 | Rehearsal proves process kill leaves database intact. Actual physical plug pull on SSD/NVMe or card not yet executed. |
| D. Clock after a cold boot, no network | `clock-report.sh` | **not run** | | Still open. Needs cold boot after extended shutdown without network. |

### Test C Rehearsal Summary (`make kill9-test N=30` on laptop)

```
== 1. doctor (integrity, hash chain, view)
offgrid doctor (changes nothing)
  PASS  version   offgrid 1.0.0-beta.1, go1.26.5 linux/amd64, sqlite 3.45.1
  PASS  clock     2026-11-07T08:15:00Z, not behind the newest edit in the log
  PASS  disk      213.1 GiB free in /tmp/tmp.ycn60iF0HO
  PASS  database  SQLite integrity check passed (2691 ops); a write-ahead log is present (normal while running, or after power was cut: it is replayed on the next open)
  PASS  chain     hash chain intact over 2691 ops
  PASS  view      the task view matches the log (2691 ops, hash c55e514d0421)

6 passed, 0 warnings, 0 failed

== 2. every acknowledged write is present
acknowledged (laptop saw ACK): 2678
present in the database:       2691
written but not acknowledged:  13   (allowed: committed just before the power went)
LOST acknowledged writes:      0

RESULT: PASS  acked=2678 present=2691 lost=0
```

## Everything that broke or surprised you

| # | What happened | How you noticed | Fixed? (commit) | Turned into a simulator action? |
|---|---|---|---|---|
| 1 | SemVer pre-release `-beta.1` rejected by Day 4 regex in `cmd/offgrid/exec_test.go` | CI release workflow failed | Fixed: regex broadened to `^offgrid \d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?\n$` | Verified in CI |
| 2 | Typo in file extension (`-db desktop.md`) creates a distinct database with unique hash | Initial hash discrepancy during manual testing | Fixed: specified exact `-db desktop.db` | Verified in field test |
| 3 | Sync server unbounded write/idle connections | Audit in Day 32 | Fixed in `synccmd.go` with `WriteTimeout: 2m`, `IdleTimeout: 90s` | Tested in `synctimeouts_test.go` |
| 4 | WSL2 sleep froze node clock 18h behind real time | `offgrid doctor` on Node 2 failed with `FAIL clock` (time before install date) | Resolved by setting clock; `doctor` then accurately emitted edge `WARN` (no time server) | Yes (stale clock on wake) |

## Decision: hardware to recommend

**Undecided.** No SD card has been tested, and physical power cuts (pulling the plug on SSD/NVMe or card) have not yet been performed.
