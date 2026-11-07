# Day 32: put it on a second machine and try to break it

**Done when:** a report from two real devices with the clock and power-loss results; the top three defects fixed; the README says exactly what has and has not run on real hardware.

**What is done and what is not.** I cannot plug in a Raspberry Pi or pull a router's power. So this day has two halves:

| Half | Status |
|---|---|
| The tools: `offgrid doctor`, the power-pull kit, the clock report, the report template | **Built and tested here** |
| Problems found by auditing the code and running the whole suite under `-race`: 3 defects and 1 missing tool, all fixed (the two network ones each with a test that failed first) | **Done** |
| The tests on real hardware: WiFi sync, router cut, ten power pulls, cold-boot clock | **Yours to run.** Nothing below counts as having happened until a row in `docs/real-device-report.md` has a date and the hardware named |
| "Fix the top three defects" from those runs | **Open.** The ones I fixed came from the audit, not from your hardware. Whatever your runs break goes on top; send me the report |

```bash
offgrid doctor -db tasks.db -deep      # is this machine fit to run a node?
make kill9-test                         # rehearse the power-pull test on one machine (no hardware needed)
```

## 1. `offgrid doctor`

Read-only: it opens the database read-only (SQLite's `mode=ro`) and creates nothing. Exit 0 when nothing failed, 1 when something did, 2 for a usage mistake. `-json` for machines, `-deep` to also recompute the hash chain and the view.

| Check | FAIL when | WARN when |
|---|---|---|
| version | the SQLite driver does not load | not Linux, or not amd64/arm64 |
| clock | the clock is before this program's install date; or the newest edit in the log is **more than an hour ahead** of the clock | newest edit more than a minute ahead; no time server (`timedatectl`); no hardware clock device |
| disk | under 50 MiB free next to the database | under 500 MiB; the machine's only storage is an SD or eMMC card |
| port | | an address Offgrid wants (7878 sync, 8787 web) is taken |
| database | SQLite's `integrity_check` reports damage, or the file will not open | |
| chain, view (`-deep`) | the log was edited, or the view does not match the log | |

Why these: they are what goes wrong on a node that was **installed once and left**. The clock matters most because every edit is stamped from it:

- **Clock behind the log by more than an hour (usually after a power cut on a machine with no battery clock):** the node keeps stamping from the newest time it ever saw, so its new edits are *ahead* of everyone else's clock, and peers refuse them (`clock_drift`). Doctor fails this loudly because the damage is silent until the first sync.
- **Clocks differing by more than five minutes between two nodes:** sync is refused outright (`clock_skew`, the HMAC window). The error message already says which side to fix.
- **No battery clock, no time server:** after a cold boot the time is whatever the system last saved (`fake-hwclock` on a Pi). It can be hours or days old.

What doctor **cannot** see: whether the hardware clock has a battery; whether an SD card is about to die; whether a router will drop the WiFi. Those are what the tests below are for.

## 2. The real-device protocol

You need: the laptop, and one second machine (a Raspberry Pi, or the old library PC booted from a Linux USB stick: say which in the report), and a router both can join. A phone hotspot works if you have no router, but then you cannot test a router power cut.

**Setup.** Build for the second machine and copy it (a Pi is arm64):

```bash
GOOS=linux GOARCH=arm64 CGO_ENABLED=1 CC=aarch64-linux-gnu-gcc go build -o offgrid-arm64 ./cmd/offgrid   # sudo apt install gcc-aarch64-linux-gnu
# or download the release tarball for your architecture, or build on the machine itself: make build
scp offgrid-arm64 scripts/real-device/*.sh pi@NODE:~/ && ssh pi@NODE 'chmod +x offgrid-arm64 *.sh'
openssl rand -hex 32 > key        # copy the SAME file to both machines
```

On the node: `./offgrid-arm64 doctor -db tasks.db -deep` and put the output in the report. Read every WARN.

### Test A: sync over real WiFi (baseline)

```bash
# node:     ./offgrid-arm64 sync serve -create -db node.db -addr 0.0.0.0:7878 -key-file key   # -create: node.db is new
# laptop:   offgrid exec -db laptop.db -script bulk.jsonl            # bulk.jsonl is made below
#           offgrid sync once -db laptop.db -peer http://NODE:7878 -key-file key
# both:     offgrid sync summary -db FILE        # the two "state hash" lines must be identical
seq 1 5000 | awk '{printf "{\"intent\":\"create_task\",\"title\":\"bulk-%d\"}\n",$1}' > bulk.jsonl
```

Record: time for 5,000 ops, and whether the hashes match. (`sync serve` listens on the network here, and sync is **signed, not encrypted**: do this on a network you trust.)

### Test B: pull the router's power mid-sync

Start a bigger sync (`sync run -every 2s` on the laptop while the node is behind by 5,000 ops), **pull the router's power** a few seconds in, wait a minute, restore it.

Pass: the sync resumes by itself, the hashes match afterwards, nothing in `offgrid doctor -db ... -deep` fails on either side. Record how long recovery took and every error line you saw.

### Test C: pull the node's power mid-write, ten times

On the laptop (the **laptop** keeps the record, because a record on the node would be lost with the node):

```bash
ssh -o ServerAliveInterval=2 pi@NODE './powerpull-writer.sh ~/node.db' | tee -a acks.log
```

At a random moment pull the node's power (the plug, not a shutdown command). Press Ctrl-C on the laptop. Power it back on. Then on the node, with `acks.log` copied over:

```bash
./powerpull-check.sh ~/node.db acks.log
```

Repeat ten times with the same `acks.log` (`tee -a`) and the same database. Pass: **every run ends `RESULT: PASS ... lost=0`**. Any `lost=N` above zero is the most important finding of the day: stop and send it to me. "Written but not acknowledged" is normal: the node committed just before the power went and the ACK did not leave.

The writer prints `ACK` only after the command exited successfully, which is after the commit reached disk (`synchronous=FULL`). A pass means: every write the node *told you* was saved is still there, the SQLite file is intact, and the hash chain and view agree.

**Rehearse first** with `make kill9-test` on any machine. It kills the writer at random moments instead of cutting power. In the sandbox I ran it for 60 rounds: 5,308 acknowledged writes, 5,338 in the database (30 written but not acknowledged), 0 lost, integrity check, chain and view all passing. **That is not a power-cut result.** `kill -9` stops the process but the operating system keeps running and still flushes its caches; only pulling the plug tests the disk, the SD card and the filesystem.

### Test D: the clock after a cold boot with no network

Write down the real time. Shut the node down for at least an hour (a night is better; this is the case that matters). Boot it with **no router**. On the node:

```bash
./clock-report.sh ~/node.db | tee clock-coldboot.txt
```

Record how far the system clock is from the real time, and what `doctor` says. Then plug in the router and see whether it corrects itself, and whether the node and laptop can sync.

## 3. What I found without hardware

Defects 1 and 2 are things a real network does and localhost never does; each has a test that **failed before the fix**. Row 3 is a missing tool, tested after the fact (and mutation-tested). Row 4 is a flaky test found by running the whole suite with `-race`.

| # | Defect | Effect on a real node | Fix | Test |
|---|---|---|---|---|
| 1 | `offgrid models`, and `Version`/`ListModels` in general, had no deadline and the HTTP client has no overall timeout by design | A host that accepts the connection and never answers (half-dead WiFi, a wedged Ollama on another node) hung the command forever | Short calls get a 10 s deadline when the caller gave none; opening a connection is bounded to 5 s | `internal/ollama/deadline_test.go` |
| 2 | `sync serve` had read limits but no `WriteTimeout` or `IdleTimeout` | A peer that stops reading (WiFi drops mid-response, a laptop sleeps) holds its connection and goroutine until the OS gives up, which for an idle keep-alive connection is never. On a small node, a week of that adds up | 2 minute write limit, 90 s idle limit, header size cap | `TestSyncServerBoundsEveryPhase` (mutation-tested: removing `WriteTimeout` fails it) |
| 3 | No way to see that a node is unfit before it misbehaves | The clock, disk and database problems above are silent until the first sync or the first power cut | `offgrid doctor` | `internal/doctor`, 13 tests; 7 of 8 mutations caught |
| 4 | `TestTheLawsCatchEveryDeliberatelyBrokenMergeAndAcceptTheControls` failed once in about 130 runs: its random search did not find the narrowest planted bug (equal stamps, different nodes and values, spread through a gossip round) | Your CI runs `go test -race` on every push, so about one push in a hundred would go red for no reason, then green on retry. That teaches people to ignore red | The test pins `rapid.seed` unless one is given on the command line. Seeds 1 to 40 all catch every planted bug, so the margin is wide. Other property tests keep random seeds | `internal/crdt/broken_test.go` (45 runs under `-race` and 40 without had passed before the change too; the failure was rare, so I cannot show a before/after count, only that the result is now fixed) |

A review of every network call found nothing else without a deadline: `sync once` has `-timeout`, `sync run` bounds each round, the web server has all four timeouts, the connectivity probes take a context.

## 4. What this does not show

- **Everything above ran in a cloud sandbox on x86.** No Pi, no WiFi, no power cut. The kit's logic is tested (the checker catches a lost write, a damaged database, an empty log, a cut-off line; the writer's ACKs are really in the database), but whether a Pi's SD card survives is exactly what Test C measures, and I cannot know the answer.
- **The mutation that survived:** `doctor`'s "not Linux" warning cannot be reached from a Linux test run.
- **A power cut can lose more than the database.** If the filesystem itself is damaged (ext4 on a worn SD card), the node may not boot; that is the "SD card" row in the roadmap's risks, and why doctor warns on SD-only machines.
- **`doctor` opens the database read-only, so on a database with a leftover write-ahead log it reads the committed state without replaying the log into the main file.** The `-deep` check opens it normally and does replay. That is why `powerpull-check.sh` runs `-deep`.

## 5. Hardware recommendation: decided by your numbers, not mine

Fill this in from the report, then edit the roadmap's hardware row:

| If | Then recommend |
|---|---|
| Test C loses nothing on the card | the card is acceptable for the pilot, with backups |
| Test C loses acknowledged writes, or the file is damaged | SSD (USB or NVMe) only. Do not ship SD-card nodes |
| Test D clock is wrong by more than 5 minutes | a battery-backed clock module, or one node on the site that serves time to the rest |
| Test D clock is right within seconds | say so, and still document the risk for a site that is off for weeks |

## 6. Found on real hardware: a mistyped path made a second replica

Your Test A/B run hit it: `sync once -db desktop.md` (a typo for `desktop.db`) silently made a new, empty replica with its own node ID and its own state hash, and the hashes did not match until the typo was found. It is the Day 31 bug on the one family of commands that is *allowed* to create a database.

**Fix.** `sync serve`, `sync once` and `sync run` now refuse a database file that does not exist (exit 3, `no such database`, nothing created) unless you add `-create`. With `-create` a new file is announced on stderr: `creating a NEW replica at PATH (it gets its own node ID, and keeps it)`. An existing file is opened as before, so `-create` on a database that exists does nothing and says nothing. `exec` and `agent` still create on first use.

- Tests, written first: they failed before the fix (`sync serve` on a mistyped path created the file and ran forever, so the test timed out): `TestSyncCommandsDoNotCreateADatabaseUnlessAsked`, `TestSyncCreateStartsANewReplicaAndSaysSo`.
- Four existing tests and six demo scripts started new replicas through sync and now say `-create` (`sync-demo`, `converge-demo`, `tamper-demo`, `web-demo`, `ai-suggests-demo`, `two-brains-demo`). `make web-check`, `tamper-demo`, `converge-demo`, `sync-demo`, `ai-suggests-demo` and `two-brains-demo` pass.
- **Your own scripts and notes:** any command line of yours that syncs into a database that does not exist yet needs `-create` now. When it is missing you get exit 3 and a message that says so, not a silent new replica.
- **What it does not catch:** a typo that happens to name a file that exists (`-db tasks.db.bak`) is opened normally. That is the same limit as Day 31.

## Your part

```bash
make test && make wasm-test && make lint            # on your machine
make doctor DB=tasks.db DEEP=1                      # try it on the laptop first
make kill9-test N=30                                # rehearse the power pull
# then Tests A to D on the second machine; fill in docs/real-device-report.md; send it to me
```

The README row for Day 31 is added in this day's README (it was left out of the Day 31 zip).
