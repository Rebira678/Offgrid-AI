# Day 3: Make the model reliable

Goal for today: turn "the model usually gives me JSON" into a measured,
defended guarantee, and find out which model you can actually trust to drive
an agent.

By the end you have:

1. `internal/structured`: a generic function that gets a validated Go value out
   of a model, with feedback-driven, bounded retries
2. `internal/intent`: worker message in, one validated command out
3. `internal/eval`: 30 labelled messages, a runner, and honest statistics
4. `offgrid parse` and `offgrid eval`
5. A table of **your** 1B vs 3B numbers, and a post

This zip contains **only Day 3's files**. It goes on top of your Day 2 repo
(Part 4).

> **Honesty note.** I could not run any of this against a real Ollama model.
> The code is tested against fake model servers, including one that imitates a
> flaky small model, so the logic and the report are verified. The numbers in
> your table must come from your laptop. Nothing below predicts them.

---

## Part 1: The concepts

### 1.1 Why models get format wrong

A language model does not "write JSON". It picks one token at a time from a
probability distribution, and JSON is unforgiving: one stray sentence before the
brace, a missing quote, a trailing comma, and a parser rejects the whole thing.
Small models, like the 1B and 3B ones you run, follow format instructions less
reliably than large ones. Nothing about this is a bug to wait out; it is how the
technology works. Reliability has to be **engineered around it**.

### 1.2 Three layers of defence

Each layer catches what the previous one cannot:

| Layer | What it guarantees | What it cannot catch | Where |
|---|---|---|---|
| 1. **Constrain** the generation | The reply is well-formed JSON of the right shape | A wrong *meaning*: right shape, wrong intent, empty title | `llm.Request.ResponseSchema` |
| 2. **Validate** in code | The content makes sense for the intent | Bad luck: the model may simply keep failing | `intent.Validate` |
| 3. **Retry** with feedback, then give up | A second chance, told exactly what was wrong | Infinite loops: it stops after a fixed number of tries | `structured.Complete` |

Remember the order: shape, then meaning, then second chances. A schema makes
you feel safe; validation is what actually makes you safe.

### 1.3 How constrained decoding works

At every step the model scores every token in its vocabulary. Normally the
sampler picks among the high scorers. With a schema, Ollama first **masks out
every token that would break the schema** at that point. After `{"intent":"`
the only tokens allowed are the beginnings of the six legal intents. The model
cannot produce `"intnet"` or a chatty sentence, because those tokens are not on
the menu.

What this does **not** do:

- It does not make the model right. `"intent":"update_task"` is perfectly
  well-formed even when the worker wanted a summary.
- It cannot rescue a reply that is cut off by the token limit. That is one
  reason `MaxTokens` is generous (200 for a command of about 60 tokens).
- You still validate everything. A backend without constraint support (a future
  cloud client, for instance) ignores the schema, which is why the validator and
  the retry loop exist.

### 1.4 Design decisions in the command format

These choices exist because small models are weak at format:

- **Flat, all-string fields**, not nested objects. Fewer ways to go wrong.
  "Not applicable" is `""`.
- **`intent` comes first** in the schema. The model fills keys in order, so it
  decides what the message means before choosing details. Go maps lose key
  order, so the schema is written as text and a test keeps it in step with the
  Go struct and the validator.
- **An `unknown` intent.** Without an honest way to say "this is not
  something the app does", the model must force "delete everything" into the
  nearest real intent. Giving it an exit is a safety feature.
- **Few-shot examples as conversation turns.** Six worked examples teach the
  format far better than description alone. None of them may appear in the
  evaluation set (a test enforces it), or you would be testing memory.
- **Static text first, the worker's message last.** Ollama reuses the already
  processed beginning of a prompt, so the long fixed part is read once and each
  new message only pays for itself. This is the Day 1 prefill lesson applied.

### 1.5 Validation: shape is not meaning

`intent.Validate` checks what a schema cannot: `create_task` needs a title,
`update_task` needs a task and something to change, `add_note` needs both a task
and a note, enums are legal, text is not absurdly long. It reports **every**
problem at once, in sentences written for the model to read on the retry:

> create_task needs a non-empty title; priority "urgent" is not allowed; use one of: low, medium, high, or leave it empty

It also normalizes harmless variations ("In Progress" becomes `in_progress`,
"Site 4" becomes `4`). Normalizing reshapes what is there; it never invents.

### 1.6 Retries: when, how, and when not

- **Retry with feedback, not blindly.** At temperature 0 the same input gives the
  same output, so asking again changes nothing. The retry sends the model its own
  bad reply and the reason it was refused.
- **Bounded.** Three attempts by default. A model that failed twice with
  feedback rarely succeeds on the fourth, and every retry costs a full model
  call, which on your CPU is seconds.
- **Retry bad replies only.** If the backend is down, the model is missing or the
  call times out, `Complete` returns immediately. That is infrastructure: the
  queue (Day 9) handles it with backoff. Mixing the two would hammer a dead
  server and hide the outage the router (Day 10) needs to see.
- **An empty reply must not crash the retry.** Every message needs content, so
  an empty reply is echoed back as "(empty reply)". A test exists because this
  is exactly the bug that appears at 2 a.m.

### 1.7 Evaluation: measure, don't feel

A unit test asks "is my code right?". An **evaluation** asks "how often is the
model right?". They need different tools. `offgrid eval` runs 30 labelled
messages through the real pipeline and counts:

| Column | Meaning |
|---|---|
| Clean JSON, 1st try | The first reply was exactly a JSON object, with nothing around it |
| Valid, 1st try | After cutting any surrounding text, it passed all the checks |
| Valid after retries | Some attempt, within the limit, was accepted |
| Intent correct | The final intent matched the label (a failure counts as wrong) |
| Arguments correct | Right intent **and** every labelled argument matched |

**Why confidence intervals.** With 30 cases, "90%" means 27 of 30, and the true
accuracy could plausibly be anywhere from about 74% to 97%. Every figure is
printed with its 95% interval (the Wilson interval). **If two intervals overlap,
you may not claim one model beats the other.** At n = 30, 25/30 and 27/30 cannot
be told apart; a test pins that fact. A thoughtful engineer or investor will
trust a result that says so.

**The ablation.** `-ablation` runs three setups per model so you can see what
each technique is worth, not just the total:

1. prompt only (the model is only *asked* for JSON)
2. + schema
3. + schema + retries

This produces the most shareable result of the day: a before-and-after that
shows what engineering added.

**Limits to state openly:** 30 cases is small; the labels are my judgment (a few
are debatable, such as "close the ticket" meaning `done`); the messages are
English only. Real field messages, in your workers' real languages, are the
obvious next dataset. Small models usually do worse on low-resource languages,
and measuring that honestly would be a valuable finding.

---

## Part 2: A Go lesson worth keeping

**Generics fit here.** `structured.Complete[T any]` works for any type: today a
`Command`, tomorrow a tool-call plan. The `validate func(*T) error` parameter
lets the validator both check and tidy the value, with no reflection and no
interface wrappers.

Also worth reading:

- `fmt.Errorf("%w after %d attempts: %w", ErrNoValidReply, n, lastErr)`:
  two wrapped errors, so callers can test for "gave up" *and* inspect why.
- `//go:embed cases.json` bakes the dataset into the binary. `go build` gives you
  one file that carries its own test set.
- `TestSchemaMatchesStructAndValidator` uses `reflect` to compare three separate
  descriptions of the same thing and fails when they drift. Use this pattern
  whenever the same fact lives in two places.
- I again broke the retry logic on purpose, four ways (no feedback sent, outages
  retried, unknown keys tolerated, empty replies echoed). Each was caught by a
  specific test.

---

## Part 3: Try it by hand first

```bash
./bin/offgrid parse "log a leaking pump at site 4, high priority"
./bin/offgrid parse -v "what's still open from yesterday?"     # shows every attempt
./bin/offgrid parse "tell me a joke"                           # expect intent: unknown
./bin/offgrid parse -schema=false -v "delete everything"       # prompt alone, watch it ramble or comply
```

Try `-model llama3.2:1b` against `-model llama3.2:3b` on the same messages and
form your own guess *before* the eval gives you the numbers.

---

## Part 4: What to do on your laptop

### Step 1: Add the Day 3 files

```bash
cd ~/path/to/offgrid
unzip -o ~/Downloads/offgrid-day3.zip
```

This **replaces** five existing files: `cmd/offgrid/main.go`,
`cmd/offgrid/ask.go`, `internal/llm/llm.go`, `internal/llm/ollama.go` and
`internal/ollama/chat.go`. If you edited any of them, merge instead of
overwriting. If you renamed the Go module, fix the imports:

```bash
grep -rl '"offgrid/' --include=*.go . | xargs sed -i 's#"offgrid/#"YOUR/MODULE/PATH/#'
```

### Step 2: Test and build

```bash
make test
make build
```

### Step 3: Smoke-test the evaluation with a few cases

```bash
./bin/offgrid eval -limit 5
```

Each case is a real model call with a prompt of several hundred tokens, so a full
run is slow on a CPU, and `-ablation` runs every case three times per model. Start
small, then run the real thing when you have time (let it run while you do
something else; Ctrl-C keeps the runs that finished):

```bash
./bin/offgrid eval -ablation -md docs/eval.md -json docs/eval.json
```

Close Brave and plug in the charger first, as on Day 1.

### Step 4: If something goes wrong

- **`error: ollama: HTTP 400 ...` mentioning the format or schema.** Your Ollama
  may be too old for structured outputs. Update it by re-running
  `curl -fsSL https://ollama.com/install.sh | sh`, then try again. To confirm
  the rest of the pipeline works, run `./bin/offgrid parse -schema=false "..."`.
- **Everything fails the same way.** Run `parse -v` on one message and read what
  the model actually said; the rejection reason is printed next to it.
- **A model scores far worse than expected.** Check the "misses" section of the
  report before concluding anything. A wrong label or an ambiguous message is a
  finding about the dataset, not the model. Edit `internal/eval/cases.json`
  (keep each message unique and rebuild); `make test` re-checks the dataset.

### Step 5: Read your results like an engineer

Before posting, answer these in writing (they become your post):

1. Does the schema raise "clean JSON" to about 100%? By how much, from what?
2. Do retries raise "valid after retries" above "valid 1st try"? How many cases
   were rescued?
3. Do the 1B and 3B **intervals overlap**? If yes, you cannot say one is better.
   Say so.
4. Which cases does *every* setup miss? Are they the model's fault or the label's?
5. What does the extra accuracy cost in time (median seconds per case, average
   tries)?

### Step 6: Commit and post

```bash
git add .
git commit -m "Day 3: structured output, intent validation, evaluation harness"
```

Post template (your real numbers only):

> Day 3 of Offgrid: can a 3B model on a laptop with no GPU drive an agent?
> [table or chart screenshot]
> Three layers: constrain the output to a schema, validate the meaning in code,
> retry with feedback (max 3).
> Valid commands: __% prompt-only, __% with schema, __% with retries.
> Honest caveat: 30 test cases, so the real accuracy is somewhere in a wide
> range, and I will not claim 3B beats 1B unless the intervals separate.
> Lesson: a schema guarantees SHAPE, not TRUTH. Validate the meaning in code.

Do not write a claim your table does not show.

---

## Part 5: Check you understood

1. A schema makes the reply well-formed JSON. Why is that not enough?
2. At temperature 0, why is retrying with the identical prompt pointless?
3. Why does `Complete` not retry when the backend is unreachable?
4. Model A scores 25/30 and model B 27/30. Which is better?
5. What goes wrong if an example from the prompt also appears in the test set?

Answers: (1) it guarantees shape, not meaning, so a command can be well-formed
and still empty or wrong; (2) the same input gives the same output, so the retry
must change the input, which feedback does; (3) an outage is infrastructure,
owned by the queue and the router, and retrying here would hammer a dead server
and hide the outage; (4) you cannot tell, because the 95% intervals (about
66-93% and 74-97%) overlap; (5) the model is graded on something it was shown
the answer to, so the score measures memory, not understanding.

## What Day 4 builds on this

Day 4 adds the tools: `create_task`, `update_task`, `list_tasks`, `add_note`,
`daily_summary`, each with typed arguments and its own tests, run **without any
model**. Today's validated `Command` is exactly what Day 4's executor will
accept: the model proposes, the code disposes.
