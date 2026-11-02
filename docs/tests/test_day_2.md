# Day 2 Master Assessment: Swappable LLM Architecture, Contract Testing & Offline Execution

> **Focus:** Provider-Neutral Design, Contract Testing, Error Translation, and Offline Loopback Streaming  
> **Target Packages:** `internal/llm`, `cmd/offgrid` (`ask.go`), `internal/ollama`

---

## Overview

Day 2 of **Offgrid** establishes the core architectural boundary of the entire project: **"Make the rest of the system independent of any single model backend."**

Instead of calling Ollama directly across the codebase, Day 2 introduces an anti-corruption layer: a provider-neutral interface (`llm.Client`), rigorous behavioral contract testing, and a streaming CLI tool (`offgrid ask`) that functions seamlessly with the WiFi adapter powered off.

This document contains:
1. **20 Conceptual Questions & In-Depth Answers** (Architecture, interface design, testing philosophies, loopback networking, and error handling).
2. **10 Code-Focused Questions & In-Depth Answers** (Line-by-line Go analysis, contract test harnesses, zero-value translation traps, and Unix I/O discipline).

---

# Part 1: 20 Conceptual Questions & In-Depth Answers

---

### Q1: Why is calling an LLM provider directly (e.g., calling Ollama throughout your codebase) considered an architectural anti-pattern?
**Answer:**  
Calling a concrete provider directly creates **tight coupling**. If your agent, command parser, router, and background workers all make direct HTTP calls to Ollama:
1. Every package must import HTTP libraries, understand Ollama-specific JSON response envelopes, and parse Ollama status codes.
2. Unit testing becomes painfully slow or impossible: every single test requires a live model running on your laptop, which is slow, non-deterministic, and consumes gigabytes of RAM.
3. If you later want to route requests to a cloud model when online (e.g., Anthropic or OpenAI), use a different local engine (e.g., `llama.cpp` directly or `vLLM`), or script model responses for testing, you would have to rewrite code across dozens of files.

An abstraction layer isolates the model provider behind a single seam, so swapping backends is a one-line change.

---

### Q2: What does the Go proverb "The bigger the interface, the weaker the abstraction" mean, and how does `llm.Client` follow it?
**Answer:**  
In Go, an interface defines a behavioral contract. If an interface contains 15 different methods (e.g., `Generate()`, `Chat()`, `Embed()`, `CountTokens()`, `ListEngines()`, `StreamAudio()`), implementing that interface requires writing code for all 15 methods. Creating a test double or mock becomes a massive, fragile task.

`llm.Client` contains **exactly one method**:
```go
type Client interface {
    Chat(ctx context.Context, req Request, onToken func(string)) (Response, error)
}
```
Because the interface is minimal:
- Any backend can satisfy it by implementing just one function.
- Creating a complete test fake takes four lines of code.
- Wrapping the client with decorators (e.g., metrics logging, rate limiting, retry handlers) is trivial and clean.

---

### Q3: What is "Contract Testing", and why is it superior to traditional unit testing for swappable components?
**Answer:**  
A Go interface verifies **syntactic compatibility** at compile time (i.e., whether the types and method signatures match). It cannot guarantee **semantic behavior** (i.e., whether two different implementations actually behave the same way under identical conditions).

**Contract Testing** defines a single suite of behavioral assertions that runs against *all* implementations:
- Does it validate requests and fail fast on bad inputs?
- Does it immediately cancel when `ctx.Done()` fires?
- Does it join streamed tokens together into the exact final response text?
- Does it report outages as `ErrUnavailable` and missing models as `*ModelNotFoundError`?
- Is it safe for concurrent use by multiple goroutines?

If both the real `Ollama` adapter and the `Fake` test double pass the exact same contract test suite, you have mathematical proof that code passing under tests with `Fake` will run identically in production with `Ollama`.

---

### Q4: What is the architectural difference between a single-turn "Generate" endpoint and a multi-turn "Chat" endpoint?
**Answer:**  
- **Generate (`/api/generate`):** Takes a single raw text string (prompt) and outputs text. It has no structured concept of roles, instructions, or dialogue turns.
- **Chat (`/api/chat`):** Ingests an ordered list of structured `Message` objects, where each message has an assigned **Role**:
  - `system`: High-level instructions defining rules, persona, and output constraints.
  - `user`: The input question or command from the operator.
  - `assistant`: Previous replies emitted by the model.

Using `Chat` standardizes conversational modeling. The inference backend automatically maps these structured messages into the specific model's underlying tokenizer chat template (e.g., ChatML or Llama-3 format).

---

### Q5: Do Large/Small Language Models possess internal session memory? How does multi-turn conversational memory actually work?
**Answer:**  
**No. Models are completely stateless functions.** A neural network has no persistent memory across API requests.

Every time a user asks a follow-up question in a chat session:
1. The client application must store the entire conversation history.
2. The client packages the system prompt, all past user queries, all past assistant responses, and the *new* user query into the `Messages` array.
3. The entire history is re-sent over the wire and processed from scratch by the model.

**Architectural Consequence for Edge Devices:**  
As a conversation grows, the prompt length increases. In Day 1, we measured that prefill time (reading the prompt) scales directly with prompt length. Uncontrolled chat history causes Time-To-First-Token (TTFT) to degrade significantly on CPU hardware.

---

### Q6: Why was a synchronous callback (`onToken func(string)`) chosen for streaming tokens instead of a Go Channel or an Iterator?
**Answer:**  
There are three ways to stream data in Go:
1. **Callback (Chosen):** The function takes `onToken func(string)`. As each token chunk arrives from the HTTP stream, the callback is invoked synchronously on the caller's goroutine.  
   *Advantages:* Zero goroutine leaks, zero channel allocation overhead, zero mutex locks required by the caller.
2. **Go Channel:** The client creates a `chan string` and spawns a background goroutine to push tokens.  
   *Drawbacks:* The caller must manage channel lifecycles, handle slow consumers, avoid deadlocks, and ensure channels are closed properly during early errors.
3. **Iterators (`iter.Seq`):** Modern and elegant, but requires **Go 1.23+**. Offgrid targets Go 1.22 (the default on LTS distributions like Ubuntu 24.04 / Zorin OS 18), making callbacks the most portable and robust choice.

---

### Q7: Why does turning off your laptop's WiFi have zero effect on local LLM inference?
**Answer:**  
The local client communicates with the Ollama server via `http://127.0.0.1:11434`.

`127.0.0.1` is the standard **IPv4 loopback address** (localhost). In the Linux kernel network stack:
1. Packets directed to `127.0.0.1` are routed through the virtual `lo` (loopback) network interface.
2. The kernel detects that the source and destination IP are local to the same machine.
3. Data is transferred entirely in kernel memory buffers between the two local processes.
4. The physical network hardware (WiFi card or Ethernet controller) is completely bypassed.

The internet connection is only needed when downloading the model file originally (`ollama pull`). Once stored locally on disk, inference is 100% self-contained and offline.

---

### Q8: What is an "Anti-Corruption Layer" (ACL), and where does it exist in the Day 2 codebase?
**Answer:**  
In Domain-Driven Design (DDD), an **Anti-Corruption Layer** is a boundary that translates foreign concepts, error structures, and data schemas from an external service into your system's native domain language, preventing external details from "corrupting" your core application logic.

In Day 2, `internal/llm/ollama.go` is the Anti-Corruption Layer:
- It translates Ollama's HTTP errors into clean domain errors (`ErrUnavailable`, `ModelNotFoundError`).
- It translates Offgrid domain messages into Ollama JSON request structs.
- It translates Ollama's nanosecond counters into a standard `Usage` domain struct.
- **Rule:** Nothing in the Offgrid codebase outside of `internal/llm/ollama.go` is permitted to import `internal/ollama`.

---

### Q9: Why is it vital to distinguish `llm.ErrUnavailable` from `context.DeadlineExceeded`?
**Answer:**  
Both errors represent failures, but they require opposite recovery actions by an agent runtime:

1. **`llm.ErrUnavailable` (Infrastructure Outage):**
   - *Cause:* Ollama is not running, crashed, or the socket connection was refused.
   - *Reaction:* The runtime recognizes that local inference is down. On Day 10, the router intercepts this error and redirects the task to the **durable offline job queue** to be processed when the daemon recovers.
2. **`context.DeadlineExceeded` (Timeout):**
   - *Cause:* The server is running fine, but the model is taking longer to generate than the configured timeout (e.g., generating an extremely long text on a slow CPU).
   - *Reaction:* This is *not* an outage. The server is alive. Switching to offline queueing would not fix the issue; the user simply needs a larger timeout or a smaller model.

---

### Q10: What is the "Zero Means Default" trap in Go struct design, and how does the Ollama adapter prevent it?
**Answer:**  
In Go, all uninitialized struct fields take their "zero value" (`int` is `0`, `float64` is `0.0`, `string` is `""`). In API design, callers expect an unset field to mean "sensible default":
- `Request.MaxTokens == 0` is expected to mean "unlimited tokens".
- `Request.Seed == 0` is expected to mean "use a random seed".

**The Trap:**  
Ollama takes `num_predict: 0` literally as *"generate 0 tokens"*, and `seed: 0` literally as *"use fixed seed 0"*. If passed directly, an unconfigured request would output zero text and always repeat identically.

**The Solution:**  
The adapter translates these semantics explicitly:
```go
numPredict := req.MaxTokens
if numPredict <= 0 {
    numPredict = -1 // Ollama's sentinel value for unlimited
}
seed := req.Seed
if seed == 0 {
    seed = 1 + rand.Intn(1<<31-1) // Force a fresh random seed
}
```

---

### Q11: Why is `Temperature: 0` treated as a valid functional setting rather than a default to be overridden?
**Answer:**  
Unlike `MaxTokens: 0` or `Seed: 0`, a temperature of `0.0` has an explicit mathematical meaning in generative models: **Greedy Decoding**. It forces the neural network to select the token with the highest logit probability at every single step, removing randomness.

In deterministic AI agent systems (such as intent parsing, JSON extraction, and database tool calling), `Temperature: 0` is the primary desired setting. Therefore, the adapter preserves `0.0` exactly and does not replace it with a non-zero default.

---

### Q12: Where should Go interfaces be declared? Contrast `bench.Generator` (Day 1) with `llm.Client` (Day 2).
**Answer:**  
- **Single-Consumer Rule (`bench.Generator`):** When an interface exists solely to satisfy the needs of one specific package, declare it inside the consuming package. `package bench` needs only one method (`Generate`), so `bench.Generator` belongs in `bench`.
- **Shared Seam / Architectural Port Rule (`llm.Client`):** When an interface represents the central boundary of the entire system—shared across the CLI, the agent orchestrator, the router, and background queue workers—declaring it in a consumer package would create circular dependencies. Instead, it lives in its own domain package (`internal/llm`) alongside its domain types (`Request`, `Response`, `Message`).

---

### Q13: What does the compile-time statement `var _ Client = (*Ollama)(nil)` achieve?
**Answer:**  
Because Go uses structural typing (interfaces are implemented implicitly without an `implements` keyword), a programmer could accidentally modify a method signature in `Ollama` without realizing they broke the interface contract until a distant package fails to compile.

`var _ Client = (*Ollama)(nil)`:
1. Allocates zero memory (it uses a typed `nil` pointer).
2. Assigns the `*Ollama` pointer to the blank identifier `_` typed as `Client`.
3. If `*Ollama` ever fails to satisfy `Client`, the Go compiler refuses to build and flags the exact line in `ollama.go`, preventing interface drift.

---

### Q14: Why must a test double (`llm.Fake`) be placed in an exported, non-test file (`fake.go`) rather than `fake_test.go`?
**Answer:**  
Files ending in `_test.go` are only compiled when running tests *within that specific package*. External packages cannot import them.

Later in the roadmap:
- Day 5 (Agent loop tests)
- Day 10 (Router failover tests)
- Day 12 (Queue worker tests)

All these external packages need a test double they can script without spinning up Ollama. By placing `Fake` in `internal/llm/fake.go` (unexported to the outside world because of `internal/`, but exported to the module), every package in the Offgrid project can reuse the exact same deterministic test double.

---

### Q15: Why is it critical to "break your code on purpose" when writing contract tests?
**Answer:**  
A test that passes when your code is broken is worse than no test at all—it gives false confidence.

To verify test effectiveness:
1. Intentionally comment out the error translation logic (e.g., return raw HTTP errors instead of `ErrUnavailable`).
2. Run `go test`. The contract test *must* fail.
3. Intentionally change `numPredict` translation so it sends `0` instead of `-1`. The contract test *must* fail.
4. Once tests visibly fail on buggy behavior, restore the correct implementation. This proves the test suite actively catches real regressions.

---

### Q16: Why does `offgrid ask` separate `cmdAsk` from `runAsk`?
**Answer:**  
This is the **Humble Object Pattern**:
- **`cmdAsk` (The Shell):** Deals with the messy external world. It parses command-line flags, reads environment variables, sets up OS interrupt signals, and instantiates the concrete `Ollama` client.
- **`runAsk` (The Core Logic):** Contains pure business logic. It takes `llm.Client` as a generic parameter, streams tokens, writes output, and formats statistics.

Because `runAsk` accepts any `llm.Client`, unit tests can invoke `runAsk` directly using `llm.Fake`, testing prompt piping, stdout streaming, and flag handling in microseconds with zero network calls.

---

### Q17: What is the Unix standard for separating stdout and stderr, and how does `offgrid ask` adhere to it?
**Answer:**  
In the Unix philosophy:
- **`stdout` (Standard Output):** Reserved exclusively for the primary data/payload of the program.
- **`stderr` (Standard Error):** Reserved for diagnostic messages, progress indicators, errors, and performance metrics.

In `offgrid ask`:
- The LLM's generated response streams exclusively to `stdout`.
- Timing and token statistics (when `-stats` is enabled) are printed exclusively to `stderr`.

This ensures that piping commands works cleanly:
```bash
./bin/offgrid ask -stats "Write a SQL query" > query.sql
```
`query.sql` receives only pure SQL code, while performance metrics still display visibly on the user's terminal screen.

---

### Q18: How does `offgrid ask` detect whether stdin is an interactive keyboard or a Unix pipe?
**Answer:**  
If a user runs `./bin/offgrid ask` with no arguments, does the program expect input from a pipe (e.g., `cat doc.txt | offgrid ask`), or is the user sitting at an interactive terminal?

If the program naively calls `io.ReadAll(os.Stdin)` when run interactively, the process will **hang forever**, waiting for an EOF that never arrives.
By inspecting `os.Stdin.Stat()` and checking the character device bit:
```go
fi.Mode() & os.ModeCharDevice != 0
```
The program immediately detects if `stdin` is a human keyboard. If so, it refuses to block and displays a helpful usage error instead.

---

### Q19: Why does `reportAskError` map `context.Canceled` specifically to exit code 130?
**Answer:**  
In Unix operating systems, when a process is terminated by the user via `Ctrl+C`, the kernel sends `SIGINT` (signal 2). The standard shell convention for fatal signal termination is:
$$\text{Exit Code} = 128 + \text{Signal Number} = 128 + 2 = 130$$
By catching `context.Canceled` and returning exit code 130, `offgrid ask` behaves as a compliant Unix command-line utility, allowing shell scripts and Makefiles to detect user cancellations correctly.

---

### Q20: How does `Request.Validate()` enforce the principle of "Fail Fast"?
**Answer:**  
If a caller submits an invalid request (e.g., empty model name, zero messages, negative max tokens, or an invalid JSON schema), contacting the model server only to receive a failure seconds later wastes CPU cycles and battery.

`Request.Validate()` executes synchronously before any network socket is touched:
- Checks required fields.
- Verifies message roles.
- Validates JSON schema syntax.
If validation fails, it returns `ErrInvalidRequest` immediately in **0.00 milliseconds**.

---

# Part 2: 10 Code-Focused Questions & Line-by-Line Answers

---

### Q21: In `internal/llm/llm.go`, examine `Request.Validate()`. Why does it require that the last message must have `RoleUser`?
```go
// From internal/llm/llm.go:
if r.Messages[len(r.Messages)-1].Role != RoleUser {
    return fmt.Errorf("%w: the last message must be from the user", ErrInvalidRequest)
}
```
**Answer:**  
In chat completion models, the model generates an assistant response *in reply to* the user's latest prompt. If the final message in the array is an `assistant` turn or a `system` instruction:
1. Some model tokenizers will malfunction or output empty strings because the prompt template expects a trailing user prompt to trigger generation.
2. In an agent workflow, asking a model to complete when the user hasn't asked a question represents a logic flaw in the caller. Enforcing that the final message is from the user guarantees structural integrity before calling the backend.

---

### Q22: In `internal/llm/ollama.go`, how does `translateError()` preserve both the sentinel domain error and the underlying network cause?
```go
// From internal/llm/ollama.go:
func translateError(err error, model string) error {
	var apiErr *ollama.APIError
	switch {
	case errors.Is(err, ollama.ErrUnreachable):
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	case errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound:
		return &ModelNotFoundError{Model: model, Err: err}
	default:
		return err
	}
}
```
**Answer:**  
1. When Ollama is unreachable, `fmt.Errorf("%w: %w", ErrUnavailable, err)` wraps **both** errors into Go 1.20+'s multi-error chain.
2. Callers checking `errors.Is(err, llm.ErrUnavailable)` match successfully, allowing the router to trigger offline failover logic.
3. At the same time, because the original `err` is preserved, logging the error prints the exact connection failure (e.g., `dial tcp 127.0.0.1:11434: connect: connection refused`).
4. For missing models (HTTP 404), it wraps the error in `*ModelNotFoundError`, storing the model tag for user advice.

---

### Q23: In `internal/llm/fake.go`, how does `Fake.Chat()` simulate realistic streaming without calling any external libraries?
```go
// From internal/llm/fake.go:
tokens := 0
for _, piece := range strings.SplitAfter(text, " ") {
    if piece == "" {
        continue
    }
    if err := ctx.Err(); err != nil {
        return Response{}, err
    }
    if onToken != nil {
        onToken(piece)
    }
    tokens++
}
return Response{Text: text, Model: req.Model, Usage: Usage{CompletionTokens: tokens}}, nil
```
**Answer:**  
1. `strings.SplitAfter(text, " ")` slices the generated mock reply string after each space character (e.g., `"Hello "`, `"world "`):
2. Slicing with spaces preserves natural token boundaries so that concatenating all emitted pieces reconstructs the original string exactly.
3. Before every piece is delivered, it checks `if err := ctx.Err(); err != nil`, ensuring that context cancellations during streaming are respected.
4. It invokes `onToken(piece)` if provided, and tallies `tokens` to populate the `Usage` struct accurately.

---

### Q24: In `internal/llm/fake.go`, why does `Calls()` use a mutex lock and defensive slice copying?
```go
// From internal/llm/fake.go:
type Fake struct {
    Handler func(ctx context.Context, req Request) (string, error)
    mu      sync.Mutex
    calls   []Request
}

func (f *Fake) Calls() []Request {
    f.mu.Lock()
    defer f.mu.Unlock()
    return append([]Request(nil), f.calls...)
}
```
**Answer:**  
1. `f.mu.Lock()` ensures thread safety. The `Client` contract explicitly requires that implementations are safe for concurrent use across multiple goroutines.
2. `append([]Request(nil), f.calls...)` creates a shallow copy of the slice. If it returned `f.calls` directly, an external test goroutine inspecting the slice could trigger a data race with another goroutine invoking `Chat()` simultaneously.

---

### Q25: In `internal/llm/contract_test.go`, how does the parameterized table test guarantee that `Fake` and `Ollama` satisfy the identical behavioral contract?
```go
// From internal/llm/contract_test.go:
func TestClientContract(t *testing.T) {
	implementations := map[string]func(t *testing.T) Client{
		"Fake": func(t *testing.T) Client { return &Fake{} },
		"Ollama": func(t *testing.T) Client {
			return NewOllama(ollama.NewClient(chatServer(t).URL))
		},
	}

	for name, newClient := range implementations {
		t.Run(name, func(t *testing.T) {
			t.Run("streamed pieces join to the final text", func(t *testing.T) {
                // ...
```
**Answer:**  
Instead of writing separate, disparate unit tests for each implementation:
1. `implementations` maps backend names to constructor factory functions.
2. For each backend, Go runs the identical suite of subtests:
   - Piece concatenation equality (`strings.Join(pieces, "") == resp.Text`).
   - Acceptance of `nil` token callback handlers.
   - Cancellation honoring (`context.Canceled`).
   - Rejection of invalid requests (`ErrInvalidRequest`).
   - Concurrency safety across concurrent goroutines.
3. If a future developer writes a cloud backend (e.g., `AnthropicClient`), adding one entry to `implementations` immediately verifies that it meets the exact behavioral standard.

---

### Q26: In `cmd/offgrid/ask.go`, examine lines 64–66. Why is this considered the single most important architectural line in Day 2?
```go
// From cmd/offgrid/ask.go:
// This is the only line that knows the backend is Ollama. Replacing it is
// all it takes to point the whole program at a different model server.
var client llm.Client = llm.NewOllama(ollama.NewClient(normalizeHost(*host)))
```
**Answer:**  
This is the **Composition Root**:
- Everything upstream (flag parsing, CLI argument handling) knows nothing about model providers.
- Everything downstream (`runAsk()`) receives the abstract `llm.Client` interface.
- This single line is the only place in the command where the concrete adapter `llm.NewOllama` is instantiated. Replacing this one line with `llm.NewCloud(...)` or `llm.NewFake(...)` redirects the entire application without touching another line of code.

---

### Q27: In `cmd/offgrid/ask.go`, how does `runAsk()` prevent output corruption when an error occurs halfway through streaming?
```go
// From cmd/offgrid/ask.go:
var wrote bool
var lastByte byte
endLine := func() {
    if wrote && lastByte != '\n' {
        fmt.Fprintln(stdout)
    }
}

resp, err := c.Chat(ctx, ..., func(piece string) {
    fmt.Fprint(stdout, piece)
    wrote = true
    lastByte = piece[len(piece)-1]
})
endLine() // Executed immediately after Chat returns, even if err != nil!
if err != nil {
    return err
}
```
**Answer:**  
If a streaming response fails midway through (e.g., network timeout or user press of `Ctrl+C`), the terminal cursor is left floating at the end of the half-completed line of text.
1. `wrote` tracks if any bytes were emitted to stdout.
2. `lastByte` tracks the final printed character.
3. `endLine()` checks if text was printed without a trailing newline, and emits `\n`.
4. Because `endLine()` is called *before* checking `if err != nil`, the terminal prompt cleanly starts on a fresh line rather than gluing the error message onto the end of the truncated sentence.

---

### Q28: In `cmd/offgrid/ask.go`, examine `readPrompt()`. How does it handle both command line arguments and piped standard input?
```go
// From cmd/offgrid/ask.go:
func readPrompt(args []string, stdin io.Reader) (string, error) {
	prompt := strings.TrimSpace(strings.Join(args, " "))
	if prompt != "" || isTerminal(stdin) {
		return prompt, nil
	}
	b, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
```
**Answer:**  
1. If the user passed prompt text via CLI flags (e.g., `offgrid ask "what is a CRDT?"`), `prompt != ""` is true, so it returns immediately without checking `stdin`.
2. If `args` is empty, it checks `isTerminal(stdin)`. If the user is typing interactively, it returns empty (prompting the help text) rather than hanging on `io.ReadAll`.
3. If input is piped in (`cat file.txt | offgrid ask`), `isTerminal` is false. It safely reads up to 1 MB (`1<<20`) using `io.LimitReader` to prevent memory exhaustion from giant inputs.

---

### Q29: In `cmd/offgrid/ask.go`, how does `isTerminal()` inspect OS file descriptor flags on Linux?
```go
// From cmd/offgrid/ask.go:
func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
```
**Answer:**  
1. Type asserts `r` to `*os.File`. If `stdin` is a mock buffer in a unit test, it returns `false`.
2. Calls `f.Stat()` to retrieve the Linux file mode metadata.
3. Tests the bitwise flag `fi.Mode() & os.ModeCharDevice != 0`. On Unix systems, interactive terminals (TTY/PTY) are character devices (`c`), whereas pipes and redirected files are FIFOs or regular files (`-`).
4. This reliably detects whether a real human is sitting at the terminal keyboard.

---

### Q30: In `internal/llm/contract_test.go`, how does `chatServer()` use `http.Flusher` to simulate real-time streaming in tests?
```go
// From internal/llm/contract_test.go:
for _, line := range []string{
    `{"message":{"role":"assistant","content":"Hello"},"done":false}`,
    `{"message":{"role":"assistant","content":" world"},"done":false}`,
    // ...
} {
    fmt.Fprintln(w, line)
    if f, ok := w.(http.Flusher); ok {
        f.Flush()
    }
}
```
**Answer:**  
In standard Go HTTP servers, response writes are buffered in memory to optimize TCP packet efficiency. If not flushed, all JSON chunks would be buffered and sent together in a single packet at connection close, which would disguise any streaming bugs in the client.

By type-asserting `w` to `http.Flusher` and calling `f.Flush()` after writing each individual line, the test server forces the OS socket to transmit each NDJSON chunk immediately, faithfully simulating a real-time streaming LLM.
