# ADR 016: One provider setting, a pinned prompt, and a gate on the valid-command rate

**Status:** accepted, 2026-11-24 (Day 50).

## Context

The model layer already had a `Client` interface with Ollama and OpenAI-compatible backends, a 30-case eval and a retry-with-feedback loop. Three things were missing: a way to point a light node at a full node's model (llama.cpp's server, Ollama on the laptop) without a code change; any record of which prompt a number was measured with; and anything that stops a change from lowering the valid-command rate without anyone noticing. Thirty cases cannot tell a 2-point change from luck.

## Decisions

1. **One setting names kind and place.** `-host` (and `$OFFGRID_PROVIDER` for all commands) takes `http://addr` (Ollama, as before), `ollama@URL`, `llamacpp@URL` or `openai@URL`. `llamacpp` and `openai` speak `/v1/chat/completions`. The same flag therefore works for `ask`, `parse`, `agent`, `drain` and `eval`.
2. **Remote means retry.** A provider not on this machine is wrapped in `WithRetry`: at most 3 tries, wait a random time up to min(2 s, 250 ms x 2^n) ("full jitter"), only for `ErrUnavailable`, never after a streamed reply has started, and the wait ends with the context. A local provider is not retried: a local server that is down will not be back in two seconds and the router's answer is to queue.
3. **Schema where available.** The OpenAI-compatible client sends the request schema as `response_format` (strict). If a server answers HTTP 400 to it, the client repeats the call once without it and remembers, so an unsupporting server costs one extra call in total. A bad request without a schema stays an error.
4. **A key is never sent in clear to the internet.** An API key with a plain `http://` address is refused unless the host is this machine, a private address, a link-local address, `.lan`, `.local`, `.home.arpa` or a single-label name.
5. **The prompt has a version and a fingerprint.** `intent.PromptVersion` and `intent.PromptFingerprint()` (a hash of the instructions, examples and schema). A test pins the fingerprint; changing the prompt fails the build until the version is bumped and the pin updated on purpose. Every eval report, baseline and history row carries both.
6. **105 labelled cases**, at least 8 per intent, unique texts. Their fingerprint is recorded too: rates on different case sets are never compared (`ErrStaleBaseline`).
7. **A gate with a margin.** `eval.Baseline.Compare` fails when the valid-command rate on the first try, the valid-command rate after retries, the intent rate or the argument rate is lower than the baseline by more than the margin (exactly the margin is allowed). Improvements are notes, never failures. A baseline setup that the run did not measure is an error. CI uses a margin of 2 points (2 cases); `offgrid eval -baseline F -margin M` defaults to 5 points for real models, which are noisy, and exits 4.
8. **Two different guards, said plainly.**
   - *CI gate (every push):* the cases run through the real prompt layout, schema, JSON repair, validator and retry loop against a deterministic **reference model** that answers from the labels and has scheduled faults (chatty wrapping and broken JSON without a schema, a wrong intent every 25th case). Three setups are gated (prompt only, prompt with retries, schema with retries). It guards the pipeline. **It cannot measure model quality** and says so.
   - *Nightly run:* a real model, recorded to `docs/eval/history.csv`, gated against `docs/eval/baseline-real.json` once you commit one.

## Rejected

- A mock-only eval presented as "the model scores 96%". It would measure nothing about a model.
- A margin of zero on a real model: a single sampling difference would fail the night.
- Retrying inside a local provider: it delays the queue decision for no gain.

## What it costs

The reference model encodes the message layout (system, examples, message). Changing the layout means changing the reference with it; the failing gate says so. The gate cannot catch a validator that is too lax, because the reference only returns valid commands; validator rules are tested directly.

## What would change it

A real-model baseline showing noise above the 5-point margin night to night (widen the margin, or run each case more than once); a provider whose protocol is neither of the two.
