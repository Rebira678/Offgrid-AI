# Day 21 (light): Recap and the long form

A light day. No new code. Three jobs: re-cut the flagship video, bring the README up to date, and save a first draft of the long article. All three are in this zip except the video, which only you can record.

**Done when:** the draft article is saved. It is: `docs/article-why-crdts-for-an-ai-agent.md`.

## What is in the zip

| File | What it is |
|---|---|
| `README.md` | rewritten: honest status line, the tests section with measured counts, an "Honest limits" section, a link to the article draft, an up-to-date layout (it overwrites yours: merge by hand if you edited it) |
| `docs/article-why-crdts-for-an-ai-agent.md` | the long-form draft, about 2,000 words, with editing notes at the end |
| `docs/day-21.md` | this file: the video plan, the Week 3 post, the checklist |
| `docs/day-21-week3.{svg,png}` | the Week 3 recap picture |

Nothing in `cmd/` or `internal/` changed, and the version stays `0.20.0-day20`.

## Where the project stands (measured today)

- `go test -v ./...`: **723 test functions pass, 0 fail, 1 skips by design** (a helper-process test), in 22 packages. Subtests are not counted.
- About **16,000 lines of Go** and about **19,000 lines of tests**.
- Week 3 shipped what the roadmap asked for, on schedule: hybrid logical clock (15), LWW-Map (16), property tests (17), OR-Set (18), sync protocol (19), Checkpoint 2 (20).
- Still open from earlier weeks: the **Day 1 benchmark table, the Day 3 eval results and the Day 5 tier tables** are not in the repo, so the README's benchmark table is still empty on purpose. Running `make bench` and `offgrid eval` on your laptop fills them; the article has room for one such number.

## The re-cut flagship video

I cannot record or edit video from here. This is a plan you can follow in about two hours. Target: **90 seconds**, no voice-over needed (captions only), one idea: *the app keeps working with no signal, survives a crash, and two phones merge their edits.*

| Time | Shot | How to get it | Caption |
|---|---|---|---|
| 0:00 to 0:05 | Title card on a plain background | any editor | "Two phones. No signal. The same task." |
| 0:05 to 0:20 | Tasks logged with the WiFi icon crossed out; restart; they are still there | `make demo PAUSE=2` (Day 7). Turn WiFi off before starting, on camera | "Offline: nothing needs the internet" |
| 0:20 to 0:35 | The agent keeps working and the queue fills | `scripts/checkpoint1-demo.sh 2` (add `NET=offline` for a repeatable retake) | "Hard requests wait in a durable queue" |
| 0:35 to 0:50 | `kill -9` in the middle of a drain, restart, nothing lost or duplicated | `scripts/drain-demo.sh` | "Killed mid-drain: nothing lost, nothing duplicated" |
| 0:50 to 1:25 | The convergence clip: WiFi off, both sides edit the same task, reconnect, hashes match | `scripts/converge-clip.sh` (tmux) | "WiFi off", "both edit T1", "different state", "WiFi on", "same hash" |
| 1:25 to 1:30 | End card | any editor | "github.com/... : every number measured, every limit stated" |

Rules for the cut:

1. **Do not speed up the final hashes.** Let the two `state hash:` lines sit on screen for two seconds: that is the proof.
2. **Say it is simulated.** The convergence shot shows `FAULT INJECTION ON (simulated, not your real network)` on screen. Keep that line visible, and put "network cut simulated, localhost" in the post text.
3. Keep your terminal font large (at least 18 pt) and the window under 100 columns, so a phone screen can read it.
4. Record each segment as its own take and join them; do not try to record all ninety seconds in one go.
5. On Zorin (GNOME), `Ctrl+Alt+Shift+R` starts and stops the built-in screen recorder; the files land in your Videos folder. To join and trim without an editor you can use `ffmpeg` (`sudo apt install ffmpeg`), or use Kdenlive or Shotcut for captions.

## Week 3 recap post

Picture: `docs/day-21-week3.png`. One visual, one number, one lesson.

> Week 3 of building Offgrid, an AI agent runtime that works with no internet, on one CPU-only laptop.
>
> The week's question: two phones edit the same data while they cannot talk to each other. What happens when they meet?
>
> Six days, six layers: a hybrid clock (a wall clock alone loses later edits), per-field last-writer-wins, property tests that try to break the merge, an add-wins set for notes, a sync protocol that sends only what is missing, and Friday's checkpoint: both sides edit offline, reconnect, and the state hashes match.
>
> One number: 723 tests, about 19k lines of them for 16k lines of code.
>
> One lesson: a delete should beat only the edits its author had seen. By timestamp, it would silently destroy someone's offline work.
>
> It is still localhost and a simulated network cut, not real phones. That is Week 4.
>
> Clip below. Article draft coming. Code: github link.

Hooks: recruiters (steady, visible progress and a writing sample). Post the draft article's headline as a teaser only once you have reread and edited it.

## Before you publish the article

- Read it aloud once. It is written in your voice from what the code does; change anything that does not sound like you or that you do not agree with.
- Add your own Day 1 number (tokens per second) if you want one, and delete the "Editing notes" block at the end.
- Replace "the repository" with the link, and attach the Day 20 clip near the top.
- Every figure in it can be re-measured: the test counts with `go test -v ./... | grep -c '^--- PASS'`, the clock simulation with `go test -v -run TestManyNodesWithBadClocks ./internal/hlc`.
- The roadmap's "After Day 30" list says to turn this draft into a published piece and link it from the README.

## Checklist for today

1. Unzip over your Day 20 repo. README overwrites yours; merge by hand if you edited it.
2. Run `make check` once so you know the repo is green on your machine.
3. Record and cut the video (plan above).
4. Post the Week 3 recap. Reread the article draft and save your edits.
5. Outreach: 15 minutes. Verify each target-list entry before you comment.
6. Send me the Day 1, Day 3 and Day 5 data when you have it, so Week 4 can quote real numbers.

## Self-check

1. Why is "the state is a function of the set of ops" the idea that makes the merge simple?
2. Why does the video say the network cut is simulated, and where on screen does it say so?
3. Which claim in the article would you remove if you had not run the tests behind it?

## Tomorrow: Day 22, Tier 2 deliberate reasoning

Week 4 starts: smarter tiers, hardening and polish. Day 22 adds a worker that processes the queued hard jobs with the 7-8B local model (plus an optional cloud client if one is configured and online), and writes the results back as ops so they sync like everything else. Per the roadmap's risk rule, Tier 2 is what we would have cut had the CRDT block overrun; it did not.
