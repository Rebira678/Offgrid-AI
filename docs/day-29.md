# Day 29: a front page that works, and the 90-second video

**Done when:** a friend clones the repo and runs it in under 10 minutes. I can test the second half of that in a clean copy; the first half (a friend, your GitHub repo) is yours. See the checklist.

```bash
make fresh-test                       # a clean copy: build, test, both demos, timed; fails over 10 minutes
make fresh-test URL=https://github.com/YOU/offgrid    # the real thing: clone it from GitHub
make demo-gif                         # re-render the README GIF from the demo's real output
```

## What changed

| | |
|---|---|
| **README order** | pitch and the demo GIF, three-command quickstart, install, architecture diagram, **design decisions and trade-offs**, benchmarks, honest limits, what's next. The long day-by-day table moved below them, because a visitor decides in the first screen |
| `docs/day-29-demo.gif` | 68 frames, about 40 seconds, 1.6 MB. **A replay of the real output of `make converge-demo`**, line by line, not a screen recording: the pauses are chosen by `scripts/render-demo-gif.py`, not taken from the run. The title bar says so, and the caption under it does too |
| `docs/architecture.svg` / `.png` | one picture of how a message and an edit travel: understand, decide, remember, check, and what is not built |
| `scripts/fresh-clone-test.sh` | copies the repo to a temp folder (or clones a URL), then build, tests, offline demo, converge demo, `version`; each timed |
| `scripts/converge-demo.sh` | **a bug fix, found by the test above** (next section) |

## What the fresh-copy test found

The first run failed in `make converge-demo`: `database belongs to a different node: it is "n-2f114996f2df", but "node-b" was requested`. It had passed several times in my working copy.

The cause was in the demo script, not in the program: the script reads B's state hash with `offgrid sync summary` to see whether B has caught up. That read ran **before** B's own first start had created its database, and a command that opens a database that does not exist **creates it**, under a random node name because the script had not named one. B then refused to start as `node-b`. It only shows when the reader wins the race, which is why it passed on my machine and failed in the clean copy. A friend would have hit it on their first try.

Fix: the script names the node on every read (`hash_of`). I confirmed the cause by hand (`sync summary` on a missing file, then `task` as `node-b`: same error) and ran the demo 6 times in a row, all pass. **The program still has this sharp edge**: a read-only command quietly creates a database and fixes its node name. The feature list is frozen, so I did not change it; it is on the "what's next" list as something worth fixing after launch.

## What I ran, and what I could not

- `make fresh-test` on a clean copy: build 4 s, tests 86 s, offline demo, converge demo 11 s; **total 101 s**. **That is a warm machine** (Go's build cache already had SQLite compiled). On a cold machine the first build compiles SQLite from C and takes longer; I could not measure that here. Budget a couple of minutes more, plus Go's own download if it is not installed.
- Not run: a clone from GitHub (the repo is not pushed from here), another person, another operating system, Ollama.
- The architecture diagram is drawn by hand; check it still matches what you would say out loud.

## The 90-second video

Record it yourself, in your own terminal, on the EliteBook. **Say the partition is simulated.** Three clips, one voice, no music needed. Rough target: 90 seconds.

| Time | On screen | What you say (edit to your voice) |
|---|---|---|
| 0:00 to 0:10 **the problem** | a plain slide or your terminal, big text: "AI agents stop when the WiFi does." Then the sentence: "A field worker's agent that needs the cloud is useless where the signal isn't." | "Most AI agents are a thin wrapper over a cloud API. Take away the network and they stop. I wanted one that keeps working offline and still ends up consistent when it comes back." |
| 0:10 to 0:25 | terminal: `make converge-demo` (set `PAUSE=2`) up to "WiFi OFF" | "Two replicas of the same task list. A is a laptop, B is a phone. They're in sync. Now I cut the network." |
| 0:25 to 0:45 | the edits: A deleted two tasks, B edited the same task and added a note; the two `state hash` lines **different** | "While offline, both edit. A deletes two tasks and finishes another. B edits one of the deleted ones without knowing, and changes the task A finished. The hashes differ, as they should." |
| 0:45 to 1:00 | "WiFi ON", the `ok:` lines, `IDENTICAL` | "Network back. They exchange only what the other lacks. Same state, byte for byte. A delete that lost to an edit it hadn't seen: the task stays." |
| 1:00 to 1:15 **the numbers** | your own `make bench` table (**not** the sandbox one), or `offgrid bench` for tokens per second | "On this laptop, no GPU: <your number> operations to catch up, <your number> tokens a second on a 3B model, about <your number> MB of memory. Every number has the hardware next to it, and `make bench` reproduces them." |
| 1:15 to 1:30 **the call to talk** | the README top, then `store verify` failing on an edited op (from `make tamper-demo`), then the GitHub URL | "It's open source, with its limits written down: it hasn't run on a real network yet. If you build offline-first or local AI, I'd like to talk." |

Notes:

- Use the real demo, at its real speed, or the GIF (the GIF is a replay, say "replay" if you use it in a video).
- Show a number only if you ran it on your laptop. The README's tables still hold sandbox rows until you run `make bench`.
- Captions on, silent autoplay is how most people will see it.
- Cut anything that is not the point. 90 seconds is a limit, not a target.

## The post (Day 29): the teaser

Attach 15 to 20 seconds of the video: the WiFi OFF moment through IDENTICAL.

> Launching tomorrow: Offgrid, an AI agent runtime that keeps working when the internet doesn't.
>
> This clip: two replicas, the network cut, both edit the same tasks, and when it comes back they end byte-identical. No server in the middle. (The network cut is simulated on localhost; real devices are the next test.)
>
> 29 days, one Go binary, a laptop with no GPU, $0. Every number in the repo names its hardware, and the limits are written down.
>
> Tomorrow: the repo, the full 90 seconds, and what broke along the way.

Hook for everyone: *one clip, one claim, tomorrow's date.*

## Checklist

- [ ] Commit and push the Day 28 files and these. CI green is still the Day 28 goal; the release tag can wait until after Day 29 if you want it to include today's README (`v0.29.0` then).
- [ ] `make fresh-test URL=<your GitHub URL>`: the real clean-clone run. Then ask **one friend** to do the quickstart cold, watching what they trip on. That is the "done" for today.
- [ ] `make bench` on the EliteBook, so the README holds your numbers, and re-export `docs/day-25-benchmarks.png`.
- [ ] Record the 90-second video, or at least the clip for the teaser.
- [ ] Read the README as a stranger: does the first screen say what it is, show it working, and say what it cannot do?
- [ ] Post the teaser. 15 minutes of outreach, and have your 30-person list ready for tomorrow.
- [ ] Still open: the 7-8B row, `ollama --version`, the Day 26 and 27 clips.
