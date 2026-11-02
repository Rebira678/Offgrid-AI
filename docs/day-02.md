# Day 2: A model client you can swap

Goal for today: make the rest of Offgrid independent of any one model backend,
and prove it by answering a question with the WiFi off.

By the end you have:

1. `internal/llm`: a small interface (`Client`) that every part of Offgrid will
   use to talk to a model, with an Ollama implementation and a test double
2. `offgrid ask`: a command that streams an answer from a local model
3. A 15-second clip and a post

This zip contains **only Day 2's files**. It goes on top of your Day 1 repo
(see Part 3).

---

## Part 1: The concepts

### 1.1 Why not just call Ollama directly?

You could. It would work today and hurt every day after:

| Later day | What it needs | Why a direct Ollama call makes it hard |
|---|---|---|
| Day 5, agent loop | Test the agent logic with scripted model answers | You would have to run a real model in every test (slow, unrepeatable) |
| Day 10, router | Know that "the model backend is down" and move the request to the queue | You would be parsing HTTP errors in the router |
| Day 22, tier 2 | Use a different, bigger model, maybe a cloud API when online | Ollama details spread through the code |

An **interface** is a promise about behaviour. Code that depends only on the
promise does not care who keeps it. That is what "swappable" means: not that
swapping is possible in theory, but that it is a one-line change.

### 1.2 The `Client` interface

```go
type Client interface {
    Chat(ctx context.Context, req Request, onToken func(string)) (Response, error)
}
```

One method, on purpose. Go proverb: *the bigger the interface, the weaker the
abstraction.* A one-method interface is trivial to implement, wrap, and fake
(see `clientFunc` in `ask_test.go`, a whole fake client in four lines).

Because a method signature cannot express behaviour, the interface comment
spells out a **contract**: validate first, honour the context, stream pieces
that join to the final text, report outages and missing models in a standard
way, be safe for concurrent use. The test `TestClientContract` runs the same
checks against both implementations. Passing the contract is what makes two
clients truly interchangeable, and a future cloud client will be added to that
test with one line.

### 1.3 Chat versus generate

Day 1 used Ollama's `/api/generate` (one prompt in, text out). Today uses
`/api/chat`, which takes a list of **messages** with roles:

| Role | Meaning | Example |
|---|---|---|
| `system` | Standing instructions that frame everything | "You help field workers. Be brief." |
| `user` | The question or task | "Log a leaking pump at site 4" |
| `assistant` | What the model said earlier | (for multi-turn history) |

Two facts that shape the whole agent design:

1. **The model has no memory.** Every call sends the entire conversation again.
   "Memory" is just you re-sending history. Longer history means a longer
   prompt, which means more prefill time (Day 1, section 1.1). Small local
   models make this cost visible.
2. **Roles are formatting, not magic.** Ollama pastes the messages into a
   single text prompt using the model's chat template. The roles work because
   the model was trained on that template.

### 1.4 Streaming and the callback

The user should see words appear, not wait for the whole answer. `onToken` is
called with each piece as it arrives.

Alternatives and why I did not use them:

| Style | Trade-off |
|---|---|
| **Callback** (used) | Simplest. The contract says it runs on the caller's goroutine, one call at a time, so you need no locks |
| Channel | You must manage a goroutine, decide what happens if the reader is slow, and remember to close it |
| Iterator (`range` over a function) | Elegant, but needs Go 1.23; this project targets 1.22 |

### 1.5 Why turning WiFi off changes nothing

`127.0.0.1` is the **loopback** address. Packets sent to it never reach the
network card; the operating system hands them straight to the other program on
the same machine. Ollama listens there, so `offgrid ask` talks to Ollama
entirely inside your laptop. Inference itself needs no internet; only
`ollama pull` does.

This is the first piece of real evidence for the whole project's claim.

### 1.6 Errors are part of the interface

Callers need to tell three situations apart, because each needs a different
reaction:

| Situation | Error | Reaction |
|---|---|---|
| Backend not reachable | `llm.ErrUnavailable` | Retry later. **Day 10: the router queues the request** |
| Backend fine, model not installed | `*llm.ModelNotFoundError` | Retrying is pointless; tell the user to pull the model |
| Took too long | `context.DeadlineExceeded` | A slow model is not an outage; do not switch to offline mode |

The Ollama adapter **translates** Ollama's own errors into these at the
boundary (`translateError`). Nothing outside `internal/llm` imports the
`ollama` package. Look at how the unavailable error is built:

```go
return fmt.Errorf("%w: %w", ErrUnavailable, err)
```

Two `%w` verbs keep both errors in the chain, so `errors.Is` finds
`ErrUnavailable` while the message still says which address failed.

### 1.7 "Zero means default" (and its trap)

In Go, an unset struct field is its zero value, so zero has to mean something
sensible. `Request.MaxTokens == 0` means "no limit". `Request.Seed == 0` means
"random". But Ollama treats `num_predict: 0` and `seed: 0` literally, so the
adapter translates (`-1` for unlimited, a random number for the seed). If it did
not, "no limit" would silently mean "write nothing", and every call would
produce the same text. The tests check both translations.

`Temperature: 0` is different: it is a real setting (always the likeliest
token), and the right one for agent decisions on Day 3.

### 1.7b Where interfaces belong (a correction to Day 1)

Day 1 said: declare an interface where it is *used*. `bench.Generator` follows
that rule: one consumer, one tiny need.

`llm.Client` is deliberately declared with its own types instead, because it is
the **shared port** of the whole program: many consumers (agent, router,
workers) and several implementations (Ollama, Fake, later a cloud client) all
meet at it. The rule of thumb: an interface used by one consumer lives with the
consumer; an interface that is the central seam of the system lives in its own
package with its domain types.

---

## Part 2: A Go and testing lesson worth keeping

**Break your code on purpose to test your tests.** After writing the adapter
tests I temporarily changed the code in two ways: stopped translating
"unreachable" into `ErrUnavailable`, and sent "no limit" as `0` instead of `-1`.
Each change made a specific test fail, which proves those tests can actually
catch that bug. (My first attempt did not even compile, so it proved nothing;
the second attempt did.) A test that cannot fail is decoration.

Also worth reading:

- `internal/llm/fake.go`: a test double in a normal file, exported, because
  later days reuse it. Compare `net/http/httptest`.
- `var _ Client = (*Ollama)(nil)`: a compile-time check that the type still
  satisfies the interface.
- `cmd/offgrid/ask.go`: `cmdAsk` handles the outside world (flags, signals,
  the timeout, choosing the backend) and `runAsk` holds the logic and accepts
  any `llm.Client`. The test calls `runAsk` with a fake: no HTTP, no Ollama.
- `internal/ollama/client.go`: the streaming loop is now shared by `Generate`
  and `Chat`. Day 1's tests did not change and still pass; that is what
  refactoring under tests looks like.

---

## Part 3: What to do on your laptop

### Step 1: Add the Day 2 files

Unzip over your Day 1 repo (it will replace two existing files, `main.go` and
`client.go`, and add the rest):

```bash
cd ~/path/to/offgrid
unzip -o ~/Downloads/offgrid-day2.zip
```

If you renamed the Go module (the first line of `go.mod` is not `module
offgrid`), fix the imports in the new files:

```bash
grep -rl '"offgrid/' --include=*.go . | xargs sed -i 's#"offgrid/#"YOUR/MODULE/PATH/#'
```

If you edited `main.go` or `client.go` yourself on Day 1, merge instead of
overwriting.

### Step 2: Test and build

```bash
make test
make build
./bin/offgrid ask -stats "In one sentence, what is a CRDT?"
```

Add `-model llama3.2:1b` for a faster answer. Flags go **before** the prompt.

### Step 3: Try the other ways to use it

```bash
./bin/offgrid ask -system "Answer in five words or fewer." "Why is the sky blue?"
echo "Summarize: pump P-12 is leaking at site 4" | ./bin/offgrid ask
./bin/offgrid ask "write a haiku about WiFi" > haiku.txt    # only the answer is saved
./bin/offgrid ask -model nonexistent "hi"                   # see the friendly error
```

Press Ctrl-C during a long answer: it stops, finishes the line, and exits with
code 130.

### Step 4: Record the clip (about 15 seconds)

1. Screen recorder running, with the top bar's WiFi icon visible.
2. Turn WiFi off: `nmcli radio wifi off` (or the quick-settings toggle).
3. Prove you are offline: `ping -c1 -W2 1.1.1.1` fails.
4. Run `./bin/offgrid ask -stats "Explain in two sentences why offline AI matters."`
   The answer streams in.
5. Turn WiFi back on: `nmcli radio wifi on`.

### Step 5: Add to the README, commit, post

Add under Quickstart:

```markdown
./bin/offgrid ask "what is a CRDT?"    # ask a local model; works with WiFi off
```

```bash
git add .
git commit -m "Day 2: swappable llm.Client, Ollama adapter, offgrid ask"
```

Post template (use your real numbers):

> Day 2 of Offgrid: WiFi off, AI still answering.
> [clip]
> The model runs on my laptop, so the internet is irrelevant.
> The part I am proudest of is not the answer, it is the interface behind it:
> one 1-method Go interface, a real Ollama client, a fake one for tests, and a
> contract test that runs the same checks on both. Swapping models is one line.
> __ tokens/sec on a CPU-only laptop.

---

## Part 4: Check you understood

1. Why does the `Client` interface have only one method?
2. What is the difference between `ErrUnavailable` and `context.DeadlineExceeded`,
   and why must the router treat them differently?
3. Why does `Request.MaxTokens == 0` need translating before it is sent to Ollama?
4. The model has no memory. How does a chat appear to remember?
5. Why does the contract test run against the Fake as well as the real client?

Answers: (1) small interfaces are cheap to implement, wrap and fake;
(2) the first means nothing answered at all (queue the work for later), the
second means something answered too slowly (a slow model is not an outage);
(3) Ollama would read 0 literally, so unlimited must be sent as -1;
(4) the whole conversation is re-sent every call; (5) so the fake used by every
later test provably behaves like the real thing, and so any future backend can
be checked with the same assertions.

## What Day 3 builds on this

Day 3 makes the model reliable: forcing JSON output, validating it, retrying
a bounded number of times, and an eval set that measures valid-JSON rate and
intent accuracy on the 1B and 3B models. It uses `llm.Client` and `llm.Fake`
exactly as defined today.
