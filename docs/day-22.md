# Day 22: Tier 2, deliberate reasoning

**Done when:** a queued summary completes after reconnect or when idle. It does: `make two-brains-demo` saves a hard question offline, waits for a quiet laptop, answers it, and shows the same answer on a second device after a sync.

## The idea in one paragraph

Offgrid has a fast brain (Tier 0 rules, Tier 1 a 3B model) that answers a person who is waiting, and now a slow brain (Tier 2): a bigger local model, plus an optional cloud model, for questions like "summarize this week's incidents and suggest priorities". The slow brain must not make the fast one slow, so it runs only when the laptop is quiet. And its answer must not live in a private queue file: it is written into the task database as an **op**, so it syncs to your other devices like any edit.

## What is new

| Piece | Where | What it does |
|---|---|---|
| `answer.recorded` op | `internal/store/sqlstore` (`answers.go`, `apply.go`) | A fact: "job K was answered with these words, by this model, locally or in the cloud". Projection version 3 rebuilds the view on first open. |
| `RecordAnswer`, `Answer`, `Answers` | `answers.go` | Idempotent: a second answer for the same key writes nothing. |
| `Think` handler | `internal/drain/think.go` | Looks for an existing answer first (no model call), asks the cloud (only if configured and online) then the local model, records the answer, returns the stored words. |
| Idle gate | `internal/drain/worker.go` (`Spec.Background`, `Config.Idle`, `Config.IdleWait`) | A slow job starts only if the CPU is quiet, no fast job is running here, and no other slow job is running. Waiting costs no attempts. |
| CPU sampler | `internal/idle` | Busy share of the CPU since the last look, from `/proc/stat`. |
| Cloud client | `internal/llm/openai.go` | Any `/v1/chat/completions` server. Never on by default. |
| CLI | `drain` flags, `offgrid answers` | `-deep-model`, `-cloud-url`, `-cloud-model`, `-cloud-key-file`, `$OFFGRID_CLOUD_KEY`, `-idle-max-busy`, `-idle-wait`. |

## Why an answer is an op

An answer is a fact about the world ("at 11:21 the laptop's 7B model said X about question K"), not an edit of a task, so it never touches a task row. Three properties fall out of the design:

1. **It syncs for free.** The sync protocol moves ops; it does not know what an answer is.
2. **Every device agrees.** The view is the op set folded in one canonical order (Day 19). Two devices that answered the same question both keep the **first** answer in that order, so after a sync both show the same words. The state hash includes answers (and only when there are some, so databases without answers keep their old hashes).
3. **Crashes are cheap.** The key is the job's idempotency key. If the process dies after saving the answer but before the queue is told, the retry finds the answer and returns it without asking the model again. That matters when one call takes a minute.

## The idle gate, in plain words

A 7-8B model on a CPU-only laptop uses every core. If a person types a message while it runs, the 3B model that should answer in two seconds crawls. So a slow job starts only if all of these hold at the moment of claiming:

- the CPU was at most 50% busy over the last 2 seconds (`-idle-max-busy`),
- no fast job is running in this worker,
- no other slow job is running.

A job already running is never stopped because the machine got busy; the gate decides only whether to **start**. The first look at the CPU has nothing to compare with and says "not yet" (starting a minute of inference on a machine we know nothing about is the mistake the gate exists to prevent), so a slow job starts about 2 seconds after the worker does.

`drain -once` used to exit when nothing could run now. That would have left a gated slow job behind on every first run, so with `-once` the worker now waits for a quiet machine, but only while a **ready** slow job exists and only for `-idle-wait` (default 2 minutes). I found this by running the real binary, not from a test: the unit tests used a gate that was already open.

## The cloud model (optional, and read this twice)

Off unless you pass `-cloud-url` and `-cloud-model` together; the key comes from `$OFFGRID_CLOUD_KEY` or `-cloud-key-file`. When it is on:

- the task data in the prompt **leaves the laptop** while the network is up; the start-up text says so;
- the cloud is tried only while the network is up; if it fails, is rejected, or returns nothing, the local model answers;
- a cancelled job stops; it does not fall through to a minute of local inference;
- the key is only sent to the URL you gave, and is scrubbed from error messages.

## What is tested (and with what)

- **Store:** recording twice writes one op; an answer never changes a task; two devices answering the same question agree on the first; out-of-order arrival converges and a rebuild gives the same hash; the old hash is unchanged when there are no answers (checked against an independent copy of the Day 20 formula).
- **Worker:** a slow job waits without using an attempt; never starts beside fast work; one at a time; gate events; `-once` waits, gives up after `-idle-wait`, does not wait for a job in backoff, does not wait when no slow job is queued.
- **Handler:** local; cloud when online; fallback on every kind of cloud failure; no fallback when cancelled; an existing answer means no model call; a crash after saving does not re-run the model.
- **Cloud client:** request shape, every status mapped to the shared errors, key never in an error, mid-request cancel is not an outage; it also passes the shared client contract.
- **Whole path:** a real queue, real worker and real stores: busy machine, nothing runs; quiet, answered; ops sent to a second store; hashes equal.
- **Mutation testing:** about 45 deliberate bugs; every one is caught except mutants that cannot change behaviour (the `!gated` early-out in `-once` waiting, and a `TrimSpace` check on cloud text whose failure falls through to the same local path).

## What is NOT tested, honestly

- **No real 7-8B model, no real cloud API.** Everything ran against stand-in servers. The wiring is real; the words in the demo are canned.
- No real phone, no real WiFi (the sync in the demo is localhost).
- The idle gate reads CPU use only: a laptop busy with disk or a GPU looks quiet. On a system without `/proc/stat` it never blocks.
- The job key includes the date (Day 10), so asking tomorrow makes a new answer. Old answers are never rewritten.
- An answer is as of the time it was made. The task data it saw is not stored with it.

## Try it

```bash
make two-brains-demo                                    # stand-in model; CPU burners show the gate waiting
STANDIN=0 DEEP_MODEL=qwen2.5:7b make two-brains-demo    # your real Ollama (ollama pull qwen2.5:7b first)

offgrid agent -route -net offline -db tasks.db "summarize this week's incidents and suggest priorities"
offgrid drain -db tasks.db -once -deep-model qwen2.5:7b
offgrid answers -db tasks.db
```

## The post (Day 22)

> Two brains, one agent: fast and slow thinking on a laptop.
>
> My offline agent has a fast brain (rules in 19 microseconds; a 3B model in about 2 seconds) and a slow brain (a 7-8B model, plus an optional cloud one).
>
> The slow brain only starts when the laptop is quiet, never beside a fast job, and never twice at once. Its answer is written into the database as an op, so it shows up on my other device after the next sync, identical on both.
>
> Found one real bug by running the binary: `-once` quietly left the slow job behind, because the CPU gate's first look has nothing to compare with.
>
> Numbers are from one laptop; the 7-8B and cloud paths are tested against stand-ins, not the real thing yet.

Hook for AI engineers: *the model is the easy part; when it runs, and where its answer lives, is the design.* Attach `docs/day-22-two-brains.png`.

## Checklist

- [ ] `go get pgregory.net/rapid@v1.2.0`, commit `go.mod` and `go.sum` (still needed from Day 17).
- [ ] `ollama pull qwen2.5:7b` (or `llama3.1:8b`), then `STANDIN=0 make two-brains-demo`.
- [ ] Add the 7-8B row to the README benchmark table: `offgrid bench -models qwen2.5:7b`.
- [ ] Add `ollama --version` beside the benchmark numbers (the log you sent says `v0.40.0`; check that is right).
- [ ] 15 minutes of outreach.
