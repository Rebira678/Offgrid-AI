# Day 14: Recap, architecture, v0.1, target list

A light day. Nothing new runs; the point is to make two weeks of work **legible**: to a stranger reading the repo, and to you.

**Done when:** the target list exists in a spreadsheet, and v0.1 is tagged.

## What is in the zip

| File | What it is |
|---|---|
| `ARCHITECTURE.md` | v1: the system in one picture, the process model, components and their dependency direction, data, **guarantees with the test that enforces each**, decisions, and known limits |
| `scripts/release-v0.1.sh` | checks everything, then creates the annotated tag `v0.1` (never commits, never moves a tag) |
| `README.md` | status line, a link to the architecture doc (overwrites yours: merge by hand if you edited it) |
| `cmd/offgrid/main.go`, `exec_test.go` | the version is now `0.1.0`; the old version test accepted only `x.y.z-dayN`, so it now accepts a plain release number too |
| `docs/day-14.md` | this file |

The target list is **not in the zip**: it is outreach preparation and does not belong in a public repository. It is a separate file, `offgrid-target-list.xlsx`.

## Concepts

**An architecture document is a claim you can check.** The useful kind does not describe boxes; it states what is *guaranteed* and points at the test that would fail if it stopped being true. That is the table in section 6 of `ARCHITECTURE.md`: "a killed worker loses nothing and duplicates nothing" is not a promise, it is `TestKill9MidDrainLosesNothingAndDuplicatesNothing`. A reader (or an interviewer) can pick any row and verify it in a minute.

**Say what you did not build.** The document has a "Not promised" paragraph and a "Known limits" section, and it says plainly that the CRDT, the hybrid clock and sync do not exist yet. Readers trust the guarantees *because* the limits are stated next to them. The roadmap says the same for a README: honesty about limits is what a CTO scans for.

**Dependency direction is a design decision you can test.** The architecture table lists what each layer does *not* know about. One of those is enforced by a test (`TestQueueDoesNotImportTheDriverOrTheTaskStore` fails the build if the queue imports SQLite or the task store). A boundary that only exists in a diagram erodes; one that a test checks does not.

**A tag is a promise about a commit.** `v0.1` should point at a state you actually tested. That is why the script refuses to tag with uncommitted changes (the tag would not match what ran), refuses if the tests fail, and refuses to move an existing tag (a published tag that moves breaks everyone who used it). It makes an *annotated* tag (a real object with a message and an author), which is what releases use, instead of a lightweight one.

**A version number should be honest about what it is.** Daily builds were `0.N.0-dayN`. A release is `0.1.0`: no suffix, and below 1.0 because the sync half of the product is not built. The version test now accepts both shapes on purpose.

## The target list

`offgrid-target-list.xlsx`, 40 organizations across the six categories in section 2 of the roadmap: local-first and sync (8), on-device and edge AI (7), AI infrastructure and agents (7), field operations (6), disaster, humanitarian and defense (6), fintech and telecom in low-connectivity markets (6). Each row has a specific note on what the organization does, an *angle* for a thoughtful comment (the roadmap asks for 15 minutes of outreach prep a day from Day 15), a confidence column, and yellow cells for you: Followed?, first-comment date, link, notes. The Summary sheet counts progress with formulas.

**Read this before you rely on it.** I could not check these organizations against their own websites (the page-fetch tool needs a per-address approval that was not given). Only **Ditto** was checked live. Every other note is from my own knowledge up to mid-2026 and may be out of date; the rows marked "Verify first" are ones I know may have changed (acquisitions, rewrites). Open each company's page before you comment, and fix the sheet when it is wrong. The notes say what an organization *does*. They do not say it is hiring, and I do not know that anyone there will answer. People are not listed: find the engineers who write publicly about this problem and follow them as individuals.

## On your laptop

```bash
cd ~/offgrid                                # your Day 13 repo
unzip -o ~/Downloads/offgrid-day14.zip      # README.md is overwritten: merge by hand if you edited it
make test && make build && ./bin/offgrid version     # offgrid 0.1.0

# commit your work however you normally do, then:
scripts/release-v0.1.sh                     # checks, then tags v0.1 locally
git push origin v0.1                        # (or: scripts/release-v0.1.sh --push)
```

Optional but worth doing for the recap: on the GitHub page, create a Release from the `v0.1` tag and paste the post's first paragraph as the description.

## Week 2 recap post (use your own clip)

Days 8 to 14 in one story. Use the `kill -9` clip from `make drain-demo PAUSE=2`, and say the model is a stand-in if you record it that way.

> **Week 2 of building Offgrid: from "works offline" to "survives being killed".**
>
> Offgrid is an AI agent runtime for places where the internet comes and goes. This week:
> - It notices the network state (online, degraded, offline) without flapping.
> - Hard requests are saved in a durable queue instead of being guessed at.
> - A router decides, per message, to answer now or save for later, and says why.
> - I built a switch to break my own network on purpose: latency, packet loss, partitions, on a timer.
> - A worker drains the queue when the network returns.
> - One screen shows the whole system live.
>
> The clip: I kill the worker with `kill -9` while jobs are running, restart it, and check the database. 60 jobs, 60 done, 60 results, none lost, none recorded twice. (Sandbox test with a stand-in model.)
>
> The honest part: a job can *run* again after a crash. No design prevents that. What I guarantee is that the *recorded outcome* happens once, and jobs that change data refuse to repeat. Everything is in `ARCHITECTURE.md`, with the test next to each claim.
>
> Not built yet: the sync. Week 3 is the hybrid logical clock and the CRDT.
> v0.1 is tagged. Repo in the comments.

Shorter version for X: "Week 2 of Offgrid: killed my own worker with kill -9 mid-job. 60 jobs, 60 done, none lost, none recorded twice. A job can run again after a crash; what's guaranteed is the recorded outcome. ARCHITECTURE.md has the test behind every claim. v0.1 tagged. [clip]"

## Honest limits

- The `60 jobs, 3 kills` figure comes from the automated test in this sandbox, with a stand-in model. I have not run the worker against a real Ollama or a real network outage, and `kill -9` is not a power cut.
- `ARCHITECTURE.md` describes the code as it is at Day 14. I checked its tables against the repository (package list, tool names, schema, tests that exist), but it is prose: it will drift unless you update it when the design changes. Day 15 (the clock) will change section 5.
- I did not tag anything. I do not make commits or tags in your repository; the script does it on your machine, after you have committed.

## Self-check questions

1. Why does the architecture document list a test next to each guarantee? *So each claim can be checked, and a claim without a test is visibly weaker.*
2. Why does the release script refuse to tag with uncommitted changes? *The tag would point at a commit that is not what was tested.*
3. Why an annotated tag? *It is a real object with an author, date and message, which is what a release should be; a lightweight tag is just a pointer.*
4. Why is the version `0.1.0` and not `1.0.0`? *The sync half of the product does not exist; 1.0 would promise a finished system.*
5. What is the single most important sentence in the recap post? *The one saying a job can run again after a crash and what is guaranteed instead: it is the claim an expert will test.*

## What Day 15 builds on this

Week 3 is the core: the hybrid logical clock (Kulkarni et al.), tests for monotonicity, skew and a clock that goes backwards, and replacing the placeholder `ts` in the op log. From today, 15 minutes a day goes to the target list: follow, read, and leave one thoughtful comment.
