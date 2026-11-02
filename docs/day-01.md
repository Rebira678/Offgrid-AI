# Day 1: Measure before you design

Goal for today: find out what your laptop can really do, with numbers you
measured yourself, and build the tool that measures them.

By the end you have:

1. Go, Ollama and three models installed
2. A Go program (`offgrid bench`) that measures model speed honestly
3. A README with a real benchmark table, and your first public post

---

## Part 1: The concepts

### 1.1 What a local model does when you ask it something

A language model does not read words. It reads **tokens**, small pieces of
text (roughly three quarters of an English word each). "Offline" might be two
tokens, "CRDT" three or four.

Answering a question happens in two very different phases:

| Phase | What happens | Name in the benchmark |
|---|---|---|
| **Prefill** | The model reads your whole prompt in one go. All tokens are processed in parallel. | Prompt processing (tok/s) |
| **Decode** | The model writes the answer one token at a time. Each new token needs a full pass through the model. | Generation (tok/s) |

When people say "this model runs at 12 tokens per second" they mean decode.
Prefill matters too: a long prompt (a task list, a site log) can take many
seconds before the first word appears. That wait is **time to first token**
(TTFT), and it is what a user actually feels.

### 1.2 Why your laptop speed is limited by memory, not by the CPU

To produce **one** token, the CPU has to read **every weight** of the model
from RAM once. So decode speed is roughly:

```
tokens per second  <=  memory bandwidth / model size in memory
```

Back-of-envelope sanity check (estimates, not measurements). Suppose your RAM
is DDR4-3200 in dual channel, which has a theoretical peak of about 51 GB/s.
Real programs reach perhaps half to two thirds of that.

| Model | Size in RAM (approx.) | Ceiling | Realistic guess |
|---|---|---|---|
| llama3.2:1b | 1.3 GB | ~39 tok/s | 20-30 |
| llama3.2:3b | 2.0 GB | ~25 tok/s | 10-16 |
| 7-8B (4-bit) | 4.7-4.9 GB | ~10 tok/s | 4-7 |

Why this matters for design: **a model twice as big is about twice as slow**,
no matter how good your code is. That is the reason Offgrid has tiers (tiny
and fast for instant decisions, bigger and slower for queued work). Day 1 gives
you the numbers that justify that design.

Prefill is different. It processes many tokens at once, so it is limited by
raw compute (the CPU cores), and per token it is much faster than decode.

### 1.3 Quantization: why a 3B model is 2 GB and not 6 GB

A "3 billion parameter" model stores 3 billion numbers. At full precision
(16 bits, 2 bytes each) that is 6 GB. **Quantization** stores each number in
about 4 bits instead, so the model is roughly a quarter of the size, with only
a small loss in quality. Ollama's default tags such as `llama3.2:3b` are
already 4-bit quantized (shown as `Q4_K_M` by `offgrid models`).

Smaller in memory means faster decode (section 1.2) and room to fit in 16 GB
alongside your browser.

### 1.4 Cold start versus warm

The first request after a model is installed or idle has to **load the model
from disk into RAM**. That can take several seconds and has nothing to do with
how fast the model runs once loaded. Mixing it into the speed number would be
a measurement mistake, so the benchmark sends one throwaway warm-up request
first and reports that load time separately.

### 1.5 Why the benchmark is built the way it is

Every choice in `internal/bench/bench.go` removes one source of lying:

| Choice | Problem it prevents |
|---|---|
| Warm-up request, excluded from speed | Disk loading inflating the first run |
| 3 runs, report the **median** | One noisy run (a browser tab wakes up) skewing the result. A median ignores one outlier; a mean does not |
| Unique `Request ID: ...` line at the start of every prompt | Ollama caches the beginning of a prompt it has just seen, so repeating a prompt makes prefill look almost free. A different first line defeats the cache |
| Temperature 0, fixed seed | Run-to-run randomness |
| Same output cap (96 tokens) for both prompts | Comparing generation speed fairly |
| A long prompt (about 800 tokens) as well as a short one | Hiding the prefill cost, which is slow on a CPU |
| Hardware printed next to every table | Numbers nobody can interpret |

---

## Part 2: The Go you will see in the code

Read the code in this order: `internal/ollama/client.go`, then
`internal/bench/bench.go`, then `cmd/offgrid/main.go`.

**Packages and `internal/`.** Code under `internal/` can only be imported by
this module. It keeps your internals private so you can change them freely.

**`context.Context` carries cancellation and deadlines.** Every function that
waits on something takes `ctx` as its first argument. Press Ctrl-C and
`signal.NotifyContext` cancels the context; the HTTP request stops, the
benchmark returns early, and partial results are still printed. This pattern
returns on every later day (connectivity monitor, queue workers).

**Why the HTTP client has no `Timeout`.** `http.Client.Timeout` limits the
whole exchange including reading the body, so it would cut off a long
streaming answer. Limits are set per request with `context.WithTimeout`.

**Streaming.** With `stream: true` Ollama sends one JSON object per line
(called NDJSON). We read them with `json.Decoder` as they arrive. The last
line has `done: true` and the timings in nanoseconds. Counting tokens
ourselves would be guesswork; Ollama reports `eval_count` and
`eval_duration`, so `tok/s = eval_count / eval_duration`.

**Three kinds of error, handled differently.**

```go
errors.Is(err, ollama.ErrUnreachable)   // sentinel: Ollama is not running
errors.As(err, &apiErr)                 // typed: server said 404 "model not found"
errors.Is(err, context.Canceled)        // the user pressed Ctrl-C
```

Each gets different advice in `printError`. Wrapping with `%w` keeps the
original error inspectable.

**Interfaces belong to the consumer.** `bench.Generator` is declared in the
bench package, not in the Ollama package. The benchmark states the one method
it needs, `*ollama.Client` happens to satisfy it, and tests pass a tiny fake.
Remember this for Day 2, when the real `llm.Client` interface appears.

**Testing without the real thing.** `httptest.NewServer` starts a fake Ollama
inside the test process. No model, no network, instant. `main()` only calls
`os.Exit(run(...))`, and `run` takes its input and output as parameters, so
the whole CLI can be tested end to end.

**One bug the tests caught while I wrote this.** The timeout test hung
forever: Go's HTTP server only notices a client has hung up after the request
body has been read, and my fake handler never read it. The fix was one line.
Worth a sentence in your post: tests for failure paths are easy to get wrong
too.

---

## Part 3: What to do on your laptop

Time budget: about 4 hours, most of it waiting for downloads.

### Step 1: Install Go

```bash
sudo apt update && sudo apt install -y golang-go
go version
```

Zorin OS 18 is based on Ubuntu 24.04, whose Go package should be 1.22. That is
enough for this project. If `go version` is older than 1.22, install the
official tarball from go.dev/dl instead.

### Step 2: Install Ollama and pull models

```bash
curl -fsSL https://ollama.com/install.sh | sh
scripts/pull-models.sh              # 1B and 3B, about 3.3 GB total
```

Add `--with-7b` for a 7B model (about 4.7 GB more). If mobile data is
expensive, do the downloads on a cheaper connection. Models are stored once.

### Step 3: Build and test

```bash
make build
make test
./bin/offgrid models
```

### Step 4: Get honest numbers

- Plug in the charger and close Brave (its tabs use gigabytes of RAM).
- In Zorin's Power settings, choose a performance mode if available.
- Run:

```bash
make bench            # 1B and 3B, short and long prompts, 3 runs each
```

For the 7B model: `./bin/offgrid bench -models qwen2.5:7b -runs 2`.

That writes `docs/benchmarks.md` and `docs/benchmarks.json`. Paste the table
into the README.

### Step 5: Check the result makes sense

- Generation speed roughly follows the table in 1.2. If the 3B model is far
  below 8 tok/s, run `ollama ps`: the PROCESSOR column should say `100% CPU`,
  and it should not be swapping to disk (`free -h`).
- Long prompt: expect a much larger time to first token than the short one.
  That gap is the prefill cost.
- The 7B model will be slow. That is the finding, not a failure.

### Step 6: Commit and post

```bash
git init
git add .
git commit -m "Day 1: Ollama client and benchmark tool"
```

Push to a public GitHub repo, update your LinkedIn headline, and post:

> Starting Offgrid: an AI agent that works with the internet off.
> Day 1: what does a laptop with no GPU really do?
> [screenshot of the table]
> 3B model: __ tok/s. 7B: __ tok/s. Long prompts take __ s before the first word.
> Lesson: decode speed is limited by memory bandwidth, so model size decides
> everything. That shapes the whole design.

Use your real numbers and nothing else.

---

## Part 4: Check you understood

Try to answer without looking back:

1. Why is a model that is twice as large about twice as slow to generate?
2. Why do we throw away the first request?
3. Why use the median of runs rather than the mean?
4. What would happen to the long-prompt results without the `Request ID` line?
5. Why does the HTTP client not use `http.Client.Timeout`?

Answers: (1) each token reads all the weights once, so time per token scales
with model size; (2) it includes loading the model from disk; (3) one noisy
run would drag the mean; (4) Ollama would reuse its cached prompt prefix and
prompt processing would look unrealistically fast; (5) it would cut off long
streaming answers, so deadlines go on the context.

## What Day 2 builds on this

A swappable `llm.Client` interface, a streaming `offgrid ask` command, and a
clip of the answer arriving with WiFi off. Today's `internal/ollama` package is
its foundation.
