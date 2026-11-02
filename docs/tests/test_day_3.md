# Day 3 Master Assessment: Structured Outputs, Semantic Validation & Empirical Evaluation

> **Focus:** Constrained Decoding, Feedback-Driven Retries, Go Generics, Statistical Evaluation & Wilson Confidence Intervals  
> **Target Packages:** `internal/structured`, `internal/intent`, `internal/eval`, `cmd/offgrid` (`parse.go`, `evalcmd.go`)

---

## Overview

Day 3 of **Offgrid** solves the central reliability bottleneck of edge AI agents: **"Turning probabilistic model text into validated, typed Go values."**

Small language models (1B to 3B parameters) are inherently prone to formatting errors, missing fields, and subtle hallucinated keys. Rather than relying on naive prompt engineering, Day 3 implements an engineering architecture built on **Three Layers of Defense**, backed by a 30-case statistical evaluation harness with **Wilson score confidence intervals** and **ablation studies**.

This document contains:
1. **20 Conceptual Questions & In-Depth Answers** (Theory, constrained decoding physics, few-shot prompting, statistical rigor, and ablation methodology).
2. **10 Code-Focused Questions & In-Depth Answers** (Line-by-line Go generics, strict JSON parsers, embedded datasets, and Wilson score mathematics).

---

# Part 1: 20 Conceptual Questions & In-Depth Answers

---

### Q1: Why do Small Language Models (1B–3B) frequently fail at emitting valid JSON, and why is this not a bug to "wait out"?
**Answer:**  
A language model does not possess a compiler, an AST (Abstract Syntax Tree), or an intrinsic understanding of JSON syntax. It operates as an autoregressive next-token predictor: at every step, it samples the next token from a vocabulary probability distribution based on training data statistics.

JSON syntax is mathematically unforgiving:
- A single preamble sentence before the opening brace (`"Here is your JSON: {"`).
- A missing closing quote or trailing comma (`{"title": "Pump",}`).
- A single markdown backtick fence (````json ... ````).

Any of these minor token deviations causes a strict JSON parser to reject the entire message. Small models have fewer parameters and weaker attention heads dedicated to tracking rigid syntactical balance across long token sequences. This is not a temporary software bug; it is an inherent property of probabilistic token generation. Reliability must be **engineered into the runtime architecture around the model**.

---

### Q2: What are the "Three Layers of Defense" in the Offgrid architecture, and what does each layer catch that the previous one cannot?
**Answer:**  
Each layer addresses a distinct failure mode in order:

| Layer | Mechanism | What It Guarantees | What It CANNOT Catch |
|---|---|---|---|
| **Layer 1: Constrain** | Grammar masking (`ResponseSchema`) | Syntactically well-formed JSON matching the required schema shape. | Semantic errors: right shape, but empty title, illegal status, or wrong intent. |
| **Layer 2: Validate** | Deterministic code (`intent.Validate`) | The content makes logical sense for the intent (e.g., `create_task` has a title, priority is valid). | Bad luck: the model may simply output an invalid command again. |
| **Layer 3: Retry** | Feedback loop (`structured.Complete`) | A second chance where the model is shown its exact error message. | Infinite loops: it bounds attempts to 3, then fails cleanly. |

**Key Rule:** *A schema makes you feel safe; validation in code is what actually makes you safe.*

---

### Q3: How does Constrained Decoding (Grammar Masking) physically work at the token sampling layer in Ollama / llama.cpp?
**Answer:**  
During unconstrained generation, the neural network calculates logit scores for every token in its vocabulary (typically 32,000 to 128,000 tokens), and the sampler selects among the highest-scoring tokens.

Under **Constrained Decoding**:
1. The engine builds a Pushdown Automaton (PDA) or Context-Free Grammar (CFG) from the provided JSON Schema.
2. At every single token position, the engine evaluates which characters are syntactically legal according to the grammar state.
3. It sets the logit score of every illegal token to $-\infty$ (masking them out).
4. For example, immediately after generating `{"intent": "`, the engine physically prohibits tokens like `"apple"`, `"hello"`, or markdown backticks; only the tokens representing legal enum values (`"create_task"`, `"update_task"`, etc.) remain in the vocabulary.

The model physically cannot output invalid JSON because illegal tokens are removed from the menu before sampling occurs.

---

### Q4: What are the fundamental limitations of Constrained Decoding? Why is "valid schema" not synonymous with "correct command"?
**Answer:**  
Constrained decoding enforces **structural syntax (SHAPE)**, but knows nothing about **domain truth or semantics (MEANING)**:
1. **Wrong Intent:** The model can output `{"intent":"update_task", ...}` when the user asked for a daily summary. The schema is 100% satisfied, but the action is completely wrong.
2. **Empty Required Fields:** The model can satisfy a string schema by outputting `{"title": ""}`. The JSON is valid, but the task is useless.
3. **Truncated Generation:** If `MaxTokens` is set too low, the generation cuts off before the JSON closes, resulting in an unparseable fragment regardless of grammar constraints.
4. **Backend Incompatibility:** If you switch to a cloud API or an alternative local engine that lacks schema-masking support, grammar constraints are ignored.

Therefore, application code must still strictly validate every field.

---

### Q5: Why is the `Command` data structure designed as a flat, all-string struct rather than deeply nested JSON objects?
**Answer:**  
In `internal/intent/intent.go`, `Command` is deliberately structured as:
```go
type Command struct {
    Intent   Intent `json:"intent"`
    Title    string `json:"title"`
    TaskRef  string `json:"task_ref"`
    Site     string `json:"site"`
    Priority string `json:"priority"`
    Status   string `json:"status"`
    When     string `json:"when"`
    Note     string `json:"note"`
}
```
1. **Model Capability:** Small 1B/3B models struggle with deeply nested JSON hierarchies (e.g., `{"task": {"metadata": {"location": {"site": "4"}}}}`). Deep nesting increases attention span complexity and token consumption.
2. **Uniform Field Representation:** By representing all optional fields as plain strings, "not applicable" is universally represented as `""`. The model never needs to decide between `null`, `false`, `0`, or omitting the key.
3. **Predictable Generation Time:** A flat struct generates a roughly constant number of structural tokens, keeping latency predictable.

---

### Q6: Why is the `intent` key explicitly placed as the FIRST property in the JSON schema?
**Answer:**  
Autoregressive language models generate tokens strictly from left to right. When generating JSON, the model fills in the keys in the order they appear in the schema:

- **If `title` came first:** The model would have to write the title *before* deciding whether the command is a `create_task`, an `add_note`, or an `update_task`.
- **With `intent` first:** The model is forced to make the high-level semantic classification (`"create_task"`) before generating the title, site, and priority.

Deciding the intent first primes the model's self-attention context, dramatically improving the accuracy of all subsequent fields.

---

### Q7: What is the purpose of the `unknown` intent, and how does it prevent model hallucination?
**Answer:**  
If an intent classifier only supports valid domain intents (`create_task`, `update_task`, `list_tasks`, `add_note`, `daily_summary`), it has no way to handle out-of-scope inputs (e.g., *"What is the weather?"*, *"Tell me a joke"*, or *"Delete all tasks"*).

Under constrained decoding, the model is trapped: it *must* select one of the valid enum values. It will forcefully shoehorn unrelated queries into the nearest real intent (e.g., treating *"Delete everything"* as `update_task`).

Providing an explicit **`unknown`** intent gives the model a safe, legitimate exit. It allows the model to state honestly: *"I recognize this text, but it is not an action this application performs."*

---

### Q8: How does Few-Shot demonstration prompting work in `internal/intent/prompt.go`, and why are examples formatted as conversation turns?
**Answer:**  
Few-shot prompting provides concrete input-output examples directly in the context window. In `internal/intent/prompt.go`:
```go
msgs = append(msgs,
    llm.Message{Role: llm.RoleUser, Content: e.text},
    llm.Message{Role: llm.RoleAssistant, Content: string(reply)},
)
```
1. **Teaching by Demonstration:** Small models understand conversational role examples (`user` followed by `assistant`) much more reliably than abstract descriptive rules.
2. **Enforcing Structure:** By seeing 6 consecutive examples of clean, flat JSON replies with empty strings for unused fields, the model internalizes the exact syntax pattern.
3. **Natural Prompting:** Formatting examples as prior dialogue turns aligns with how instruction-tuned models were trained during RLHF/chat fine-tuning.

---

### Q9: Why does `internal/intent/prompt.go` place static text and few-shot examples first, and the worker's message last?
**Answer:**  
This applies the **Day 1 Prefill & KV Cache Optimization**:
1. Ollama reuses the precomputed Key-Value (KV) attention cache for prompt prefixes it has already seen.
2. The system prompt and the 6 few-shot examples contain approximately **450 tokens** that never change.
3. Because this static block is at the very beginning of the prompt, Ollama evaluates it once and caches it in memory.
4. When a worker submits a new 20-token message, the model only computes the prefill for the trailing 20 tokens, drastically reducing Time-To-First-Token (TTFT) on CPU hardware.

If the worker's dynamic message were placed at the beginning, the entire 470-token prompt would have to be recomputed from scratch on every call.

---

### Q10: What is the architectural difference between Schema Enforcement and Semantic Validation (`intent.Validate`)?
**Answer:**  
- **Schema Enforcement (Grammar):**
  - Operates on *syntax*.
  - Verifies that fields are valid JSON strings, keys exist, and enums match predefined lists.
  - Example: It ensures `"priority"` is one of `["", "low", "medium", "high"]`.
- **Semantic Validation (`intent.Validate`):**
  - Operates on *domain logic*.
  - Checks contextual inter-dependencies between fields:
    - If `intent == CreateTask`, `title` cannot be empty.
    - If `intent == UpdateTask`, `task_ref` is strictly required, and either `status` or `priority` must be specified.
    - If `intent == AddNote`, both `task_ref` and `note` must be present.
  - Normalizes text variations (e.g., converting `"Site 4"` to `"4"`).

---

### Q11: What is Normalization in command parsing, and what is its golden boundary rule?
**Answer:**  
`Command.Normalize()` tidies incoming values into canonical formats:
- Trims whitespace.
- Lowercases and converts dashes/spaces to underscores (`"In Progress"` $\rightarrow$ `"in_progress"`).
- Strips redundant site prefixes (`"Site 4"` or `"site-3"` $\rightarrow$ `"4"`).

**The Golden Boundary Rule:**  
**Normalization reshapes existing information; it never invents new information.**  
If a user writes `"inspect pump"`, normalization must never assume site `"1"` or priority `"high"`. Inventing data leads to silent corruption; reshaping data ensures consistency across downstream database queries.

---

### Q12: Why is retrying blindly at `Temperature: 0` mathematically pointless, and how does feedback-driven retry fix this?
**Answer:**  
At `Temperature: 0` (greedy decoding), the model's output is a completely **deterministic function of its input**:
$$\text{Output} = f(\text{Input})$$
If you send the exact same prompt a second time, the model will output the exact same rejected token sequence. Repeating the request wastes CPU cycles and battery.

**Feedback-Driven Retries:**  
To change the output, you must change the input. Offgrid appends two new conversational turns:
1. `RoleAssistant`: The model's own bad response (`resp.Text`).
2. `RoleUser`: A targeted error message:
   ```
   Your reply was rejected: create_task needs a non-empty title.
   Reply again with only the corrected JSON object.
   ```
The model's attention mechanism reads its prior mistake and the explicit correction prompt, guiding it to repair the specific invalid field.

---

### Q13: Why are retries bounded to a maximum of 3 attempts, and why is exponential backoff omitted?
**Answer:**  
1. **Bounded Attempts (Limit = 3):** Empirical testing shows that if a small model fails twice with explicit error feedback, the probability of it succeeding on attempt 4 is near zero. Additional attempts simply burn CPU resources and frustrate the user.
2. **No Exponential Backoff:** Exponential backoff (waiting 1s, 2s, 4s...) is designed to prevent overwhelming remote cloud servers during network congestion. On a local laptop running a single-user model, there is no network congestion. Waiting adds artificial delay; the model should be re-prompted immediately.

---

### Q14: Why must `structured.Complete` NEVER retry infrastructure errors (unreachable backend, timeouts, missing models)?
**Answer:**  
`structured.Complete` is responsible strictly for **content extraction errors** (invalid JSON, semantic validation failures).

If Ollama is down (`ErrUnavailable`) or times out (`DeadlineExceeded`):
- Retrying immediately would hammer an unresponsive process.
- Retrying would hide the infrastructure outage from the router.
- In an offline-first architecture, infrastructure outages belong to the **durable job queue (Day 10)**, which persists requests to SQLite and handles backoff. Mixing content retries with infrastructure retries creates architectural chaos.

---

### Q15: What is the "Empty Reply" edge case in conversational retries, and how does `nonEmpty()` prevent conversational collapse?
**Answer:**  
If a model suffers an internal glitch or hit an immediate EOS token, it may return an empty string (`""`).

In chat APIs, message arrays enforce that every message has non-empty text (`strings.TrimSpace(m.Content) != ""`). If the retry loop naively appends:
```go
llm.Message{Role: llm.RoleAssistant, Content: resp.Text}
```
The subsequent HTTP call will fail immediately with `ErrInvalidRequest: message is empty`!

`nonEmpty(resp.Text)` intercepts empty strings and substitutes `"(empty reply)"`. This preserves conversation formatting and allows the model to be told that an empty answer was rejected.

---

### Q16: What is the fundamental difference between a Unit Test and an LLM Evaluation (Eval)?
**Answer:**  
- **Unit Test (Deterministic):**
  - Asks: *"Is my software logic mathematically correct?"*
  - Expects binary outcomes: Pass (100%) or Fail (0%).
  - Tests code contracts, error wrapping, schema mapping, and parsers.
  - Runs in microseconds using fakes.
- **Evaluation (Probabilistic):**
  - Asks: *"How accurately does this language model interpret real human requests?"*
  - Produces statistical rates: e.g., 27 out of 30 cases correct (90%).
  - Evaluates models across intent classification and argument extraction.
  - Requires statistical confidence bounds and ablation baselines.

---

### Q17: In statistical evaluation with $N=30$ cases, why is reporting a raw percentage (e.g., "90% accurate") scientifically misleading?
**Answer:**  
With a sample size of only 30 test cases, each individual test case represents **3.33%** of the entire score. A score of 27/30 (90%) and a score of 25/30 (83.3%) differ by only two individual edge cases.

In probability theory, observing 27 successes out of 30 trials from an underlying distribution means the true underlying population accuracy could realistically lie anywhere between **74% and 97%**.

Claiming *"Model B (90%) beats Model A (83%)"* is statistically invalid because their confidence intervals overlap heavily. Reporting raw percentages creates an illusion of precision that masks statistical uncertainty.

---

### Q18: What is the Wilson Score Confidence Interval, and why is it preferred over the standard Wald textbook formula?
**Answer:**  
The standard textbook formula (Wald interval):
$$p \pm z \sqrt{\frac{p(1-p)}{n}}$$
breaks down completely when sample sizes $n$ are small or when success rates are near 0% or 100%:
- At 30/30 (100%), Wald calculates an interval of $[100\%, 100\%]$ (zero variance), which is absurd.
- At 29/30, the upper bound can calculate to $> 100\%$.

The **Wilson Score Interval** inverts the hypothesis test using a normal approximation to the binomial distribution. It introduces a continuity correction centered around the sample mean:
$$\text{center} = \frac{\hat{p} + \frac{z^2}{2n}}{1 + \frac{z^2}{n}}$$
It naturally handles small $n$, never exceeds $0\%$ or $100\%$, and provides mathematically sound error bars even when all cases pass.

---

### Q19: What is an Ablation Study in AI systems engineering, and what do the three Offgrid setups isolate?
**Answer:**  
An **Ablation Study** systematically removes or adds individual components of a system to measure the exact performance contribution of each piece.

`offgrid eval -ablation` runs three distinct configurations:
1. **Setup 1: Prompt Only (`-schema=false -attempts=1`):**  
   The model is simply *asked* in plain English to write JSON. Measures raw model capability.
2. **Setup 2: + Schema (`-schema=true -attempts=1`):**  
   Adds grammar-constrained decoding. Measures the value of schema enforcement on formatting.
3. **Setup 3: + Schema + Retries (`-schema=true -attempts=3`):**  
   Adds feedback-driven retries. Measures how many semantic mistakes can be rescued by runtime feedback.

This proves empirically what percentage of reliability comes from the model versus what percentage comes from runtime engineering.

---

### Q20: Why must few-shot prompt examples be strictly excluded from the evaluation dataset (`cases.json`)?
**Answer:**  
If a benchmark includes messages that were already shown in the system prompt examples (e.g., `"T-3 is finished"`):
- The model does not need to reason or generalize; it simply recalls the exact answer from its immediate context window.
- The evaluation measures **memorization/in-context recall**, not **generalization understanding**.
- In `internal/eval/cases_test.go`, a unit test explicitly cross-references `ExampleTexts()` against all `cases.json` inputs and fails the build if any overlap is detected.

---

# Part 2: 10 Code-Focused Questions & Line-by-Line Answers

---

### Q21: In `internal/structured/structured.go`, examine the signature of `Complete[T any]()`. How do Go Generics provide type safety without reflection?
```go
// From internal/structured/structured.go:
func Complete[T any](
    ctx context.Context,
    c llm.Client,
    req llm.Request,
    validate func(*T) error,
    opt Options,
) (Result[T], error)
```
**Answer:**  
1. `[T any]` defines a generic type parameter. `Complete` can extract a `Command` today, a `TaskPlan` tomorrow, or a `DatabaseQuery` next week.
2. `validate func(*T) error` accepts a pointer to `T`. This allows the validation function to inspect the decoded struct and mutate/tidy it in place (e.g., running `c.Normalize()`) without returning a new copy.
3. The returned `Result[T]` holds `Value T` strongly typed at compile time. Callers do not need type assertions `value.(Command)` or reflection (`reflect.ValueOf`), eliminating runtime panics.

---

### Q22: In `internal/structured/structured.go`, examine `parse[T]()`. Why does it call both `DisallowUnknownFields()` and `dec.Token()`?
```go
// From internal/structured/structured.go:
dec := json.NewDecoder(strings.NewReader(body))
dec.DisallowUnknownFields()
if err := dec.Decode(&v); err != nil {
    return v, repaired, fmt.Errorf("the reply is not the expected JSON object: %w", err)
}
if _, err := dec.Token(); !errors.Is(err, io.EOF) {
    return v, repaired, errors.New("the reply has extra text after the JSON object")
}
```
**Answer:**  
Standard `json.Unmarshal` is lenient: it ignores unrecognized JSON keys and ignores trailing text after the closing brace.
1. `dec.DisallowUnknownFields()`: If the model hallucinates keys (e.g., `{"intent": "create_task", "urgent": true}`), decoding fails immediately. This forces the model to adhere strictly to the declared struct fields.
2. `dec.Token()`: Standard decoding stops at the closing `}` and succeeds even if trailing conversational text follows (e.g., `{"intent":"list_tasks"} Let me know if you need anything else!`). Reading the next token and asserting `errors.Is(err, io.EOF)` guarantees that nothing follows the JSON object.

---

### Q23: In `internal/structured/structured.go`, how does `extractJSON()` isolate embedded JSON objects and set `Repaired`?
```go
// From internal/structured/structured.go:
func extractJSON(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end < start {
		return s, false
	}
	cut := s[start : end+1]
	return cut, cut != s
}
```
**Answer:**  
1. Models running without constrained decoding frequently wrap JSON in markdown fences (````json { ... } ````) or introductory chat phrases (`"Sure! Here is your command: { ... }"`).
2. `strings.Index(s, "{")` finds the very first opening brace; `strings.LastIndex(s, "}")` finds the final closing brace.
3. `cut := s[start : end+1]` slices out the inner JSON candidate.
4. `cut != s` returns `true` for `repaired` if any surrounding characters were sliced away. This flag is tracked in `Attempt.Repaired` so benchmark reports can measure how often a model rambled around its JSON.

---

### Q24: In `internal/structured/structured.go`, how does the retry loop construct feedback messages for the model?
```go
// From internal/structured/structured.go:
lastErr = perr
msgs = append(msgs,
    llm.Message{Role: llm.RoleAssistant, Content: nonEmpty(resp.Text)},
    llm.Message{Role: llm.RoleUser, Content: feedback(perr)},
)
```
```go
func feedback(err error) string {
    return "Your reply was rejected: " + err.Error() + "\nReply again with only the corrected JSON object."
}
```
**Answer:**  
1. The model's rejected output is preserved as a `RoleAssistant` message. This gives the model an explicit conversational reference to its prior output.
2. `feedback(perr)` formats the exact error string generated by `intent.Validate` as a new `RoleUser` message.
3. The prompt explicitly commands: *"Reply again with only the corrected JSON object."*
4. On the subsequent iteration, the model's self-attention mechanism attends to both its previous attempt and the error description, allowing it to regenerate only the corrected fields.

---

### Q25: In `internal/intent/intent.go`, why does `ValidationError` accumulate problems in a slice instead of returning on the first error?
```go
// From internal/intent/intent.go:
type ValidationError struct {
    Problems []string
}

func (e *ValidationError) Error() string { return strings.Join(e.Problems, "; ") }
```
```go
// Inside Validate():
var problems []string
add := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }
// ... checks intent ...
// ... checks priority enum ...
// ... checks status enum ...
// ... checks field lengths ...
if len(problems) > 0 {
    return &ValidationError{Problems: problems}
}
```
**Answer:**  
If validation failed fast on the first error:
- Attempt 1: Rejects empty title.
- Attempt 2: Model fixes title, but still has an illegal priority `"urgent"`. Rejects illegal priority.
- Attempt 3: Model fixes priority, but site name is 50 characters long. Out of retry attempts!

By accumulating all validation issues into `problems`, `Error()` produces a comprehensive compound message:
`"create_task needs a non-empty title; priority 'urgent' is not allowed; use one of: low, medium, high"`.
The model receives all corrections simultaneously on Attempt 1, maximizing the chance of a successful retry.

---

### Q26: In `internal/intent/intent.go`, examine `Command.Normalize()`. How does it sanitize site names without destroying alphanumeric site codes?
```go
// From internal/intent/intent.go:
if len(c.Site) > 4 && strings.EqualFold(c.Site[:4], "site") && strings.ContainsRune(" #:-_", rune(c.Site[4])) {
    c.Site = strings.TrimSpace(strings.TrimLeft(c.Site[4:], " #:-_"))
}
```
**Answer:**  
Field workers write site names in multiple colloquial ways: `"Site 4"`, `"site-4"`, `"Site #4"`, or `"site: 4"`.
1. It verifies the string begins with `"site"` case-insensitively (`strings.EqualFold(c.Site[:4], "site")`).
2. Crucially, it checks `strings.ContainsRune(" #:-_", rune(c.Site[4]))` to ensure the fifth character is a delimiter.
3. This strips `"Site 4"` down to `"4"`, but preserves legitimate alphanumeric equipment codes like `"Siteworks-2"` or `"Sitemap"`, preventing destructive string truncation.

---

### Q27: In `internal/intent/prompt.go`, why is the JSON schema written as a raw constant rather than generated via Go reflection?
```go
// From internal/intent/prompt.go:
const schema = `{
  "type": "object",
  "properties": {
    "intent":   {"type": "string", "enum": ["create_task", "update_task", ...]},
    "title":    {"type": "string"},
    "task_ref": {"type": "string"},
    "site":     {"type": "string"},
    "priority": {"type": "string", "enum": ["", "low", "medium", "high"]},
    "status":   {"type": "string", "enum": ["", "open", "in_progress", "done"]},
    "when":     {"type": "string", "enum": ["", "today", "yesterday", "this_week"]},
    "note":     {"type": "string"}
  },
  "required": ["intent", "title", "task_ref", "site", "priority", "status", "when", "note"],
  "additionalProperties": false
}`
```
**Answer:**  
In Go, converting a `map[string]any` to JSON scrambles key order randomly because Go maps are intentionally randomized.
- For constrained decoding, key order is critical: `"intent"` must be generated first so the model classifies the action before generating parameters.
- Standard struct-to-schema reflection packages do not guarantee strict deterministic key order across compiler versions.
- Writing the schema as an explicit string constant guarantees that `"intent"` is always the first token generated.  
A dedicated reflection test (`TestSchemaMatchesStructAndValidator`) guarantees that this raw schema, the `Command` struct, and the validator never drift apart.

---

### Q28: In `internal/eval/eval.go`, how does `//go:embed` package the test dataset into the compiled binary?
```go
// From internal/eval/eval.go:
import _ "embed"

//go:embed cases.json
var casesJSON []byte
```
**Answer:**  
1. `//go:embed cases.json` is a Go compiler directive. During `go build`, the compiler reads `cases.json` from disk and bakes its raw bytes directly into the compiled executable binary's data section.
2. When the user executes `./bin/offgrid eval`, the binary has zero external file dependencies. It does not need to locate files relative to working directories, preventing `"file not found"` errors when executed from scripts or system paths.

---

### Q29: In `internal/eval/eval.go`, examine `isFatal()`. How does it differentiate fatal run-stopping errors from individual case failures?
```go
// From internal/eval/eval.go:
func isFatal(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	var notFound *llm.ModelNotFoundError
	return ctx.Err() != nil ||
		errors.Is(err, llm.ErrUnavailable) ||
		errors.As(err, &notFound) ||
		errors.Is(err, llm.ErrInvalidRequest)
}
```
**Answer:**  
During a 30-case evaluation run:
1. **Non-Fatal Error:** If a single case times out or the model produces an unparseable response (`ErrNoValidReply`), `isFatal()` returns `false`. The error is recorded inside that `CaseResult`, and the evaluation proceeds to case 2.
2. **Fatal Error:** If the user presses `Ctrl+C` (`ctx.Err() != nil`), Ollama crashes (`ErrUnavailable`), or the model tag is missing (`*ModelNotFoundError`), running further cases is completely meaningless. `isFatal()` returns `true`, immediately halting the evaluation loop and saving whatever results completed.

---

### Q30: In `internal/eval/stats.go`, walk through the mathematical computation of the Wilson Score Interval.
```go
// From internal/eval/stats.go:
func (p Proportion) Wilson() (lo, hi float64) {
	if p.N == 0 {
		return 0, 0
	}
	const z = 1.96 // 95% confidence
	n := float64(p.N)
	phat := p.Rate()
	denom := 1 + z*z/n
	centre := (phat + z*z/(2*n)) / denom
	half := z * math.Sqrt(phat*(1-phat)/n+z*z/(4*n*n)) / denom
	return math.Max(0, centre-half), math.Min(1, centre+half)
}
```
**Answer:**  
1. `z = 1.96`: Corresponds to the 97.5th percentile of the standard normal distribution, establishing a **95% two-sided confidence interval**.
2. `phat = p.Rate()`: The observed sample success rate ($K / N$).
3. `denom = 1 + z^2 / n`: Normalization factor that scales the interval inversely with sample size $n$.
4. `centre`: Shifts the center of the interval away from extreme values (0 or 1) toward 0.5 when sample sizes are small (the Agresti-Coull effect).
5. `half`: Calculates the standard error margin incorporating both sample variance $\hat{p}(1-\hat{p})/n$ and small-sample correction $z^2/(4n^2)$.
6. `math.Max(0, ...)` and `math.Min(1, ...)`: Clamps the interval strictly within $[0.0, 1.0]$, preventing statistically impossible rates $< 0\%$ or $> 100\%$.
