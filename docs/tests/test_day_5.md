# Day 5 Master Assessment: Don't Call an LLM When a Regex Will Do

> **Focus:** Tiered Agent Orchestration, Rules Engine (Tier 0) vs. Local SLM (Tier 1), Latency Asymmetry, Closed-Vocabulary Parsing, Fuzzing & Offline Resilience  
> **Target Packages:** `internal/agent`, `cmd/offgrid` (`agentcmd.go`)

---

## Overview

Day 5 of **Offgrid** introduces the **Tiered Agent Architecture**:  
**"Don't call an LLM when a regex will do."**

On CPU-constrained edge hardware, generating tokens with a local 3B model consumes **seconds** and drains battery. A compiled regular expression or deterministic token parser executes in **microseconds** (over 50,000x faster) and is mathematically immune to hallucinations.

Day 5 constructs a two-tier routing engine:
1. **Tier 0 (Deterministic Rules Engine):** Handles predictable, high-frequency natural language commands in microseconds with zero model overhead.
2. **Tier 1 (Probabilistic Model Fallback):** Engages the local Small Language Model (SLM) *only* when the rules encounter ambiguous, complex, or colloquial phrasing.
3. **Unified Execution Gate:** Both tiers route into the Day 4 `tools.Registry`, ensuring the identical security and invariant guarantees apply regardless of who parsed the message.

---

# Part 1: 20 Conceptual Questions & In-Depth Answers

---

### Q1: What is a Tiered Agent Architecture, and why is it essential for edge computing?
**Answer:**  
In naive agent systems, *every* user input—even trivial queries like *"show open tasks"* or *"mark T1 done"*—is routed to an LLM. On battery-powered laptops or field devices running on CPU:
- An LLM call takes **2 to 8 seconds** of compute time.
- CPU cores hit 100% utilization, generating heat and draining battery.
- The model might hallucinate or format JSON incorrectly.

**Tiered Architecture:**  
Organizes processing into progressive tiers:
- **Tier 0 (Rules):** Instant, free, deterministic pattern matching.
- **Tier 1 (Local Model):** SLM fallback for complex, unstructured messages.
- **Tier 2 (Cloud / Async Queue):** Heavy frontier models when internet is available.

Simple requests are intercepted at Tier 0 in microseconds, saving the CPU for queries that truly require semantic reasoning.

---

### Q2: What is the empirical latency asymmetry between Tier 0 and Tier 1?
**Answer:**  
- **Tier 0 (Rules Engine):** Measured at **~40 microseconds (0.000040 s)** to **150 microseconds**.
- **Tier 1 (Ollama 3B SLM on CPU):** Measured at **2.5 to 6.5 seconds (2,500,000 to 6,500,000 µs)**.

The performance gap is **50,000x to 100,000x**. Routing even 50% of incoming field messages to Tier 0 reduces average user wait times from multi-second pauses to imperceptible, instantaneous responses while extending battery life exponentially.

---

### Q3: Why is the cost of being wrong asymmetric between "missing" a message vs. "misreading" a message?
**Answer:**  
This is the central design trade-off of the Offgrid rules engine:

| Failure Type | What Happens | System Impact |
|---|---|---|
| **A rule MISSES** (False Negative) | The rule stays silent. The message drops to Tier 1 (the model). | Cost: **A few seconds of latency**. The model reads it correctly; work proceeds normally. |
| **A rule MISREADS** (False Positive) | The rule fires on the wrong intent or extracts the wrong entity. | Cost: **Catastrophic data corruption**. A task is closed or created incorrectly with **zero warning**. |

Because missing a message carries an insignificant cost (a short latency penalty) while misreading carries a severe cost (silent corruption), the rules engine is engineered strictly for **Precision Over Coverage**.

---

### Q4: What does the "Precision Over Coverage" design philosophy dictate in rules engineering?
**Answer:**  
A rule does **not** need to understand every possible way a human can speak. It only needs to possess 100% certainty **when it chooses to speak**.
- If a rule is 99% sure, it must remain silent and yield to Tier 1.
- Silence is always safe because the language model is waiting immediately behind the rules engine.
- A rule that handles only 15 out of 30 standard phrases with 100% accuracy is vastly superior to an aggressive rule that handles 28 out of 30 but corrupts 2.

---

### Q5: What are "Closed-Vocabulary Rules", and how do they prevent keyword collision disasters?
**Answer:**  
A naive keyword matcher scans text for trigger words:
- Example: Seeing *"show tasks not done"*, a keyword rule detects `"show"`, `"tasks"`, and `"done"`.
- It executes: `list_tasks(status: "done")` &rarr; listing finished tasks, the **exact opposite** of what the operator requested!

**Closed-Vocabulary Rules:**  
In `matchList()` and `matchSummary()`:
1. The rule maintains an exhaustive dictionary of allowed words (starters, nouns, statuses, filler words, prepositions).
2. It tokenizes the input and asserts: **Every single word in the message must be present in the known dictionary.**
3. When it encounters an unknown word like `"not"`, `"except"`, or `"urgent"`, the rule immediately aborts and returns `false`.
4. The message falls through to Tier 1, where the language model's full attention mechanism interprets the negation correctly.

---

### Q6: What are "Frame Rules", and how do they handle mutations (`create_task`, `add_note`, `update_task`)?
**Answer:**  
Unlike list queries, mutation commands carry dynamic user payloads (e.g., custom task titles or free-form notes) that cannot be constrained to a closed vocabulary.

**Frame Rules** enforce rigid sentence structures:
- `create-task`: `^(?:new|create|add|log)\s+task:\s+(.+)$`
- `add-note`: `^(?:add\s+note|note)\s+on\s+([^:]+):\s+(.+)$`
- `update-status`: `^(?:mark|set)\s+(.+?)\s+as\s+(done|open)$`

If a message deviates from the expected structural frame (e.g., *"the pump at site 4 is leaking because the seal broke"*), the frame regex fails to match. The rule remains silent, allowing the model to parse the conversational phrasing.

---

### Q7: Why must a valid site code strictly contain at least one digit (e.g., `site 4`, `A4`), and what bug does this prevent?
**Answer:**  
In English, the word *"site"* is commonly used as a noun or modifier:
- *"New task: check the site visit"*
- *"New task: prep site for tomorrow"*

If the site regex allowed letters-only codes (e.g., `site ([a-zA-Z]+)`):
- It would parse `"visit"` as the site code!
- The task title would be truncated to `"check the"`.
- Real industrial site codes almost universally include numeric identifiers (`site 4`, `B-12`, `depot-2`).

`validSiteCode()` requires `strings.ContainsAny(s, "0123456789")`. Phrases containing letters-only words like *"site visit"* fail the check and fall through to the model, which understands the grammatical context.

---

### Q8: Why does `cleanText()` strip polite prefixes and trailing punctuation while preserving the original character case?
**Answer:**  
1. **Polite Prefixes:** Field workers frequently write conversational preambles: *"please create task: ..."*, *"can you mark T1 done"*, or *"i'd like to list open tasks"*. Stripping these prefixes (`"please "`, `"could you "`) exposes the canonical sentence frame underneath.
2. **Preserving Case:** While intent matching must be case-insensitive, task titles and notes must preserve the operator's exact capitalization:
   - Proper nouns: *"Inspect Siemens Turbine B"*.
   - Chemical names: *"Check NaCl tank"*.
   Lowercasing the stored string would destroy domain formatting in persistent logs.

---

### Q9: Walk through the complete execution sequence of `Agent.Handle()`.
**Answer:**  
1. **Clock Initialization:** Samples the injected clock (`start := now()`).
2. **Empty Check:** If message is blank, returns `StatusNotUnderstood` immediately without calling the model.
3. **Tier 0 Evaluation:** Runs `Rules.Match(text)`.
   - If matched: invokes `Registry.Execute()`.
   - If registry returns `*ArgumentError` (rule bug): records `Fallback` explanation and proceeds to Tier 1.
   - If registry succeeds or returns a domain refusal (`ErrNotFound`, `ErrAmbiguous`): records timings and returns immediately.
4. **Tier 1 Evaluation:** If Tier 0 matched nothing (or fell back):
   - Checks if `a.Model == nil` &rarr; returns `StatusFailed` with `ErrNoModel`.
   - Invokes `Model.Classify()`.
   - Passes the resulting `Command` to `Registry.Execute()`.
5. **Telemetry & Timing:** Calculates `Understand`, `Execute`, and `Total` durations and packages the result into an `Outcome`.

---

### Q10: If a Tier 0 rule matches but the registry returns `ErrNotFound` or `*AmbiguousError`, why does the loop NOT fall back to the model?
**Answer:**  
Suppose a worker types: *"close the boiler ticket"*, but there are no boiler tasks in the database (or there are two boiler tasks, `T1` and `T2`):
- The rule understood the worker's intent with 100% accuracy: intent is `update_task`, target is `"the boiler ticket"`, status is `"done"`.
- The failure happened because of the **state of the database**, not because of a language comprehension failure.
- A language model has no internal telepathy to know which tasks exist in SQLite. Asking the model would waste 5 seconds of CPU time only for the model to emit the identical command, which would fail again at the registry.
- The loop respects the rule's answer and halts immediately with `StatusRefused`.

---

### Q11: What happens if the registry rejects a Tier 0 command with an `ArgumentError`? Why fall back to the model?
**Answer:**  
An `*ArgumentError` means the command itself was structurally invalid (e.g., an empty title, an illegal priority string, or a disallowed control rune).
- Because Tier 0 rules are written by developers, an `ArgumentError` indicates a **bug in the rule's extraction regex** (e.g., the regex sliced an empty string for the title).
- The worker did nothing wrong.
- The agent loop logs the rule defect in `out.Fallback` (for engineering observability) and grants Tier 1 (the model) a chance to parse the worker's sentence correctly.

---

### Q12: How does the tiered agent demonstrate "Graceful Degradation" when Ollama is offline or uninstalled?
**Answer:**  
In conventional architectures, an offline LLM service crashes the entire application.  
In Offgrid:
1. If Ollama is down (`llm.ErrUnavailable`) or disabled via `-no-model`:
2. **Every Tier 0 command continues to function at full speed.** Workers can still create tasks (`"new task: check pump"`), update status (`"mark T1 done"`), list open tasks, and request summaries.
3. Only messages requiring complex semantic reasoning fail.
4. Instead of a fatal crash or cryptic HTTP error, the system returns an informative, human-readable message:  
   *"I could not read that with rules alone, and the language model is not reachable. Try a plainer phrase, such as 'list open tasks'."*

---

### Q13: Explain the Five Distinct Outcomes in `agent.Status`.
**Answer:**  
Every outcome is categorized into one of five mutually exclusive statuses:

| Status | Meaning | Example |
|---|---|---|
| **`ok`** | Understood by a tier and successfully executed by the registry. | Created task `T1`. |
| **`refused`** | Understood, but business invariants rejected execution. | Task `T99` not found, or reference is ambiguous. |
| **`declined`** | Understood as an out-of-scope query (`unknown` intent). | *"What is the weather?"* |
| **`not_understood`** | The model could not produce a valid command within retry limits. | Gibberish or unparseable input. |
| **`failed`** | Infrastructure failure unrelated to command semantics. | Ollama daemon down, model missing, or context timeout. |

---

### Q14: Why does the evaluation harness report the MEDIAN and SLOWEST latency rather than the arithmetic average?
**Answer:**  
1. **Average is Distorted by Outliers:** In a batch of 30 requests, 16 requests handled by rules take $40\ \mu\text{s}$, while 14 requests handled by the model take $3.5\ \text{s}$. If one single model request stalls for 30 seconds due to CPU thermal throttling, the arithmetic average spikes dramatically, misrepresenting the typical user experience.
2. **Median Represents the 50th Percentile:** The median reflects the true middle-of-the-road experience: half of all user requests were faster, and half were slower.
3. **Slowest (Max) Captures the Worst Case:** Highlighting the slowest request exposes latency spikes and worst-case queuing delays.

---

### Q15: Why does `FormatDuration()` dynamically adapt time units between microseconds (`µs`), milliseconds (`ms`), and seconds (`s`)?
**Answer:**  
In `internal/agent/stats.go`:
```go
func FormatDuration(d time.Duration) string {
    switch {
    case d < time.Millisecond:
        return fmt.Sprintf("%d µs", d.Microseconds())
    case d < time.Second:
        return fmt.Sprintf("%.1f ms", float64(d)/float64(time.Millisecond))
    }
    return fmt.Sprintf("%.2f s", d.Seconds())
}
```
If latency were hardcoded to seconds, a Tier 0 rule taking $42\ \mu\text{s}$ would render as `0.00 s`, hiding the measurement completely. If hardcoded to microseconds, a 4-second model call would render as `4218903 µs`, hindering quick human readability. Dynamic scaling makes both extreme regimes instantly readable.

---

### Q16: What is a "Never-Fails API" design, and why does `Agent.Handle()` return `Outcome` instead of `(Outcome, error)`?
**Answer:**  
In conventional Go design, functions return `(Result, error)`.  
However, when an agent processes natural language, "failure" is normal operational data:
- The user asking an unknown question is not a system failure.
- A task not existing in SQLite is not a runtime bug.
- A model failing validation is a domain outcome.

If `Handle` returned `(Outcome, error)`, callers might discard the `Outcome` whenever `err != nil`, losing crucial diagnostic fields (e.g., `Understand` duration, `Tier`, and `Fallback` reasons).  
Returning an `Outcome` struct containing `Status` and `Err` guarantees that callers always receive full telemetry without risk of dropped diagnostics.

---

### Q17: What is the Consumer-Side Interface pattern, and how does `agent.Model` demonstrate it?
**Answer:**  
In standard Go architectural design, **interfaces belong to the consumer, not the producer**:
- The `agent` package does not import `intent.Classifier` or the concrete `ollama.Client`.
- Instead, `agent` defines the exact minimal interface it requires:
  ```go
  type Model interface {
      Classify(ctx context.Context, text string) (structured.Result[intent.Command], error)
  }
  ```
Because `intent.Classifier` implements `Classify`, Go's structural typing satisfies the interface automatically. This allows unit tests in `internal/agent` to substitute lightweight fakes without compiling or running real neural network code.

---

### Q18: What is Table-Driven Negative Testing, and why is testing "silence" essential for safety?
**Answer:**  
In `internal/agent/rules_test.go`, a table test specifies **47 near-miss phrases** that must produce **zero matches** (`ok == false`):
- *"show tasks not done"* (contains negative modifier)
- *"mark task as urgent"* (contains invalid priority)
- *"what did we do last week"* (out of scope time word)
- *"what is open"* (question, not a status update)

**Why Testing Negatives Matters:**  
Positive tests prove that a system *can* work. Negative tests prove that a system *cannot be tricked into corrupting data*. Proving that a rule remains silent when presented with ambiguous input is the ultimate verification of the "Precision Over Coverage" contract.

---

### Q19: What is Property-Based Fuzz Testing (`FuzzRules`), and what invariants did it verify across 87,000 inputs?
**Answer:**  
Go's native fuzzing engine (`go test -fuzz FuzzRules`) feeds thousands of semi-random, mutated byte strings into `DefaultRules().Match()`:
1. **Crash Invariant (No Panics):** Even when fed malformed UTF-8, giant byte arrays, binary control characters, or nested symbols, the regex rules engine never panics or encounters index-out-of-range bounds errors.
2. **Structural Invariant:** Whenever `ok == true`, the returned `Command.Intent` is guaranteed to be a valid, recognized enum value.
3. Over 87,000 random inputs were evaluated in 20 seconds, proving total runtime stability under adversarial noise.

---

### Q20: In the 30-case evaluation set, what is the difference between the "blind" baseline (14/30) and the "tuned" baseline (16/30)?
**Answer:**  
- **The Blind Baseline (14/30):** The first version of the rules was written using general intuition *before* inspecting the 30 test cases. It successfully matched 14 cases, getting 13 completely correct. This represents the true, unbiased expectation of how human-crafted rules perform on unseen natural language.
- **The Tuned Baseline (16/30):** After reviewing the failures, 4 specific adjustments were made (e.g., recognizing *"end of day"* as `"today"`, and supporting `"X is now done"`). Accuracy increased to 16/16.
- **The Statistical Takeaway:** Reporting 16/30 as general accuracy would be intellectually dishonest because the rules were modified specifically to satisfy those known cases. Highlighting both numbers demonstrates rigorous evaluation integrity.

---

# Part 2: 10 Code-Focused Questions & Line-by-Line Answers

---

### Q21: In `internal/agent/agent.go`, examine how `Agent.Handle()` short-circuits empty messages.
```go
// From internal/agent/agent.go:
// An empty message is not worth a model call, which costs seconds.
if strings.TrimSpace(text) == "" {
    out.Status, out.Err = StatusNotUnderstood, errors.New("empty message")
    return finish()
}
```
**Answer:**  
1. `strings.TrimSpace(text)` strips whitespace, tabs, and newlines.
2. If the resulting string is empty `""`, calling Tier 1 would consume 3–5 seconds of CPU inference only for the model to report that no content was provided.
3. The check short-circuits execution immediately, setting `out.Status = StatusNotUnderstood` and calculating `out.Total` in under 1 microsecond.

---

### Q22: In `internal/agent/agent.go`, examine Tier 0 error handling. How does it distinguish rule bugs from domain errors?
```go
// From internal/agent/agent.go:
res, err := a.Registry.Execute(ctx, cmd)
out.Execute = now().Sub(execStart)

var argErr *tools.ArgumentError
if errors.As(err, &argErr) {
    out.Fallback = fmt.Sprintf("rule %q proposed a command the registry refused (%v)", rule, err)
} else {
    out.Result = res
    out.Status, out.Err = classify(err)
    return finish()
}
```
**Answer:**  
1. If `err` matches `*tools.ArgumentError` via `errors.As`:
   - The rule emitted a malformed command (e.g., empty title or invalid priority).
   - This represents an internal regex bug. The code notes the problem in `out.Fallback` and allows execution to fall through to Tier 1 (the model).
2. Otherwise (whether `err == nil` or `err` is a domain error like `task.ErrNotFound` or `task.ErrAmbiguous`):
   - The command was well-formed; the database simply could not resolve it.
   - The loop halts immediately and returns `finish()`, preventing unnecessary model calls.

---

### Q23: In `internal/agent/agent.go`, examine `classify()`. How does it map errors into `Status` constants?
```go
// From internal/agent/agent.go:
func classify(err error) (Status, error) {
	var argErr *tools.ArgumentError
	switch {
	case err == nil:
		return StatusOK, nil
	case errors.Is(err, tools.ErrNoTool):
		return StatusDeclined, err
	case errors.As(err, &argErr), errors.Is(err, task.ErrNotFound), errors.Is(err, task.ErrAmbiguous):
		return StatusRefused, err
	default:
		return StatusFailed, err
	}
}
```
**Answer:**  
1. `err == nil`: Operation succeeded &rarr; `StatusOK`.
2. `errors.Is(err, tools.ErrNoTool)`: Valid command, but out-of-scope (`unknown` intent) &rarr; `StatusDeclined`.
3. `errors.As(err, &argErr)` or `task.ErrNotFound` or `task.ErrAmbiguous`: The request was understood, but business logic refused execution &rarr; `StatusRefused`.
4. `default`: Unhandled errors (e.g., database disk error, cancelled context) &rarr; `StatusFailed`.

---

### Q24: In `internal/agent/rules.go`, examine `cleanText()`. How does it strip polite prefixes and punctuation iteratively?
```go
// From internal/agent/rules.go:
func cleanText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	for changed := true; changed; {
		changed = false
		s = strings.TrimRight(s, " .!?")
		low := strings.ToLower(s)
		for _, p := range politePrefixes {
			if strings.HasPrefix(low, p) {
				s = s[len(p):]
				changed = true
				break
			}
		}
		if strings.HasSuffix(strings.ToLower(s), " please") {
			s = s[:len(s)-len(" please")]
			changed = true
		}
	}
	return s
}
```
**Answer:**  
1. `strings.Join(strings.Fields(s), " ")` normalizes whitespace.
2. The `for changed` loop executes repeatedly until no further modifications occur. This correctly handles chained polite prefixes (e.g., *"Hey please could you list tasks"* &rarr; *"please could you list tasks"* &rarr; *"could you list tasks"* &rarr; *"list tasks"*).
3. `strings.TrimRight(s, " .!?")` drops trailing punctuation.
4. Prefix checks use `low := strings.ToLower(s)` for case-insensitive matching, but slice `s = s[len(p):]` from the original string, preserving the worker's original casing for task titles.

---

### Q25: In `internal/agent/rules.go`, examine `matchList()`. How does the closed-vocabulary token loop operate?
```go
// From internal/agent/rules.go:
for i := 1; i < len(toks); i++ {
    t := toks[i]
    switch {
    case listNouns[t]:
        anchored = true
    case statusWords[t] != "":
        anchored = true
        if !setOnce(&cmd.Status, statusWords[t]) { return intent.Command{}, false }
    case t == "low" || t == "medium" || t == "high":
        if !setOnce(&cmd.Priority, t) { return intent.Command{}, false }
    case isWhen(t):
        if !setOnce(&cmd.When, whenWord(t)) { return intent.Command{}, false }
    case t == "site" || t == "sites":
        // ... extracts site code ...
    case listAnchors[t]:
        anchored = true
    case listFiller[t]:
    default:
        return intent.Command{}, false // unknown word, for example "not" or "except"
    }
}
```
**Answer:**  
1. Iterates through tokens starting from index 1 (after the starter word like `"list"` or `"show"`).
2. Categorizes each word into allowed sets (`listNouns`, `statusWords`, priorities, time words, `listFiller`).
3. `setOnce()` fills target fields but returns `false` if the user specifies conflicting values (e.g., *"show open and done tasks"*).
4. `default: return intent.Command{}, false`: If *any* token is unfamiliar (e.g., `"not"`, `"without"`, `"except"`), the rule aborts immediately and yields to the language model.

---

### Q26: In `internal/agent/rules.go`, examine `matchCreate()`. How does it peel off priority and site suffixes?
```go
// From internal/agent/rules.go:
// Peel "site N" and "high priority" off the end, in whichever order they come.
for i := 0; i < 3; i++ {
    if p := prioritySuffixRe.FindStringSubmatchIndex(rest); p != nil && cmd.Priority == "" {
        cmd.Priority = strings.ToLower(firstNonEmpty(submatch(rest, p, 1), submatch(rest, p, 2)))
        rest = strings.TrimSpace(rest[:p[0]])
        continue
    }
    if p := siteSuffixRe.FindStringSubmatchIndex(rest); p != nil && cmd.Site == "" && validSiteCode(submatch(rest, p, 1)) {
        cmd.Site = submatch(rest, p, 1)
        rest = strings.TrimSpace(rest[:p[0]])
        continue
    }
    break
}
```
**Answer:**  
1. Workers can append attributes in any order:  
   - *"new task: fix valve at site 4 with high priority"*  
   - *"new task: fix valve high priority site 4"*
2. The loop runs up to 3 times, matching regex patterns anchored at the end of the string.
3. When a suffix matches, it extracts the value, slices off the matched substring (`rest = rest[:p[0]]`), and repeats.
4. The remaining string becomes the clean task title: `"fix valve"`.

---

### Q27: In `internal/agent/rules.go`, examine `cleanRef()`. How does `refDeny` prevent false reference matches?
```go
// From internal/agent/rules.go:
var refDeny = set("new", "create", "add", "log", "note", "notes", "and", "then", "because", "but",
	"also", "at", "site", "for", "to", "that", "which", "when", "if", "priority", "of", "status",
	"what", "whats", "which", "who", "how", "where", "why", "it", "this", "that", "these", "those", "everything", "all", "there")

func cleanRef(ref string) (string, bool) {
    // ...
    words := strings.Fields(ref)
    if len(words) == 0 || len(words) > 5 {
        return "", false
    }
    for _, w := range words {
        if refDeny[strings.ToLower(w)] {
            return "", false
        }
    }
    return ref, true
}
```
**Answer:**  
1. Strips common prefixes (`"the "`, `"task "`) and suffixes (`" task"`).
2. Enforces a maximum length of 5 words; legitimate task references are short names or IDs.
3. Checks every token against `refDeny`:
   - Pronouns (`"it"`, `"this"`): cannot be resolved without conversational memory.
   - Question words (`"what"`, `"why"`): indicate questions, not task names.
   - Conjunctions (`"because"`, `"and"`): indicate compound sentences.
4. Any match in `refDeny` rejects the reference, sending the message to Tier 1.

---

### Q28: In `internal/agent/rules.go`, examine `validSiteCode()`. How does it validate site identifiers?
```go
// From internal/agent/rules.go:
var siteCharsRe = regexp.MustCompile(`(?i)^[a-z0-9_-]+$`)

func validSiteCode(s string) bool {
	return len(s) >= 1 && len(s) <= 20 && siteCharsRe.MatchString(s) && strings.ContainsAny(s, "0123456789")
}
```
**Answer:**  
1. `len(s) >= 1 && len(s) <= 20`: Enforces length limits.
2. `siteCharsRe.MatchString(s)`: Restricts characters strictly to alphanumeric, underscores, and hyphens.
3. `strings.ContainsAny(s, "0123456789")`: Requires at least one numeric digit.
4. This ensures `"4"`, `"site-4"`, and `"A12"` are accepted, while common English words like `"visit"` or `"tomorrow"` in phrases like *"site visit"* are rejected.

---

### Q29: In `internal/agent/stats.go`, examine `medianDuration()`. How does it calculate the median latency?
```go
// From internal/agent/stats.go:
func medianDuration(sorted []time.Duration) time.Duration {
	n := len(sorted)
	switch {
	case n == 0:
		return 0
	case n%2 == 1:
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}
```
**Answer:**  
1. Assumes the input slice has already been sorted in ascending order.
2. If $N$ is odd (`n%2 == 1`): returns the exact element at index `n/2` (the exact midpoint).
3. If $N$ is even: averages the two middle elements `(sorted[n/2-1] + sorted[n/2]) / 2`.
4. This accurately computes the 50th percentile latency without distortion from extreme outliers.

---

### Q30: In `internal/agent/stats.go`, examine `NewLogEntry()`. How does it format telemetry for structured logging?
```go
// From internal/agent/stats.go:
func NewLogEntry(at time.Time, o Outcome) LogEntry {
	ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	e := LogEntry{
		Time: at.Format(time.RFC3339), Text: o.Text, Tier: int(o.Tier), Rule: o.Rule,
		Intent: string(o.Command.Intent), Status: string(o.Status), Attempts: o.Attempts,
		UnderstandMS: ms(o.Understand), ExecuteMS: ms(o.Execute), TotalMS: ms(o.Total), Fallback: o.Fallback,
	}
	if o.Err != nil {
		e.Error = o.Err.Error()
	}
	return e
}
```
**Answer:**  
1. `ms()` helper converts `time.Duration` nanoseconds into fractional milliseconds (`float64`).
2. Generates an RFC3339 timestamp.
3. Captures tier routing: `Tier` (0, 1, or -1) and the specific `Rule` name if Tier 0 fired.
4. Deconstructs latency into:
   - `UnderstandMS`: Time spent parsing the message (regex or LLM tokens).
   - `ExecuteMS`: Time spent running the tool in SQLite / memory.
   - `TotalMS`: End-to-end request duration.
5. Records `Fallback` reasons and error strings, providing complete structured JSONL observability for production performance auditing.
