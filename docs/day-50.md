# Day 50: the model layer, measured

What was already there: a `Client` interface, Ollama and OpenAI-compatible backends, retry with feedback, a 30-case eval. What this day added: more places a model can live, a pinned prompt, 105 cases, and a gate that fails the build when the valid-command rate drops. Decisions are in [ADR 016](adr/016-providers-prompt-versions-and-the-eval-gate.md).

## Providers

```
offgrid ask   -host llamacpp@http://192.168.1.20:8080 "..."      llama.cpp's server on another node
offgrid agent -host ollama@http://laptop.lan:11434 ...           Ollama on the machine with the model
offgrid eval  -host openai@https://api.example.com -models gpt-x  any OpenAI-compatible endpoint
export OFFGRID_PROVIDER=llamacpp@http://pi-full.lan:8080         the same, once, for every command
export OFFGRID_API_KEY=...                                        only sent over https or a private network
```

A bare address is Ollama as before. A provider on another machine is retried (3 tries, full-jitter waits under a 2 s cap, only on "unavailable", never after a reply began streaming); a local one is not. The OpenAI-compatible client now sends the schema as `response_format`, and if a server answers 400 to it, repeats the call once without and remembers.

## Prompt versioning

`intent.PromptVersion` is "2" and `PromptFingerprint()` is `9d865728ff30`. A test pins it: edit the prompt, an example or the schema and the build fails until you bump the version and update the pin. Every report says `Prompt: v2 (9d865728ff30), cases: 8fa84c0de958`, and so does every baseline and history row.

## The 105 cases

The 30 cases became 105 (at least 8 per intent, typos and casual phrasing included, unique texts). With 105 cases a rate has a Wilson interval of about +-6 points at 90%, versus +-12 with 30. That is still wide: **a 2-point change is still inside the noise for a real model**, which is why the real-model margin is 5 points.

## The gate

`go test ./internal/eval` (and the `eval.yml` workflow on every push) runs the cases through the real prompt, schema, JSON repair, validator and retry loop against a deterministic reference model with scheduled faults, in three setups, and compares with `internal/eval/baseline.json`:

| Setup (reference model) | Valid, first try | Valid after retries | Intent | Arguments |
|---|---|---|---|---|
| prompt only | 95/105 | 95/105 | 91/105 | 57/68 |
| prompt + retries | 95/105 | 105/105 | 101/105 | 65/68 |
| schema + retries | 105/105 | 105/105 | 101/105 | 65/68 |

The gate fails if any of those rates falls more than 2 points (exactly 2 passes). **These are the reference model's numbers, not a real model's.** They say: without a schema 10 of 105 replies need repair or a retry, retries rescue all of them, the schema removes them, and 4 are wrong whatever you do. What the gate protects is the machinery around the model. Mutations that break the schema being sent, the message layout, the retry feedback and the retry limit each fail it.

For a real model: `offgrid eval -models M -baseline docs/eval/baseline-real.json -margin 0.05` (exit 4 on regression), `-write-baseline` to create one, `-record docs/eval/history.csv` to append a row per run. `scripts/eval-nightly.sh` does all three for cron; `.github/workflows/eval.yml` runs a nightly on GitHub (Ollama on the runner; it has not been run there, expect to adjust the model or timeout).

## Verified

- `go test -race -short ./...` clean; gofmt, vet, lint, wasm as in earlier days.
- Mutations, all caught: 17 on retry, schema fallback and provider parsing (two survivors, `ctx.Err()` and the default of 3 tries, were fixed with new tests); 13 on the gate arithmetic and history; 4 on the pipeline (schema not sent, message layout, retry feedback, retry limit) plus a prompt wording change caught by the pin.
- The eval ran end to end against a llama.cpp-style server that has no Ollama endpoints, and sent the schema on every call.

## Limits, honestly

- No real model was run through any of this. The reference model proves the pipeline, not quality; the nightly job and your machine do that.
- The reference only produces valid commands, so a validator that became too lax would not move the gate; the validator has its own tests.
- The nightly workflow, the Ollama install step and the push to `eval-history` are untested on GitHub.
- The llama.cpp support is the OpenAI protocol against my own test server, not against a real `llama-server`.

## Your part

1. `go test ./internal/eval ./internal/llm ./internal/intent` locally; then merge nothing in `main.go` (not touched this day).
2. On the laptop and the Pi, with a model installed: `scripts/eval-nightly.sh` once by hand, look at `docs/eval/latest.md`, and if the numbers are acceptable run `offgrid eval -models M -write-baseline docs/eval/baseline-real.json` and commit it.
3. If you have a llama.cpp server: `offgrid eval -host llamacpp@http://HOST:8080 -models any -limit 20` and tell me what it printed.
4. Put `scripts/eval-nightly.sh` in cron, or enable the workflow (`vars.EVAL_MODEL` sets the model; Settings, Actions, allow write permission for the nightly).
5. Earlier open items still stand.
