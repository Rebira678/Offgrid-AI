# Day 1 Master Assessment: Hardware Benchmarking & Local SLM Foundations

> **Focus:** Roadmap Milestones, Day 1 Engineering Notes, and Direct Code Implementation  
> **Target Packages:** `internal/bench`, `internal/ollama`, `internal/sysinfo`, `cmd/offgrid`

---

## Overview

Day 1 of **Offgrid** sets the fundamental engineering rule: **"Measure before you design."**  
Before writing agent loops, parsers, or storage engines, an engineer must know the real, unvarnished physical capabilities of their target machine.

This document contains:
1. **20 Conceptual Questions & In-Depth Answers** (Theory, physics of memory bandwidth, quantization, and experimental controls).
2. **10 Code-Focused Questions & In-Depth Answers** (Line-by-line Go architecture, streaming NDJSON, interface segregation, and signal handling).

---

# Part 1: 20 Conceptual Questions & In-Depth Answers

---

### Q1: Why must an offline-first AI project begin with hardware benchmarking rather than prompt engineering or agent loops?
**Answer:**  
In cloud-based AI systems, compute and memory are treated as effectively infinite because serverless APIs abstract away the hardware. In an offline-first environment (e.g., remote field laptops, utility trucks, disaster zones), your code runs entirely on local, constrained consumer silicon with zero GPU acceleration.

If an engineer designs complex multi-step agent loops without knowing the laptop's real token generation speed (decode) and prompt reading speed (prefill), the agent may take 45 seconds just to process a single user request. Benchmarking on Day 1 provides empirical reality: it reveals whether a model runs at 25 tokens/sec, 12 tokens/sec, or 3 tokens/sec. These measurements dictate your entire system architecture—such as the necessity of a Tier 0 deterministic rules engine that bypasses the model for predictable tasks.

---

### Q2: What is the physical and mathematical difference between the "Prefill" phase and the "Decode" phase in language models?
**Answer:**  
When an LLM processes text, it does so in two fundamentally distinct operational phases:

1. **Prefill (Prompt Processing):**
   - **What happens:** The model ingests the entire user prompt simultaneously.
   - **Compute characteristic:** Highly parallelizable matrix multiplications. The CPU cores can work together concurrently across all input tokens.
   - **Bottleneck:** **Compute-bound** (CPU execution units / SIMD / AVX instructions).
   - **Metric:** Prompt tokens processed per second (`prompt_tokens_per_sec`).

2. **Decode (Token Generation):**
   - **What happens:** The model outputs the answer sequentially, one token at a time. Each new token depends on all preceding tokens (autoregressive generation).
   - **Compute characteristic:** To generate just *one* token, the CPU must read every single weight parameter of the entire model out of RAM into the CPU cache.
   - **Bottleneck:** **Memory bandwidth-bound** (RAM bus transfer speed).
   - **Metric:** Generation tokens per second (`gen_tokens_per_sec`).

---

### Q3: Why is token generation speed (decode) on a CPU-only laptop bound by memory bandwidth rather than CPU clock frequency?
**Answer:**  
In neural network inference during the autoregressive decode phase, calculating the probability of the next token requires multiplying the current hidden state vector by every weight matrix in the model.

Because each model weight is used only once per generated token, the CPU cannot keep the weights in fast L1/L2/L3 caches. It must stream the entire model out of system RAM over the motherboard bus. 
- A 3B parameter model quantized to 4 bits requires reading approximately **2.0 Gigabytes of data from RAM for every single token**.
- If a laptop's RAM bandwidth is 30 GB/s, the absolute physical ceiling is:
  $$\text{Max Tokens/sec} \approx \frac{30\text{ GB/s}}{2.0\text{ GB/token}} = 15\text{ tokens/sec}$$
Even if your CPU has 64 high-frequency cores, those cores sit idle waiting for RAM to deliver the weights. Therefore, decode throughput is strictly memory bandwidth-bound.

---

### Q4: What is the back-of-the-envelope formula for estimating theoretical token generation speed, and what does it reveal about model parameter size?
**Answer:**  
The back-of-the-envelope formula is:

$$\text{Tokens per second} \le \frac{\text{System Memory Bandwidth (GB/s)}}{\text{Model Size in RAM (GB)}}$$

**Key Realization:**  
Because memory bandwidth is fixed by your laptop's physical RAM hardware (e.g., dual-channel DDR4-3200 provides a realistic usable throughput around 30–35 GB/s):
- **1B model (~1.3 GB in RAM):** $30 / 1.3 \approx 23$ tok/s.
- **3B model (~2.0 GB in RAM):** $30 / 2.0 \approx 15$ tok/s.
- **7B model (~4.8 GB in RAM):** $30 / 4.8 \approx 6$ tok/s.

**Conclusion:** A model that is twice as large is fundamentally **twice as slow**, regardless of how well your software is optimized. This physical law directly justifies why Offgrid uses small 1B/3B models for real-time edge interactions.

---

### Q5: What is weight quantization (e.g., 4-bit `Q4_K_M`), and how does it compress models without severe loss of reasoning?
**Answer:**  
Standard deep learning models are trained using 16-bit floating-point numbers (`FP16` or `BF16`), where each weight parameter occupies 2 bytes (16 bits). A 3-billion parameter model in FP16 consumes $3 \times 10^9 \times 2\text{ bytes} \approx 6.0\text{ GB}$.

**Quantization** is the mathematical process of mapping continuous 16-bit floating-point numbers into lower-bit discrete representations (typically 4 bits per weight, such as `Q4_K_M`):
1. Instead of storing 16 bits per number, it groups weights into blocks, calculates a shared scale factor and minimum offset, and stores each individual weight as an integer between 0 and 15 (4 bits).
2. A 4-bit quantization reduces memory footprint by approximately **70–75%**, compressing a 6 GB model down to ~2 GB.
3. Because neural networks are inherently over-parameterized and robust to minor precision noise, 4-bit modern quantization schemes preserve roughly 98–99% of the model's original language comprehension while tripling inference speed on bandwidth-limited hardware.

---

### Q6: What is Time-To-First-Token (TTFT), and why is it the most critical metric for user experience?
**Answer:**  
**Time-To-First-Token (TTFT)** is the duration between the exact moment the client sends an HTTP request and the arrival of the very first token emitted by the model.

Mathematically:
$$\text{TTFT} = \text{Network Latency} + \text{Model RAM Load Time (if cold)} + \text{Prefill Time (Prompt Eval Duration)}$$

**Why it matters for user experience:**
Human perception of responsiveness is dominated by TTFT. Once tokens start streaming onto a screen at 10–15 tokens/sec, a user can read in real time and feels the system is fast. However, if TTFT is 8 seconds (for example, reading a lengthy equipment maintenance log during prefill), the user stares at a frozen screen thinking the program has crashed. Managing and minimizing TTFT is paramount in interactive terminal applications.

---

### Q7: What is the distinction between a "Cold Start" and a "Warm Start", and why is mixing them together a fatal benchmarking error?
**Answer:**  
- **Cold Start:** The model file is currently stored on disk (NVMe/SSD). When the first request arrives, the OS must read 2 to 5 Gigabytes from disk, allocate system RAM, map memory pages, and initialize computation tensors. This initial load can take 3 to 10 seconds.
- **Warm Start:** The model's weights already reside in active RAM pages, ready for immediate tensor operations.

**Why mixing them is a fatal error:**  
If a benchmark includes the disk load time inside the token generation rate calculation, the resulting numbers are meaningless. For example, if generating 10 tokens takes 1 second of compute but 4 seconds of disk loading, reporting 2 tokens/sec conflates disk I/O performance with CPU inference throughput. A proper benchmark must separate Model Load Time from Active Inference.

---

### Q8: Why does the Offgrid benchmark harness execute an explicit warm-up request with `keep_alive = "10m"`?
**Answer:**  
1. **Warm-up Request:** Before recording any timed runs, the benchmark sends a short request (`"Say hi."`, 8 tokens). This forces the Ollama daemon to read the weights from disk into RAM, initialize memory buffers, and run initial CPU kernel compilation. The duration is recorded strictly as `LoadSeconds`.
2. **`keep_alive: "10m"`:** By default, Ollama unloads models from RAM after 5 minutes of inactivity to save system resources. Passing `keep_alive: "10m"` explicitly instructs the Ollama engine to keep the model pinned in memory for at least 10 minutes, ensuring that all subsequent benchmark iterations measure pure warm-start inference speed.

---

### Q9: Why does the benchmark report the Median of multiple runs rather than the Arithmetic Mean?
**Answer:**  
The **arithmetic mean (average)** is extremely sensitive to outliers. In a desktop or laptop environment, background operating system events (such as a browser tab re-rendering, an OS background updater, or a momentary CPU thermal throttle) can cause one run out of three to take 12 seconds instead of 2 seconds. The arithmetic mean would be heavily skewed, giving an inaccurate representation of typical system behavior.

The **median** sorts the samples and selects the middle value:
$$\text{samples} = [14.2, 14.5, 29.8] \implies \text{Median} = 14.5\text{ tok/s (Mean would be } 19.5\text{ tok/s)}$$
The median completely discards the anomalous noisy run, reflecting the true, repeatable performance of the hardware.

---

### Q10: How does Ollama's prompt-prefix KV cache work, and why does prepending a unique `Request ID` salt prevent synthetic speed inflation?
**Answer:**  
Ollama (built on `llama.cpp`) utilizes **KV Cache reuse**. If two consecutive requests begin with identical prompt text, the engine reuses the precomputed Key-Value attention states from the previous run, skipping the prefill phase almost entirely.

If a benchmark ran the exact same prompt three times in a row:
- Run 1 would do full prompt processing.
- Runs 2 and 3 would reuse the cache, reporting artificial prefill speeds of thousands of tokens per second.

**The Solution in Offgrid:**  
Every benchmark prompt is dynamically prefixed with a unique salt:
```go
fmt.Sprintf("Request ID: %s/%s/%d.\n\n%s", cfg.Salt, p.Name, i, p.Text)
```
Because the very first line is unique on every single request, the KV cache cannot be matched, forcing the CPU to read and process the entire prompt honestly every time.

---

### Q11: Why are inference parameters locked to `Temperature: 0` and a fixed `Seed: 42` during hardware evaluation?
**Answer:**  
In probabilistic generation:
- Higher temperatures introduce non-deterministic sampling, meaning the model might choose shorter or longer vocabulary branches, select different token lengths, or trigger earlier EOS (End Of Sequence) tokens.
- Setting `Temperature: 0` (greedy decoding) forces the model to choose the mathematically most probable token at every step.
- Fixing `Seed: 42` guarantees that pseudo-random operations (if any tie-breaking occurs) behave identically.

This eliminates linguistic randomness so that differences in execution time reflect **pure hardware performance**, not different text outputs.

---

### Q12: Why does the benchmark enforce an output generation ceiling (e.g., `num_predict: 96` / `MaxTokens`) across runs?
**Answer:**  
If a model is asked an open-ended question without a token cap, Run 1 might output a 30-word response and Run 2 might output a 400-word essay. Comparing two runs that generated vastly different token counts introduces variable memory heating, differing CPU thermal boost decay, and inconsistent decode durations. Capping generation at a fixed count (e.g., 96 tokens) ensures that every run performs equivalent work.

---

### Q13: How do Day 1 benchmark results justify the multi-tier architectural decisions implemented in later days?
**Answer:**  
Day 1 reveals that running even the smallest local model (Llama 3.2 1B) on an AMD Ryzen 5 CPU takes **700 to 1,000 milliseconds** just to start emitting text (TTFT), plus 50–100 milliseconds per token.

If a field worker types `"list tasks"` or `"status"`, spending 1.5 seconds and spinning CPU fans to extract a structured intent is wasteful. Day 1 measurements provide the empirical justification for:
- **Tier 0 (Deterministic Regex Engine):** Resolves common commands in **26 microseconds** (100,000x faster than the model).
- **Tier 1 (Local SLM):** Reserved only for complex, unstructured natural language where rules fail.
- **Tier 2 (Offline Queue):** Heavy tasks (7B/8B models) must be queued asynchronously because they take 15–30 seconds.

---

### Q14: Why is a 7B or 8B model impractical for real-time interactive tasks on a 16 GB laptop without a GPU?
**Answer:**  
1. **Memory Pressure:** An 8B model quantized to 4 bits requires ~5 GB of RAM. When combined with OS overhead, background tools, and process memory, the system risks memory swapping to disk, which destroys performance.
2. **Decode Latency:** At an empirical generation rate of 5–8 tokens/sec, an 80-token response takes 10 to 16 seconds.
3. **Prefill Latency:** Processing an 800-token prompt on an 8B model takes 15 to 25 seconds before the first token appears.  
A user waiting 30 seconds for a basic command response cannot use the tool effectively in the field. Hence, 7B/8B models belong in background batch queues, not synchronous interactive loops.

---

### Q15: What is the Ollama daemon, and how does Offgrid interact with it at the process and networking layers?
**Answer:**  
The **Ollama daemon** (`ollama serve`) is a background service written in Go and C++ that manages model storage, weight loading, and native hardware execution via `llama.cpp`.

Offgrid interacts with Ollama over standard **HTTP/1.1 REST endpoints** on `http://127.0.0.1:11434`:
- `GET /api/tags`: Enumerates installed models, file sizes, parameter counts, and quantization levels.
- `GET /api/version`: Retrieves the server release version.
- `POST /api/generate`: Initiates streaming text generation using Newline-Delimited JSON (NDJSON).

Offgrid treats Ollama as an external local service: it contains zero CGO bindings to llama.cpp itself, keeping the Offgrid binary pure, lightweight, and cleanly decoupled.

---

### Q16: Why must every benchmark table explicitly print system hardware metadata (CPU, threads, RAM, OS)?
**Answer:**  
A benchmark stating `"Llama 3.2 3B runs at 14.5 tokens/sec"` is meaningless in isolation. A reader has no way of knowing whether that number was achieved on an M3 Max MacBook with 400 GB/s memory bandwidth, an NVIDIA RTX 4090 GPU, or an entry-level budget laptop.

By binding every benchmark result to its environment—e.g., `AMD Ryzen 5 PRO 5650U, 12 threads, 15.0 GiB RAM, Zorin OS 18.1`—the numbers become **reproducible scientific facts**. Any engineer with similar hardware can replicate the exact numbers.

---

### Q17: Why does the benchmark system export both a Markdown table and a JSON artifact?
**Answer:**  
- **Markdown (`docs/benchmarks.md`):** Formatted for human readability. It can be directly embedded into project documentation, GitHub READMEs, and technical blog posts.
- **JSON (`docs/benchmarks.json`):** Formatted for machine ingestion. It allows automated CI/CD scripts to parse benchmark data, detect performance regressions, compute statistical trends, and render SVG performance graphs over time.

---

### Q18: What is the difference between client-side Wall Clock Time and internal server-reported timing duration?
**Answer:**  
- **Client-Side Wall Time (`res.Wall = time.Since(start)`):** Measured directly in Go code from before the HTTP request is dispatched until the final EOF/done chunk is read. It includes TCP handshake latency, OS socket scheduling, HTTP header serialization, and JSON deserialization.
- **Server-Reported Durations (`ch.TotalDuration`, `ch.EvalDuration`):** Reported directly by the Ollama engine in nanoseconds. `EvalDuration` measures pure GPU/CPU tensor execution time inside the inference engine.  
Client-side wall time is always slightly greater than server total time; observing this delta ensures that client-side overhead remains minimal (<1 ms).

---

### Q19: How do laptop power profiles and thermal states affect CPU benchmark integrity?
**Answer:**  
Modern mobile processors use dynamic boost frequencies (e.g., AMD Precision Boost / Intel Turbo Boost). When cold, a CPU may boost to 4.2 GHz, but under sustained multi-core AVX loads, temperatures rise toward 85°C–90°C, causing the hardware to throttle clock speeds down to 2.8 GHz to prevent overheating.

Furthermore, running on battery triggers conservative OS power-saving governors that reduce RAM bus frequency. To ensure integrity:
1. Always keep the laptop plugged into AC mains.
2. Select high-performance power profiles in OS settings.
3. Close memory-hungry applications (e.g., web browsers) before running tests.

---

### Q20: What is the "Honest Numbers Only" philosophy, and why is an empty benchmark table better than a placeholder estimate?
**Answer:**  
In AI engineering, marketing claims often publish theoretical peak numbers that collapse in real-world deployment. The "Honest Numbers Only" philosophy states: **Never publish, document, or promise a performance number that was not directly measured on real hardware.**

An empty benchmark table in documentation signals: *"These numbers have not yet been evaluated on this specific machine."* An estimate or synthetic number, conversely, creates false expectations and can be quoted back at you as an error. Engineering trust is built on verifiable measurements.

---

# Part 2: 10 Code-Focused Questions & Line-by-Line Answers

---

### Q21: In `internal/ollama/client.go`, why does the `http.Client` explicitly have NO global `Timeout` field set?
```go
// From internal/ollama/client.go:
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

func NewClient(baseURL string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{}, // <-- No Timeout configured here!
	}
}
```
**Answer:**  
In Go's standard library, setting `http.Client.Timeout` applies a hard deadline to the **entire request-response lifecycle**, including the time spent reading the response body.

In a streaming LLM client:
- An LLM may take 30 to 60 seconds to complete streaming a large response.
- If `http.Client.Timeout` were set to 10 seconds, Go's HTTP runtime would forcefully kill the connection halfway through the response, even if tokens were streaming normally.

Instead, deadlines are applied per-request using `context.Context` (e.g., `context.WithTimeout(ctx, cfg.RunTimeout)`). This allows fine-grained control, such as canceling during initial connection hangs while allowing long streaming responses to complete.

---

### Q22: In `internal/ollama/client.go`, why does `stream()` use `json.NewDecoder(resp.Body)` instead of `bufio.Scanner` to parse NDJSON?
```go
// From internal/ollama/client.go:
dec := json.NewDecoder(resp.Body)
for {
	var ch streamChunk
	if err := dec.Decode(&ch); err != nil {
		// ...
```
**Answer:**  
`bufio.Scanner` is commonly used to read line-by-line text, but by default it has a maximum buffer capacity of **64 KiB** (`bufio.MaxScanTokenSize = 64 * 1024`). If Ollama ever emits a single JSON chunk containing a large payload or embedding that exceeds 64 KiB, `bufio.Scanner` fails immediately with `bufio.ErrTooLong`.

`json.NewDecoder` operates directly on the underlying `io.Reader` without arbitrary line-length constraints. It parses individual JSON objects sequentially from the byte stream, making it safer for streaming network payloads.

---

### Q23: How does the `streamChunk` struct unify responses from both `/api/generate` and `/api/chat` in a single loop?
```go
// From internal/ollama/client.go:
type streamChunk struct {
	Response string `json:"response"` // Populated by /api/generate
	Message  struct {
		Content string `json:"content"` // Populated by /api/chat
	} `json:"message"`
	Done bool `json:"done"`
	// ...
}
```
```go
// Inside stream():
if piece := ch.Response + ch.Message.Content; piece != "" {
	// ...
}
```
**Answer:**  
Ollama's API returns text under different JSON keys depending on the endpoint:
- `/api/generate` provides text in the top-level string `"response": "..."`.
- `/api/chat` provides text nested inside an object `"message": {"content": "..."}`.

By defining both fields in `streamChunk`, `json.NewDecoder` unmarshals whichever field exists while leaving the other as an empty string. The expression `piece := ch.Response + ch.Message.Content` concatenates them safely. Since only one endpoint is called per request, `piece` receives the token text regardless of which API generated it, avoiding duplicate streaming logic.

---

### Q24: How does `stream()` calculate client-side `TimeToFirstToken` (TTFT)?
```go
// From internal/ollama/client.go:
start := time.Now()
// ...
sawFirst := false
for {
	// ...
	if piece := ch.Response + ch.Message.Content; piece != "" {
		if !sawFirst {
			sawFirst = true
			res.TimeToFirstToken = time.Since(start)
		}
		text.WriteString(piece)
		if onToken != nil {
			onToken(piece)
		}
	}
	// ...
}
```
**Answer:**  
1. `start := time.Now()` captures the client timestamp immediately before initiating the HTTP request.
2. The boolean flag `sawFirst` is initialized to `false`.
3. The client loops over incoming JSON chunks. As soon as a chunk arrives where `piece != ""`, the branch `if !sawFirst` executes exactly once.
4. It sets `sawFirst = true` and records `res.TimeToFirstToken = time.Since(start)`.
5. All subsequent chunks skip this block. This captures the client's perceived latency before text generation began.

---

### Q25: How does `internal/ollama/client.go` handle errors using Go's modern error wrapping (`%w`) and sentinel types?
```go
// From internal/ollama/client.go:
var ErrUnreachable = errors.New("cannot reach Ollama")

type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("ollama: HTTP %d: %s", e.Status, e.Message)
}
```
```go
// Inside do():
resp, err := c.HTTP.Do(req)
if err != nil {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	return nil, fmt.Errorf("%w at %s: %v", ErrUnreachable, c.BaseURL, err)
}
if resp.StatusCode >= 400 {
	defer resp.Body.Close()
	return nil, readAPIError(resp)
}
```
**Answer:**  
The client categorizes failures into three distinct error patterns:
1. **Sentinel Errors (`ErrUnreachable`):** When the connection is refused (e.g., daemon not running), it wraps `ErrUnreachable` using `%w`. Callers check this cleanly via `errors.Is(err, ollama.ErrUnreachable)`.
2. **Typed Error (`*APIError`):** When the server responds with HTTP $\ge 400$ (e.g., model not found 404), it parses the server's JSON error and returns a pointer to `APIError`. Callers extract the status code using `errors.As(err, &apiErr)`.
3. **Context Cancellation:** If the user presses `Ctrl+C` or a timeout expires, `ctx.Err()` returns `context.Canceled` or `context.DeadlineExceeded`, allowing clean termination.

---

### Q26: Why is the `Generator` interface defined inside `package bench` instead of `package ollama`?
```go
// From internal/bench/bench.go:
type Generator interface {
	Generate(ctx context.Context, req ollama.GenerateRequest, onToken func(string)) (ollama.Result, error)
}
```
**Answer:**  
This follows the core Go idiom: **"Accept interfaces, return structs"** and **"Interfaces belong to the consumer, not the producer."**

- If `Generator` were defined in `package ollama`, `package bench` would be tightly coupled to Ollama's concrete implementation.
- By defining `Generator` in `package bench`, the benchmark states the exact minimal contract it requires.
- `*ollama.Client` satisfies this interface implicitly without needing an explicit `implements` declaration.
- Crucially, unit tests in `bench_test.go` can pass a lightweight in-memory fake generator without running an HTTP server or real language model.

---

### Q27: How does `median()` in `internal/bench/bench.go` compute the median without mutating the input slice?
```go
// From internal/bench/bench.go:
func median(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}
```
**Answer:**  
1. **Defensive Copying:** `sort.Float64s(vals)` sorts a slice in place. If `vals` were sorted directly, it would alter the chronological order of the benchmark runs in the caller's slice. `append([]float64(nil), vals...)` allocates a fresh copy `s`, leaving the original slice untouched.
2. **Odd Length:** If `len(s)` is odd (e.g., 3 runs, `mid = 1`), `s[mid]` is the exact middle element.
3. **Even Length:** If `len(s)` is even (e.g., 4 runs, `mid = 2`), it calculates the average of the two middle elements `(s[mid-1] + s[mid]) / 2`.

---

### Q28: How does the `Run()` loop in `internal/bench/bench.go` handle individual model failures without stopping the entire suite?
```go
// From internal/bench/bench.go:
for _, model := range cfg.Models {
	if err := ctx.Err(); err != nil {
		return rows, err
	}
	// ...
	warm, err := call(ctx, g, cfg, ...)
	if err != nil {
		if ctx.Err() != nil {
			return rows, ctx.Err() // Hard stop if user aborted
		}
		progress("[%s] failed during warm-up: %v", model, err)
		rows = append(rows, Row{Model: model, Prompt: "(warm-up)", Error: err.Error()})
		continue // Skip to next model, do not crash!
	}
	// ...
}
```
**Answer:**  
The benchmark distinguishes between **cancellation errors** and **model errors**:
1. If `ctx.Err() != nil` (user pressed `Ctrl+C`), the loop aborts immediately and returns whatever rows have completed so far.
2. If an individual model fails (e.g., out of memory or model tag missing), it logs the failure, records the error string inside that model's `Row` struct, and executes `continue`.
3. The remaining models in `cfg.Models` continue evaluating uninterrupted. This ensures a single broken model does not waste the benchmark results of other models.

---

### Q29: How does `internal/sysinfo/sysinfo.go` detect hardware specifications on Linux without third-party dependencies?
```go
// From internal/sysinfo/sysinfo.go:
func Detect() Info {
	info := Info{
		Threads: runtime.NumCPU(),
		OS:      runtime.GOOS + "/" + runtime.GOARCH,
	}
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		info.CPU = ParseCPUModel(string(b))
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		if kib := ParseMemTotalKiB(string(b)); kib > 0 {
			info.MemGiB = float64(kib) / 1024 / 1024
		}
	}
	if b, err := os.ReadFile("/etc/os-release"); err == nil {
		if name := ParseOSName(string(b)); name != "" {
			info.OS = name
		}
	}
	return info
}
```
**Answer:**  
Instead of importing heavy external CGO libraries or running shell commands:
1. `runtime.NumCPU()` reads total available logical CPU threads directly from the Go runtime scheduler.
2. `/proc/cpuinfo` is parsed to find the line starting with `"model name"`, extracting the hardware processor string (e.g., `AMD Ryzen 5 PRO 5650U`).
3. `/proc/meminfo` is parsed to read `"MemTotal:"` in KiB and converts it to GiB via `kib / 1024 / 1024`.
4. `/etc/os-release` is read to extract `PRETTY_NAME` (e.g., `Zorin OS 18.1`).
5. If any file cannot be read (such as on macOS or Windows), the fields remain zero or default to `runtime.GOOS/GOARCH` without panicking.

---

### Q30: In `cmd/offgrid/main.go`, how does `cmdBench()` handle graceful shutdown when a user presses `Ctrl+C`?
```go
// From cmd/offgrid/main.go:
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
defer stop()

// ...
rows, runErr := bench.Run(ctx, client, bench.Config{ ... })
```
**Answer:**  
1. `signal.NotifyContext` creates a child context that listens for OS termination signals (`os.Interrupt` / `SIGINT` via `Ctrl+C`, and `syscall.SIGTERM`).
2. When the user presses `Ctrl+C`, the runtime cancels `ctx`.
3. Any active HTTP client request using `req.WithContext(ctx)` immediately closes its underlying socket connection, unblocking the thread.
4. `bench.Run` intercepts `ctx.Err()`, terminates cleanly, and returns all rows measured up to that point.
5. `defer stop()` unregisters the signal handler, restoring default OS signal behavior and preventing goroutine resource leaks.
