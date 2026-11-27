# Day 51: Checkpoint 3, the resource governor

Decisions in [ADR 017](adr/017-resource-governor-and-node-profiles.md); measurements and the 24-hour recipe in [checkpoint3-report.md](checkpoint3-report.md).

## What you can do now

```
offgrid web   -profile light -pprof 127.0.0.1:6060 ...     a small box: no local model, profiler on loopback
offgrid drain -profile full  -model-slots 1 -allow-battery  a laptop: one local model call at a time, even on battery
offgrid agent -profile light -host llamacpp@http://pi-full.lan:8080   a light node borrowing a full node's model
offgrid sync serve -profile auto -mem-limit 80 ...          light on ARM or under 8 GiB, a custom soft limit
offgrid sync stats -max-rss 128 FILE                        fail a soak whose RSS ever peaked over 128 MiB
export OFFGRID_PROFILE=light                                the same for every command on this machine
```

Each long-running command prints a line such as `sync serve: profile light: memory limit 96 MiB (profile light), no local model, 1 remote call(s) at once`. The default profile is `full`, so nothing changes until you choose.

## Behaviour, as tested

- Light node, local model: refused at once with "this is a light node and has no local model; point -host at a full node (llamacpp@URL or ollama@URL)". The refusal is `llm.ErrUnavailable`, which the router and agent already degrade on.
- Light node, remote model: allowed.
- Full node: at most `-model-slots` local calls at once; the next 4 may wait up to 60 seconds; beyond that, refused immediately. A cancelled or refused waiter gives its queue place back (a mutation that forgot to was caught).
- Drain's slow jobs wait for the charger and for the CPU to be quiet. A person asking is never held back.
- Battery state comes from `/sys/class/power_supply`; a mains, USB-C, USB-PD or wireless supply that is online means "not on battery"; unreadable means "not on battery".
- `GOMEMLIMIT` in the environment beats the profile's limit.
- pprof: off by default; loopback only (0.0.0.0, a LAN address and a bare port are refused); its own mux. A test reads every source file and fails if anything in the product could serve the default mux.

## Verified

`go test -race -short ./...` clean, gofmt, vet, lint 0 issues, wasm builds. Mutations: 20 on the governor (3 survivors found and killed with new tests: USB-C and wireless chargers, a leaked queue place after a refusal, zero slots), 10 on the soak additions (one equivalent mutant removed by tightening a guard). Two 12-minute, two-process soaks (light and full) passed with signed ops and identical state hashes; numbers in the report.

## Found on the way

The first version of the trend check failed both rehearsal runs, because RSS climbs for about five minutes while caches fill and then goes flat. A straight line through that looks like a leak. It now needs two hours of steady data and says "not judged" when it has less; the budget check (the peak) still applies to every run. That means **the trend check has not yet been exercised on real data**: only your 24-hour runs will do that.

## Limits, honestly

- No Pi was measured, and no 24-hour run was made. The container numbers are a floor.
- The memory limit covers this process only. A local model server is a separate process; budget its memory yourself (the Pi does not run one).
- The soak made no model calls, so it says nothing about memory during inference.
- The battery rule is Linux sysfs only; macOS, Windows and WSL2 report "not on battery".

## Your part

1. Run the 24-hour soaks from the report on the Pi (light) and the laptop (full) and paste the numbers into `docs/checkpoint3-report.md`.
2. Put `OFFGRID_PROFILE=light` in the Pi's service environment.
3. If a laptop user finds that drain no longer runs on battery, that is the new default; `-allow-battery` restores the old behaviour.
4. Earlier open items still stand.
