# Day 5: Don't call an LLM when a regex will do

Goal for today: build the loop that decides **who** reads a message, and measure
what that decision saves. Rules first, the model only for what the rules cannot
read, and every request labelled with the tier that handled it and its latency.

By the end you have:

1. `internal/agent`: Tier 0 rules, Tier 1 model, one loop around the Day 4 tools
2. Per-request tier and latency, per-tier statistics, an optional JSONL log
3. `offgrid agent`, including `-eval` to push the 30 labelled messages through it
4. **Your** rules-versus-model latency numbers, and a post

This zip contains **only Day 5's files**. It goes on top of your Day 4 repo
(Part 4).

> **Honesty note.** The rules run for real in my sandbox, so the Tier 0 numbers
> below (coverage, correctness, microseconds) are measured, not simulated. I
> could **not** run a real Ollama model, so I have no real Tier 1 latency. The
> seconds-per-message figure in your post must come from your laptop. A
> simulated model I used only to test the plumbing answers in milliseconds;
> ignore any number that came from it.

---

## Part 1: The concepts

### 1.1 Why tiers

A small model on your CPU takes seconds per message and sometimes gets it wrong.
A regular expression takes microseconds and is never *randomly* wrong. Most
messages in a real work queue are plain: "what's still open?", "new task: ...",
"close T3". The expensive, slow, fallible tool should be the fallback, not the
default.

```
message
  |
  v
Tier 0: rules ------- match --> Command --+
  |                                       |
  no match                                v
  |                                  tools.Registry (Day 4: validate, then act)
  v                                       ^
Tier 1: model (Day 3 classifier) -> Command
```

Both tiers end at the **same registry**. A rule that misfires is contained
exactly as a model that misfires is: the Day 4 walls do not care who proposed the
command.

### 1.2 The cost of being wrong is not symmetric

This is the design idea of the day, and the thing to put in front of a founder:

| | Cost |
|---|---|
| A rule **misses** a message | a few seconds (the model handles it) |
| A rule **misreads** a message | a wrong action, with no warning |

So the rules are built for **precision over coverage**. A rule only needs to be
right *when it speaks*. Silence is always safe.

### 1.3 Closed-vocabulary rules: "understand every word or say nothing"

The list and summary rules do not search for keywords. They read the *whole*
message and fire only if **every word is one they know**. Any unfamiliar word
makes the rule stay silent.

Why this matters: a keyword rule seeing "show tasks **not** done" finds "show",
"tasks", "done" and lists the *finished* tasks, the exact opposite of what was
asked. The closed-vocabulary rule meets "not", does not know it, and passes the
message to the model. The same goes for "except", "last week", "urgent" (which is
not one of our three priorities) and so on. A test lists 47 such near-misses
and requires silence on all of them.

The create, note and update rules are **frames**: they require an explicit shape
("new task: ...", "note on T3: ...", "mark X as done") and refuse references that
look like longer sentences ("the pump at site 4 because it leaks").

Small details that matter:

- A **site code must contain a digit** (`4`, `A4`, `B7`). "site visit" and
  "site for today" are ordinary English. A letters-only site name goes to the
  model, which can read the surrounding sentence. (I found this bug *after* the
  first version: "new task: check the site visit" would have been stored as site
  `visit` with the title cut to "check the". A mutation test now pins it.)
- A title or note keeps the worker's **own capitalisation**. Lower-casing is for
  matching only.
- Politeness ("please", "could you") and trailing punctuation are stripped.

### 1.4 What the loop does, in order

1. An **empty message** never reaches the model (it would cost seconds to learn
   nothing).
2. **Tier 0**: if a rule matches, run the command.
   - If the registry **refuses because the task is missing or ambiguous**, that
     answer stands. The model cannot know which tasks exist, so asking it would
     only cost time. Example: `close the pump ticket` with two pump tasks gets
     "matches 2 tasks; say which one", from a rule, in microseconds.
   - If the registry **refuses the command itself** (an `ArgumentError`), that is
     a bug in the *rule*, not the worker's fault. The loop records why in
     `Fallback` and lets the model try. (Logs will show which rule needs fixing.)
3. **Tier 1**: no rule matched, so ask the model (Day 3's classifier, with its
   own retries). Then run the command.
4. Every step is timed: *understand* (the rule or the model), *execute* (the
   registry), *total*.

### 1.5 Degrading gracefully: the offline story

This is the point of the whole project, shown in one behaviour. **If Ollama is
not running, the rules still work.** "new task: check the fuel line" succeeds
with no model at all. Only the messages that need understanding fail, with a
message that says what to do ("try a plainer phrase, such as 'list open
tasks'"), not a stack trace. Tests cover both a missing model (`-no-model`) and
an unreachable one.

### 1.6 Five outcomes, one word each

| Status | Meaning |
|---|---|
| `ok` | understood and carried out |
| `refused` | understood, but the code said no (missing or ambiguous task, invalid command) |
| `declined` | understood: this is not something Offgrid does (`unknown`) |
| `not_understood` | the model produced no valid command in its attempts |
| `failed` | infrastructure: no model, model down, timeout, cancelled |

Keeping these apart is what lets you later tell "the model is bad" from "the
server is down" from "the worker asked for something odd".

### 1.7 Measure latency honestly

Latency is measured with an injected clock (`Agent.Now`), so tests use a fake
clock that moves exactly 1 ms per reading and assert exact numbers: no
`time.Sleep`, no flaky timing tests. The tables show **median** and **slowest**,
not average: one 30-second model stall would swamp an average and say nothing
about the typical request. Units adapt (`µs`, `ms`, `s`) so a rule's 40 µs and a
model's 6 s are both readable.

---

## Part 2: What I measured, and how fair it is

Evaluation set: the 30 labelled messages from Day 3.

**Tier 0 alone** (`offgrid agent -no-model -eval`), all measured:

| Version of the rules | Handled by rules | Matched the label |
|---|---|---|
| **First version, written before I looked at the 30 cases** | 14 of 30 | 13 of 14 |
| After four general fixes made after seeing the results | 16 of 30 | 16 of 16 |

Read this carefully, because it is the honest part:

- The first row is the **fair estimate** of how rules written from general
  knowledge do on messages they have not been tuned to. The one miss was a label
  question (`end of day report` implies *today*; the rule left `when` empty,
  which behaves identically but differs from the label).
- The second row is **optimistic**. I made four fixes (an end-of-day report
  means today; "X is now done"; "set the priority of X to Y"; and the ID `T-12`
  now resolves to `T12`) after seeing which cases failed. They are all ordinary
  phrasings, but I chose them *because* of this set, so 16 of 30 is not a
  prediction for unseen messages. This is the Day 3 lesson, again: do not grade
  something on the data you used to build it.
- The 14 messages the rules stay silent on are the point of Tier 1: "I finished
  replacing the gasket", "what did I log yesterday?", "log a leaking pump at site
  4" (no explicit "task"), misspellings ("lst my in progres taks"), and the three
  requests the app should decline.
- Tier 0 median latency: **tens of microseconds** (measured on my sandbox; yours
  will differ by a small factor, not by orders of magnitude).

**Your job on the laptop:** run it with the real model and record the Tier 1
median. That gap (microseconds against seconds) is the post.

---

## Part 3: A Go lesson worth keeping

- **A consumer-side interface for the model.** The agent needs one method,
  `Classify`, so it declares `Model` itself. Day 3's `intent.Classifier` happens
  to satisfy it; tests pass a fake. The agent imports no network or Ollama code,
  and a test parses the real imports to enforce it.
- **Never-fails API.** `Handle` returns an `Outcome`, not `(Outcome, error)`.
  Every way it can go wrong is *data* (`Status`, `Err`), so the caller has one
  thing to inspect and cannot forget to check an error.
- **Table tests with negatives.** The most valuable test today is the list of
  messages that must produce *no match*. Positive tests prove a rule works;
  negative tests prove it is safe.
- **Fuzzing** (`FuzzRules`). Go's built-in fuzzer fed the rules about 87,000
  random and mutated strings in 20 seconds: no panic, and every accepted message
  named a real intent. Run it longer yourself:
  `go test -run xxx -fuzz FuzzRules -fuzztime 60s ./internal/agent`
- **Mutation testing, and its limits.** I broke the loop and rules deliberately
  and each break was caught by a named test. Three of my first attempts only
  failed to *compile* (an unused variable), which proves nothing, so I redid them
  in a form that compiles. Two mutations *survived*: one was a genuine test gap
  (the clock was readable twice on the model path; I added a test), the other
  was **redundant protection** rather than a gap: "what is done" is kept out of
  the update rules by two independent guards (rule order *and* the
  pronoun/question deny-list), so removing one changes nothing. That is two
  walls on purpose.
- **A fake clock that ticks.** `ticker` advances a fixed step on every read, so
  "understand 1 ms, execute 1 ms, total 4 ms" is an exact assertion, and it
  caught a counting mistake in my own first expectation.

---

## Part 4: What to do on your laptop

### Step 1: Add the Day 5 files

```bash
cd ~/path/to/offgrid
unzip -o ~/Downloads/offgrid-day5.zip
```

This **replaces five existing files**: `cmd/offgrid/main.go`,
`cmd/offgrid/exec_test.go` (its version check no longer hard-codes a day),
`internal/eval/eval.go` (one new method, `Case.Correct`),
`internal/task/resolve.go` (IDs like `T-12`) and `internal/task/task_test.go`.
If you edited any of them, merge instead of overwriting. Everything else is new.

### Step 2: Test and build

```bash
make test
make build
./bin/offgrid version        # offgrid 0.5.0-day5
```

### Step 3: See the tiers with no model at all

```bash
./bin/offgrid agent -seed -no-model < examples/day5-messages.txt
./bin/offgrid agent -no-model -eval
```

The first shows rules answering in microseconds, the loop refusing to guess
between two pump tasks, and a polite failure for what needs the model. The
second is the Tier 0 coverage table above, on your machine.

### Step 4: Run it with the real model (this is your post)

Make sure Ollama is running and `llama3.2:3b` is installed (close Brave, plug in
the charger):

```bash
./bin/offgrid agent -eval -model llama3.2:3b -log docs/day5-requests.jsonl
```

This is slow: about fourteen messages go to the model. Let it run. At the end you
get one line per case with its tier and latency, then the per-tier table. Save
it:

```bash
./bin/offgrid agent -eval -model llama3.2:3b > docs/day5-eval.txt
```

Then try it live, mixing both kinds of message:

```bash
./bin/offgrid agent -seed "what's still open from yesterday?"      # a rule
./bin/offgrid agent -seed "log a leaking pump at site 4, high priority"   # the model
```

And the offline demonstration: **stop Ollama** (`sudo systemctl stop ollama`) and
run the first line again. It still works. Start it again afterwards
(`sudo systemctl start ollama`).

### Step 5: If something goes wrong

- **Every model message fails with "not reachable".** Ollama is not running:
  `systemctl status ollama` or `ollama serve`.
- **A rule answers when you expected the model.** Good, that is the point. The
  line shows `rule <name>`; if the rule was *wrong*, that is a bug worth a test
  in `rules_test.go` (add the message to the "stay silent" list and fix the
  rule).
- **A message you think the rules should read goes to the model.** That is
  safe. Add a case to the "understand plain requests" list only if you are sure
  it cannot be misread.

### Step 6: Commit and post

```bash
git add .
git commit -m "Day 5: tiered agent loop, rules before model, per-request tier and latency"
```

Post template (real numbers only):

> Day 5 of Offgrid: don't call an LLM when a regex will do.
> My agent has two tiers. Tier 0 is plain rules: [N] of 30 test messages, median
> [X] µs. Tier 1 is a 3B model on a CPU: median [Y] s.
> That is roughly [Y/X]x faster, and the rules are never randomly wrong.
> The catch: rules MISREADING is worse than rules MISSING, so they only speak when
> they understand every word. "show tasks NOT done" goes to the model.
> And if Ollama is down, the rules still work. Offline-first, not offline-only.
> Honest caveats: 30 cases; my rules are tuned to plain phrasing; the first
> version of the rules (written blind) handled 14 of 30 and the tuned version 16.
> [screenshot of the tier table]

Do not write a ratio your table does not show.

---

## Part 5: Check you understood

1. Why is a rule that misses better than a rule that misreads?
2. Why must the list rule refuse "show tasks not done" instead of just finding the
   keywords?
3. A rule matches "close the boiler ticket" but there is no boiler task. Why does
   the loop *not* ask the model?
4. A rule produces a command the registry rejects as invalid. Why fall back to the
   model here, when it would not for a missing task?
5. Why report the median and not the average latency?

Answers: (1) a miss costs seconds, a misread causes a wrong action with no
warning; (2) keywords would find "show", "tasks", "done" and list the finished
tasks, the opposite of the request, while an unknown word ("not") keeps the rule
silent so the model reads the whole sentence; (3) the model cannot know which
tasks exist, so asking it only adds seconds to the same answer; (4) an invalid
command means the *rule* is faulty, not the worker's request, and the model may
still read the message correctly (the loop records which rule to fix); (5) one
long stall would drag the average far from what a typical request feels like,
while the median ignores outliers.

## What Day 6 builds on this

Day 6 replaces the in-memory store with an **event-sourced SQLite store**: every
change is an immutable operation in an append-only `ops` table, and the `tasks`
view is rebuilt from it (delete the view, rebuild it, get identical state). The
`task.Store` interface from Day 4 and its 13-behaviour contract test are what
make that swap safe: the SQLite store must pass the same tests, and the tools and
the agent loop do not change.
