# Day 30: launch

**Done when:** the 30 messages are sent and the launch posts are live. Everything below is draft text and tools. **Nothing in it has been posted or sent**, and I cannot post for you. Every number in the posts is either verified in this repo or marked `<yours>` for you to fill from your own run.

```bash
scripts/launch-check.sh                                    # run this right before you post; it changes nothing
scripts/outreach-drafts.py offgrid-target-list.xlsx --video URL > drafts.md
scripts/outreach-drafts.py --check drafts.md               # fails until every first line is written
```

## 0. The claims ledger: say these, and only these

| You may say | Where it is true |
|---|---|
| One Go binary, runs on a laptop with no GPU, costs $0 | the build; README |
| 906 test functions in 28 packages (about 21,600 lines of Go, 24,200 of tests) | `go test -list` count on Day 28; **rerun it** before you quote it |
| 3,000 random failure schedules pass; the simulator found a real bug on its first run (744 of the first 1,000 seeds hit it) | `docs/day-24.md` |
| Two replicas that edit offline end with identical state hashes | `make converge-demo`, `internal/sim` |
| A 3B model generates about 10 tokens a second on the EliteBook (short prompt) | README table, your Day 1 run: **recheck with your own `make bench`** |
| An edited op in the log is named by `store verify` | `make tamper-demo`, `docs/day-27.md` |
| It has not run on real devices or a real network | README, Honest limits |

**Do not say:** "production ready", "secure", "encrypted" (sync is signed, not encrypted), "tamper-proof" (it is tamper-evident, with a stated hole), "works on Mac/Windows" (untested), "faster than X" (nothing here compares with another system), or any number from the sandbox. If a commenter finds a hole, the honest reply is "you're right, here is where it is written down" (every limit is in the README).

## 1. The launch post (LinkedIn)

Attach the 90-second video. Post as text plus video, link in the **first comment** if you prefer (links in the body can reduce reach on LinkedIn; test what works for you).

> I spent 30 days building an AI agent that keeps working when the internet doesn't. It's open source today.
>
> Offgrid is one Go binary. A small local model and a set of rules understand what you ask; anything hard waits in a durable queue until a connection comes back; and two devices that were apart merge their edits and end up byte-identical. No server in the middle. No GPU. $0.
>
> What I'm proudest of is not a feature. It's how it's checked:
> • 906 tests in 28 packages
> • a simulator that threw 3,000 random failures at it (partitions, clock skew, crashes, lossy sync). It found a real bug on its first run, and the fix is in the repo
> • a log that notices when someone edits it by hand, and says which operation changed
> • every benchmark names the hardware, runs three times, and fails instead of printing a time if the two replicas didn't end identical
>
> What it can't do yet, written down in the README: it has only run on one laptop over localhost (the network cut in the video is simulated). Sync is signed, not encrypted. The log only grows.
>
> If you build offline-first software, local AI, or field tools, I'd love your eyes on it, especially the parts I got wrong. And I'm open to roles.
>
> Repo and the 90-second demo below.

First comment: the GitHub URL, and "The part I most want help with: running it on a second real device." Reply to every comment for the first 48 hours.

## 2. X / short thread

1. I spent 30 days building an AI agent runtime that keeps working with the internet off. Open source today. One Go binary, no GPU, $0. [video] [repo]
2. Hard requests don't fail offline. They wait in a durable queue (survives `kill -9`) and run when the connection returns, or when the laptop is idle.
3. Two devices edit the same tasks with the network cut, reconnect, and end byte-identical. No server in between. (The cut is simulated on localhost; real devices are next.)
4. I threw 3,000 random failure schedules at the sync engine. It found a real bug on the first run: two devices each creating "T1" before meeting could never sync. Fixed by putting the device name in the ID.
5. Every number in the repo names its hardware. `make bench` reproduces them. The limits are in the README, not in my replies.
6. What I need: someone to run it on a second real device. Issues labelled "good first issue" are open.

## 3. Show HN

**Title** (HN titles are plain, no hype; check the current length limit when you submit):

> Show HN: Offgrid – an AI agent runtime that keeps working offline (Go, CRDT sync)

**URL:** the GitHub repo. **First comment** (post it yourself right after submitting):

> Hi HN, I'm Rebira. I built this over 30 days, in public, because most "AI agents" are a thin wrapper over a cloud API and stop being useful where the signal isn't.
>
> What it is: a single Go binary with local models (via Ollama), SQLite, and a small op-log CRDT with hybrid logical clocks. Easy requests are answered by rules or a small model on CPU. Hard ones go into a durable queue and run when the connection returns (or the laptop is idle). Two replicas that edited offline merge deterministically.
>
> What I'd want to know if I were reading this: it has only run on one laptop (Ryzen 5 PRO, no GPU) over localhost, with network cuts simulated by a fault-injecting transport. Sync is HMAC-signed but not encrypted. The CRDT is hand-written (per-field LWW and an add-wins set), so I tested it with property tests and a seeded simulator, which found a real bug on day one; I don't claim it's battle-tested. The log is hash-chained so an edit is detected, but someone who can write the file can recompute the chain; the README explains the hole and what catches it.
>
> The model never edits anything on its own: its output is a suggestion beside the task, refused if a person edited the task since. Everything is in the README, including "Honest limits" and the benchmarks with the hardware.
>
> I'd most like feedback on the sync protocol and on the threat model in docs/day-27.md.

Rules to follow: do not ask anyone to upvote, do not post it more than once, do not ask friends to comment from new accounts, and stay at the keyboard for the first few hours to answer. Weekday morning US Eastern time is the usual advice (that is afternoon in Addis); it is a habit, not a guarantee.

## 4. r/LocalLLaMA and r/golang

**Read each subreddit's rules and its self-promotion policy first.** Some require a flair, a minimum account history, or ban project posts outside a weekly thread. If a rule says no, wait for the allowed thread instead of posting around it. Tailor each post; never paste the same one in both.

**r/LocalLLaMA** (the angle is the local-model part):

> **Title:** I built an agent runtime that runs a 3B model on a CPU-only laptop and keeps working with the network off
>
> Ryzen 5 PRO 5650U, 15 GB, no GPU. A 3B model generates about 10 tokens/s on short prompts here (table in the README, rerun with `make bench`). The design question was: what do you do with a small, slow, sometimes-wrong model? Answer so far: rules answer the easy messages instantly, the model only handles what the rules can't, every proposal is re-validated against a closed tool menu, and anything hard waits in a queue for a bigger model that runs only when the machine is idle. AI changes to a task are suggestions a person accepts. Not tested with a 7-8B model yet, and no real cloud API. Repo + 90s demo: ... Happy to hear what you'd do differently.

**r/golang** (the angle is the Go engineering):

> **Title:** Offgrid: a Go project built around an op-log CRDT, SQLite (cgo), property tests and a deterministic simulator
>
> One binary: embedded dashboard (`go:embed`), `mattn/go-sqlite3`, `rapid` property tests, and a seeded failure simulator that replays a failing seed exactly. Things I'd like review on: the hash-chained op log (`internal/store/sqlstore/chain.go`), the sync protocol (`internal/syncer`), and whether the cgo dependency is worth it vs. a pure-Go SQLite. CI runs on Go 1.22 and stable with the race detector. Honest limits are in the README.

## 5. The 30 messages

The roadmap's rules, and why they matter: always personalize the first line, never mass-paste, ask for a conversation first, follow up once after 4 or 5 days.

```bash
scripts/outreach-drafts.py offgrid-target-list.xlsx --video <your video URL> --n 30 > drafts.md
```

Each draft has your notes on the company, the angle, and a **placeholder for the first line that the script will not fill**. The `--check` command fails until all of them are written.

Practical rules:

- **Verify first.** In your list, 39 of 40 rows are "From my knowledge, verify" or "Verify first: ownership or product may have changed". Open each company's site before you write to them; write the first line from something you read there or in their post, never from my note.
- **Send one at a time**, a few per hour, from your own account. Some platforms limit the length of a first message or a connection note: shorten, keep the demo link and the ask.
- **Keep a log** in your target list (the `Followed?`, `First comment (date)` columns): date sent, reply, follow-up date. Follow up **once**, after 4 or 5 days, with something new (a fix, a number, a comment on their post), not "just checking in".
- **Ask for feedback or 15 minutes, not for a job**, as the template does. "I'm also open to roles" is enough.
- If you cannot get to 30 honest, personalized messages today, send fewer. Twelve real ones beat thirty generic ones. Finish the rest on Day 31 and 32.

## 6. Questions you will be asked (draft answers, edit into your voice)

| Question | Honest answer |
|---|---|
| Why not Automerge, Yjs or another CRDT library? | I wanted to understand and prove the merge, and the data model is small (task fields and notes). It costs me maturity and features: the library is the right choice for rich text or large documents. |
| Is the CRDT correct? | The merge is tested as commutative, associative and idempotent with property tests, and 3,000 seeded schedules end identical. That is evidence, not a proof, and the simulator checks two recomputed rules, not every meaning. |
| Why a local model at all? | The point is what happens when the network is gone: the rules and a 3B model answer on the laptop; the hard work waits. |
| Does it work on a real network / on a phone? | Not tested. It has only run on one laptop over localhost. A second real device is the thing I most need. |
| Is it secure? | Sync is signed (HMAC with a shared key), not encrypted. The dashboard has no login, so it only listens on localhost. The log is tamper-evident, not tamper-proof. Nothing is encrypted at rest. |
| Why Go, and why cgo SQLite? | One self-contained binary (dynamically linked to glibc) and good concurrency. cgo SQLite means a C compiler to build; a pure-Go SQLite is an open question I'd like opinions on. |
| What would you build next? | Real-device testing, per-device signing keys, compaction (all in "What's next"). |
| Can I use it commercially? | MIT licence, no warranty. (If you changed the licence, change this.) |

If you do not know, say "I don't know yet, I'll find out and update the README." That answer is worth more than a guess.

## 7. Launch day, in order (Wed Nov 4)

Time zone: you are UTC+3. Do the posts in the hours when you can stay online to answer.

- [ ] Day before: run `scripts/launch-check.sh` with `gh` logged in. Everything green, or fix it. Rerun `make fresh-test URL=...` from GitHub.
- [ ] Run `make bench` on the EliteBook; commit the README with your numbers. Re-record the clip if it shows sandbox numbers.
- [ ] Final video uploaded (YouTube unlisted or LinkedIn native), link works logged out.
- [ ] Tag the release if CI is green: `git tag -a v0.30.0 -m "v0.30.0" && git push origin v0.30.0`; check the release has both tarballs and `SHA256SUMS`; download one and run `version`.
- [ ] Repo: description, topics, a social preview image (use `docs/architecture.png`), good first issues open. **Pin the repo** (your profile, Customize your pins).
- [ ] LinkedIn: add the repo and the video to **Featured**. Publish the launch post. Put the link in the first comment.
- [ ] Post to X.
- [ ] Show HN (a weekday, when you can stay online for hours). Post the first comment at once.
- [ ] Reddit, only where the rules allow it.
- [ ] Outreach: write and send the messages, a few per hour. `--check` before you send the last one.
- [ ] Reply to every comment and message within a few hours, for the first 48 hours.

## 8. The next 48 hours, and after

- Keep a list of every bug, question and objection in an issue each. A fast, honest fix is the best thing a launch can show.
- If someone finds a real hole (security, data loss), say thank you in public, fix it before anything else, and write what you changed.
- Day 31 onward: one honest update a week (a bug fixed, a number improved, a lesson). Follow up once with each outreach contact after 4 or 5 days. Publish the article and link it from the README. Run it on a second device.

## 9. What I could not do

- Post, send or pin anything. I did not open any website or account.
- Check the real targets. Their notes are from your list, which says how sure you were.
- Test the posts' reach. Timing advice is common practice, not something I measured.
