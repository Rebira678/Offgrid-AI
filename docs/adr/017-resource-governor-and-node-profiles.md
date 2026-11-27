# ADR 017: A resource governor and two node profiles

**Status:** accepted, 2026-11-26 (Day 51, Checkpoint 3).

## Context

Offgrid runs on a laptop that can host a model and on a Raspberry Pi that cannot (a 3B model on a Pi is too slow to be useful and would use most of its memory). Until now nothing stopped a node from trying: model calls were unlimited, slow background jobs ignored the battery, and the only memory check was a soak you had to remember to run. A crash on the Pi at 3 a.m. looks like "the app is flaky".

## Decisions

1. **Two profiles, chosen by the owner.** `light` (no local model; may use a remote one) and `full` (a local model may run). The flag is `-profile light|full|auto` on `web`, `drain`, `agent`, `sync serve` and `sync run`; `$OFFGRID_PROFILE` sets it for a machine. The default is `full`, so nothing changes for anyone who does not choose. `auto` is light on ARM, under 8 GiB or under 4 threads, else full, and unknown memory counts as small. Every command prints which profile is in force.
2. **Light means "ask a full node", not "no AI".** A model call on a light node is refused at once with a message that says where to go (`-host llamacpp@URL` or `ollama@URL`). A remote model costs this machine nothing and is allowed. Refusals are `llm.ErrUnavailable`, so the router and agent already degrade (rules, queue) instead of failing.
3. **Slots, a queue and a wait.** At most N model calls run at once (full: 1 local, 2 remote; light: 1 remote), at most 4 (light 2) more may wait, and a waiter gives up after 60 s (light 20 s). Beyond that the call is refused immediately. Zero slots says so at once instead of waiting.
4. **Background work waits for the charger and for quiet.** Calls made under `governor.Background` (the drain loop) do not run on battery unless `-allow-battery`, nor while the CPU is busy. A person at the keyboard is never held back by either rule. Battery state is read from `/sys/class/power_supply`; a machine whose state cannot be read counts as not on battery, so the check can never be what stops a node from working.
5. **A soft memory limit per profile**, applied with the Go runtime's memory limit (light 96 MiB, full 768 MiB; `-mem-limit`; `$GOMEMLIMIT` wins). It covers this process only: a local model server is a separate process and needs its own budget. The limit is soft; near it the node gets slower, it does not die.
6. **The hard check is the soak.** `offgrid sync stats -max-rss MiB` fails a run whose RSS ever peaked above the budget (warm-up included: a node is as big as its peak). A trend check fails a slow leak that the end-versus-start test cannot see: it fits a line to the steady part, extrapolates 24 hours, and fails above 50% growth (`-max-rss-drift`), but only when the steady part spans at least 10 minutes.
7. **pprof is off by default** and, when asked for with `-pprof 127.0.0.1:PORT`, listens on loopback only, on its own mux. A test reads every source file and fails if anything could serve `http.DefaultServeMux`, where `net/http/pprof` registers itself on import.

## Rejected

- **Auto as the default.** A 4 GiB laptop that quietly loses its local model is a worse surprise than a Pi that is told to say `-profile light` once.
- **A hard memory cap (killing at a limit).** A node that dies at its limit is the failure we are trying to avoid.
- **Gating interactive calls on the battery.** The person asked; making them wait is the wrong trade.

## What it costs

On a laptop the drain loop now waits for the charger by default. That is the point, and `-allow-battery` is the way out. Memory limits are for this process only; the model server's memory must be budgeted by whoever installs it.

## What would change it

Real Pi numbers that show 96 MiB is too tight for sync plus the web UI with photos (raise it; the soak budget will have said so first); a power source other than sysfs that matters (macOS, Windows).
