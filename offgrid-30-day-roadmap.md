# Offline-First AI Agent Runtime: 30-Day Roadmap

Working name: **Offgrid** (rename it whenever you like).
Stack: Go, SQLite (`modernc.org/sqlite`), Ollama (local models), your own CRDT code.
Schedule: Day 1 is Tue Oct 6, Day 30 is Wed Nov 4, 2026. About 125 focused hours in total.
Cost: $0. Everything runs on your HP EliteBook with no GPU.

---

## 1. Read this first

No roadmap can guarantee that a founder or recruiter invites you in. What this plan does is control the two things you can control: it builds proof that is hard to ignore, and it puts that proof in front of the right people every day, ending with direct outreach. Invitations come from proof plus outreach, so both are in the plan.

The speed numbers I gave earlier (tokens per second) were estimates. Day 1 replaces them with your own measurements, and those measurements become your first post.

---

## 2. Positioning (the market-analyst part)

**One-line pitch:**
> An AI agent runtime that keeps working when the internet doesn't. A local model makes instant decisions, a durable queue holds the hard thinking for later, and CRDT sync merges offline and online work without conflicts.

**The story line:** AI infrastructure for the places the AI industry assumes don't exist. Use your own words and your own experience here. A real personal reason is the part nobody can copy.

### Who you are trying to stop, and what stops them

| Audience | What they scan for | What to show them |
|---|---|---|
| Founders / CEOs | A clear problem, a live demo, evidence you ship and own outcomes | The 90-second video, the story, the numbers |
| Recruiters | Keywords and visible activity: Go, distributed systems, CRDT, LLM, SQLite, a clean repo, a daily commit streak | Your LinkedIn headline, pinned repo, steady posts |
| CTOs / hiring engineers | Trade-offs, tests, benchmarks, honest limitations | Property tests, simulation tests, a README that admits what it can't do |

### Where to aim (my read of the landscape, not measured market data)
- Local-first software and sync infrastructure companies
- On-device and edge AI startups
- AI infrastructure, agent platforms, and agent observability tools
- Field-operations software: agriculture, health, logistics, utilities, construction
- Disaster response, humanitarian, and defense technology
- Fintech and telecom companies serving low-connectivity markets

---

## 3. What you are building

**Demo scenario:** a field-worker assistant. The worker types or speaks short commands ("log a leaking pump at site 4, high priority", "what's still open from yesterday?"). It works with no signal and syncs when the connection returns.

```
 request
    |
    v
 Router  --(simple)-->  Tier 0: rules, instant, no model
    |    --(medium)-->  Tier 1: small local model (1-3B), seconds
    |    --(hard)---->  Tier 2: queued, deliberate model (7-8B local, optional cloud)
    v
 Tools (validated, typed)  -->  Op log (SQLite, immutable ops, HLC timestamps)
                                      |
                                      v
                       CRDT state (LWW-Map + OR-Set)  <--sync over HTTP-->  other replicas
```

**Core scope (must ship):** agent core, tiers, durable queue, connectivity monitor, LWW-Map plus OR-Set, two-replica sync, tests, benchmarks, CLI, README, video.
**Stretch (drop first if behind):** web dashboard (Day 26), hash-chained log (Day 27), optional cloud model.

---

## 4. Daily rhythm (4-5 hours)

- 30 min: plan the day and read the one thing you need
- 2.5-3 h: build
- 45 min: tests, commit, push (a commit every single day)
- 30 min: record a clip or screenshot and write the post

**Light days** are Days 7, 14, 21 and 28 (2-3 hours: recap, cleanup, one longer post). Seven straight full days for a month is how people burn out in week 3.

---

## 5. Public-proof rules

Every day ends with one post. The formula:

1. **One visual**: a short clip, terminal screenshot, or diagram
2. **One number**: tokens per second, % valid JSON, ops synced, seeds passed
3. **One lesson**: a sentence about what surprised you or what you'd do differently

Post on LinkedIn first. Cross-post the strongest ones to X, r/golang, r/LocalLLaMA. Reply to every comment within a few hours. Replies are cheap and visibly show you're reachable.

---

## 6. Checkpoints and kill rules

| Checkpoint | Day | You must have | If you miss it |
|---|---|---|---|
| 1 | 10 | Offline agent plus queue works end to end with WiFi off | Cut Day 13's dashboard to plain logs |
| 2 | 20 | Two replicas diverge offline and converge after reconnect | Cut Days 22-24 to the minimum, skip Days 26-27, keep the sync |
| 3 | 26 | Feature freeze | Days 27-30 are polish, README, video, outreach only |

Rule: when you miss a checkpoint, cut features. Never extend the deadline.

**Fallback:** if Days 1-3 go badly (model unusably slow, or CRDTs won't click after real effort), switch to the Agent Trust & Audit Layer idea. It's smaller and its tamper-evidence demo is nearly as convincing.

---

## 7. Daily plan

### Week 1 (Oct 6-12): Foundations and a first public claim

### Day 1: Benchmark your hardware and stake your claim
- **Build:** Install Go and Ollama. Pull `llama3.2:1b`, `llama3.2:3b`, and one 7-8B model. Measure tokens/sec (`ollama run <model> --verbose`) on short and long prompts. Create the public repo, `go mod init`, and a README with the problem and the one-line pitch. Update your LinkedIn headline with Go, distributed systems, AI infrastructure.
- **Done when:** the README has a table of real benchmark numbers from your laptop.
- **Post:** "Starting a 30-day build: an AI agent that works with the internet off. Here's what a laptop with no GPU actually does." Attach the benchmark screenshot.
- **Hooks:** founders (public commitment and story), recruiters (new repo, right keywords).

### Day 2: Model client you can swap
- **Build:** An `internal/llm` package with a `Client` interface and an Ollama implementation (streaming, timeouts, context cancellation). Add a CLI command `offgrid ask`.
- **Done when:** the CLI answers with WiFi off, and a unit test passes using a fake client.
- **Post:** A 15-second clip of the WiFi icon off while the answer streams in.
- **Hooks:** everyone (instantly understandable), engineers (clean interface).

### Day 3: Make the model reliable
- **Build:** Constrained JSON output (Ollama's `format` option), validation, and bounded retries. Write an eval set of 30 field-worker commands with expected intents. Run it on the 1B and 3B models.
- **Done when:** you have a table of valid-JSON rate and intent accuracy per model.
- **Post:** The chart: "1B vs 3B: which one can you trust to drive an agent?"
- **Hooks:** AI hiring managers (eval numbers), CEOs (you measure before you decide).

### Day 4: The model proposes, the code disposes
- **Build:** A tool registry with typed arguments and five tools: `create_task`, `update_task`, `list_tasks`, `add_note`, `daily_summary`. The executor validates every argument. Model output never touches the database directly.
- **Done when:** all tools are unit-tested with no model involved.
- **Post:** A short note on the rule "the model proposes, the code disposes".
- **Hooks:** CTOs and security-minded founders.

### Day 5: Agent loop with tiers
- **Build:** The loop: Tier 0 rules (keyword and regex intents run instantly, no model), Tier 1 small model for the rest. Log which tier handled each request and its latency.
- **Done when:** 20 of your 30 eval commands run end to end with tier and latency printed.
- **Post:** Latency comparison, rules in milliseconds versus model in seconds. Lesson: "don't call an LLM when a regex will do."
- **Hooks:** founders (cost and latency judgment).

### Day 6: Event-sourced storage
- **Build:** SQLite store with an append-only `ops` table (each change is an immutable operation with a node ID and a timestamp placeholder) plus a materialized `tasks` view rebuilt from ops. Add migrations.
- **Done when:** you can delete the materialized table, rebuild it from ops, and get identical state.
- **Post:** A diagram of the op-log design and why sync needs it.
- **Hooks:** senior engineers (event sourcing is the foundation for everything later).

### Day 7 (light): Week 1 recap
- **Build:** Refactor, add a README quickstart, record a full demo of logging tasks entirely offline.
- **Done when:** a fresh clone runs in three commands.
- **Post:** Week 1 recap with the demo clip and the benchmark table.
- **Hooks:** recruiters (clean repo, visible streak).

### Week 2 (Oct 13-19): Offline queue, connectivity, and crash safety

### Day 8: Connectivity monitor
- **Build:** A goroutine that probes connectivity (short-timeout HTTP or DNS) with a state machine ONLINE / DEGRADED / OFFLINE and hysteresis so it doesn't flap. It publishes events on a channel and shuts down cleanly via context.
- **Done when:** toggling WiFi flips the state within seconds, and a fake prober tests the transitions.
- **Post:** The state diagram plus a clip of the state changing live.
- **Hooks:** distributed-systems engineers.

### Day 9: Durable job queue
- **Build:** A SQLite-backed jobs table with statuses, exponential backoff with jitter, idempotency keys, a max-attempts limit, and a dead-letter state.
- **Done when:** 100 jobs enqueued offline all persist, and backoff has unit tests.
- **Post:** "Why retries need jitter", with a small chart of retry timing.
- **Hooks:** CTOs (you know the classic failure modes).

### Day 10: Router and Checkpoint 1
- **Build:** The router decides "answer now" or "defer" using intent complexity, output validity, and connectivity.
- **Done when:** with WiFi off, simple requests answer instantly and hard ones ("summarize this week's incidents and suggest priorities") queue with a visible status. **Checkpoint 1.**
- **Post:** Your first flagship video (60-90 seconds): WiFi off, agent keeps working, queue fills.
- **Hooks:** everyone. This is your first clip worth sending directly to people.

### Day 11: A switch to break your own network
- **Build:** A fault-injecting HTTP transport plus CLI flags for latency, drop percentage, and partition on/off, so failures are reproducible.
- **Done when:** a script flips a partition on and off on a timer.
- **Post:** "I built a switch to break my own network on purpose." It also makes your demos safe from real-world WiFi surprises.
- **Hooks:** reliability-minded CTOs.

### Day 12: Drain on reconnect, survive a crash
- **Build:** A worker that drains the queue in order on reconnect with a concurrency limit and resumable progress.
- **Done when:** an automated test runs `kill -9` mid-drain, restarts, and finds no lost jobs and no duplicates.
- **Post:** Clip of kill, restart, nothing lost.
- **Hooks:** the strongest "this person is senior" signal of week 2.

### Day 13: See the system
- **Build:** Structured logs with `log/slog`, counters (queue depth, jobs per tier, latency), and a Bubble Tea terminal view showing connection state, queue, and tier decisions live.
- **Done when:** one screen shows the whole system's state.
- **Post:** A clip of the live dashboard. This is the visual that holds a scrolling viewer.
- **Hooks:** founders (they can see it working), recruiters (polish).

### Day 14 (light): Recap and target list
- **Build:** Write `ARCHITECTURE.md` v1, tag `v0.1`. Build your **target list**: 40 companies or people from the categories in section 2, with one specific note on what each does.
- **Done when:** the list exists in a spreadsheet and v0.1 is tagged.
- **Post:** Week 2 recap with the `kill -9` clip.
- **Hooks:** recruiters (steady progress).

### Week 3 (Oct 20-26): CRDT sync, the core of the project

From today until Day 30, spend 15 minutes a day on outreach prep: follow your targets and leave a thoughtful comment on one of their posts.

### Day 15: Hybrid logical clock
- **Build:** Implement a hybrid logical clock from the paper by Kulkarni et al. Add tests for monotonicity, clock skew, and the clock going backwards. Replace the placeholder timestamps.
- **Done when:** the tests pass, including skew cases.
- **Post:** "Why your phone's clock can't be trusted for sync, and the fix."
- **Hooks:** distributed-systems people and CTOs.

### Day 16: LWW-Map
- **Build:** Per-field last-writer-wins registers for tasks, with a deterministic tie-break by node ID. The merge function must be pure.
- **Done when:** unit tests cover concurrent edits to the same field.
- **Post:** A two-person conflict example explained in plain language.
- **Hooks:** anyone, because it's explained without jargon.

### Day 17: Prove the merge is correct
- **Build:** Property-based tests (`pgregory.net/rapid`) that check merge is commutative, associative, and idempotent over random operation sequences.
- **Done when:** thousands of generated cases pass. Then deliberately break the tie-break and show the tests catch it.
- **Post:** The test output plus "the bug my tests caught."
- **Hooks:** senior engineers and CTOs (you prove it, you don't hope).

### Day 18: OR-Set for task lists and tags
- **Build:** An add-wins observed-remove set with tombstones.
- **Done when:** tests cover concurrent add and remove of the same item.
- **Post:** "What happens when I delete a task while you edit it?"
- **Hooks:** product-minded founders (it's a real user problem).

### Day 19: Sync protocol
- **Build:** Version summaries per node, delta exchange of only the missing ops, `/sync/summary` and `/sync/ops` endpoints, idempotent apply, and minimal HMAC authentication with a shared key.
- **Done when:** two processes exchange ops over localhost HTTP.
- **Post:** A sequence diagram of one sync round.
- **Hooks:** engineers.

### Day 20: Two replicas converge (Checkpoint 2)
- **Build:** The scenario: both replicas go offline (using your chaos switch), edit the same task, add and delete list items, then reconnect.
- **Done when:** both replicas end with identical state, verified by comparing state hashes. **Checkpoint 2.**
- **Post:** Flagship clip #2: split-screen, two terminals, WiFi off, edits on both sides, reconnect, hashes match.
- **Hooks:** this is the clip that goes straight into founders' inboxes.

### Day 21 (light): Recap and the long form
- **Build:** Re-cut the flagship video, update the README, draft a longer article "Why I used CRDTs for an AI agent."
- **Done when:** the draft article is saved.
- **Post:** Week 3 recap with the convergence clip.
- **Hooks:** recruiters and hiring managers (a visible writing sample).

### Week 4 (Oct 27-Nov 2): Smarter tiers, hardening, polish

### Day 22: Tier 2 deliberate reasoning
- **Build:** A worker that processes queued hard jobs with the 7-8B local model (plus an optional cloud client if configured and online). Results are written back as ops so they sync like everything else.
- **Done when:** a queued summary completes after reconnect or when idle.
- **Post:** "Two brains, one agent: fast and slow thinking on a laptop."
- **Hooks:** AI engineers.

### Day 23: The AI never overwrites a human
- **Build:** AI output is stored as *suggestions* in a separate field. Human edits always win, and accepting a suggestion is an explicit op.
- **Done when:** a test shows a human edit made offline survives an AI job finishing later.
- **Post:** "My agent can't overwrite your work. Here's the rule."
- **Hooks:** trust and safety minded CEOs and enterprise buyers.

### Day 24: Deterministic simulation test
- **Build:** A seeded simulation that throws random partitions, clock skew, and crashes at three replicas and asserts convergence. A failing seed must be exactly reproducible.
- **Done when:** 1,000+ seeds pass in a CI-friendly time.
- **Post:** "I ran N random failure schedules. Here's the one that broke it." Use the real number and the real bug.
- **Hooks:** top-tier signal for distributed-systems hiring managers.

### Day 25: Benchmarks
- **Build:** Tokens/sec per model, queue drain time, sync time for 100 / 1,000 / 10,000 ops, and memory use. A `make bench` target regenerates the README table.
- **Done when:** the benchmarks are reproducible by anyone.
- **Post:** The benchmark chart with your hardware stated plainly.
- **Hooks:** CTOs.

### Day 26: Web dashboard (Checkpoint 3, feature freeze)
- **Build:** A single-page UI served from the Go binary with `embed`: replicas, connection state, queue, merges, tier decisions, and accept/reject for AI suggestions.
- **Done when:** the dashboard runs from the single binary. **Feature freeze after today.** Skip this day if you are behind and use the time to harden instead.
- **Post:** A clip of the UI.
- **Hooks:** non-technical founders and CEOs, who judge by what they can see.

### Day 27: Tamper-evident log (stretch)
- **Build:** Hash-chain the op log (each op includes the previous op's hash) and add a `verify` command. Write a short threat-model section. If you are behind, do only the threat-model document.
- **Done when:** editing a past op makes `verify` fail.
- **Post:** "I edited my own log and the system caught it." This also previews the audit-layer idea.
- **Hooks:** compliance-minded founders.

### Day 28 (light): Ship-ready repo
- **Build:** GitHub Actions CI (build, test, lint), release binaries for Linux, a license, `CONTRIBUTING.md`, and a few "good first issue" tickets.
- **Done when:** CI is green and a release is published.
- **Post:** Week 4 recap.
- **Hooks:** recruiters (a project that looks maintained).

### Final stretch (Nov 3-4): Package and launch

### Day 29: README and video
- **Build:** README structure: pitch, 60-second demo GIF, 3-command quickstart, architecture diagram, design decisions and trade-offs, benchmarks, honest limitations, what's next. Record the final 90-second video (0-10s the problem, 10-60s the demo, 60-90s the numbers and a call to talk).
- **Done when:** a friend clones the repo and runs it in under 10 minutes.
- **Post:** A "launching tomorrow" teaser with the trailer clip.
- **Hooks:** everyone.

### Day 30: Launch and outreach
- **Build:** Pin the repo on GitHub, add it to your LinkedIn Featured section, publish the full launch post, and share to r/golang, r/LocalLLaMA, and Hacker News (Show HN). Send **30 personalized messages** from your target list.
- **Done when:** the 30 messages are sent and the launch posts are live.
- **Post:** The launch post with the 90-second video.
- **Hooks:** everyone. This is the day the whole month was for.

---

## 8. Outreach message (use after personalizing the first line)

> Hi [name], I saw [one specific thing about their product or a recent post]. I built an open-source AI agent runtime that keeps working with the internet off (local model, CRDT sync, thousands of random failure schedules tested; benchmarks in the README). 90-second demo: [link]. If offline reliability is something your team deals with, I'd love 15 minutes to hear how you handle it. I'm also open to roles.

Rules: always personalize the first line, never mass-paste, ask for feedback or a conversation first, and follow up once after 4-5 days.

---

## 9. After Day 30

- Reply to every comment and message within a few hours for the first 48 hours
- Post one honest update a week: a bug fixed, a number improved, a lesson
- Keep the repo active. A project with recent commits reads as alive
- Turn the article from Day 21 into a published piece and link it from the README

---

## 10. Main risks

| Risk | Defense |
|---|---|
| CRDT block overruns (Days 15-20) | Hard checkpoint on Day 20, cut Tier 2 and the dashboard before extending |
| Small model returns malformed output | Constrained JSON, retries, and Tier 0 rules for common commands (Days 3-5) |
| Burnout | Four light days, and never skip a night of sleep for a feature |
| Scope creep | Anything not in section 3's core list waits until after Day 30 |
| Posting without substance | The one-visual, one-number, one-lesson rule keeps every post concrete |
| Overclaiming | State your hardware, your test counts, and the limitations plainly in the README |
