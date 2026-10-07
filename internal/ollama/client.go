// Package ollama is a small, dependency-free client for the Ollama HTTP API.
//
// It only implements what Offgrid needs: listing installed models, reading the
// server version, and streaming text generation with the timing numbers that
// Ollama reports (these are what the benchmark is built on).
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultBaseURL is where `ollama serve` listens by default.
const DefaultBaseURL = "http://127.0.0.1:11434"

// ErrUnreachable means we could not open a connection to the Ollama server at
// all (not running, wrong port, firewall). Callers use errors.Is to detect it.
var ErrUnreachable = errors.New("cannot reach Ollama")

// APIError is an error that the Ollama server itself reported, for example a
// 404 "model not found". Callers use errors.As to inspect it.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("ollama: HTTP %d: %s", e.Status, e.Message)
}

// Client talks to one Ollama server.
//
// Note that HTTP deliberately has NO overall Timeout. http.Client.Timeout
// covers the whole exchange including reading the body, so it would kill a
// long streaming answer half way through. Time limits are applied per request
// through context.Context instead.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// NewClient returns a client for baseURL. An empty baseURL means DefaultBaseURL.
func NewClient(baseURL string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{},
	}
}

// Options are the sampling settings sent with a generate request. All fields
// are always sent (no omitempty) because Temperature 0 and Seed 0 are
// meaningful values, and omitting them would silently change behaviour.
type Options struct {
	NumPredict  int     `json:"num_predict"` // maximum tokens to generate
	Temperature float64 `json:"temperature"` // 0 = always pick the likeliest token
	Seed        int     `json:"seed"`        // fixed seed for repeatable output
}

// GenerateRequest is the body of POST /api/generate.
type GenerateRequest struct {
	Model     string  `json:"model"`
	Prompt    string  `json:"prompt"`
	Stream    bool    `json:"stream"`
	KeepAlive string  `json:"keep_alive,omitempty"` // how long to keep the model in RAM, e.g. "10m"
	Options   Options `json:"options"`
}

// streamChunk is one line of a streamed NDJSON response. Every line has a
// piece of text; the last line has done=true and all the timing counters.
// Durations are in nanoseconds.
//
// /api/generate puts the text in "response" and /api/chat puts it in
// "message.content". Only one of the two is ever filled in, so one struct can
// decode both and the streaming loop below is shared.
type streamChunk struct {
	Response string `json:"response"`
	Message  struct {
		Content string `json:"content"`
	} `json:"message"`
	Done               bool   `json:"done"`
	DoneReason         string `json:"done_reason"`
	Error              string `json:"error"`
	TotalDuration      int64  `json:"total_duration"`
	LoadDuration       int64  `json:"load_duration"`
	PromptEvalCount    int    `json:"prompt_eval_count"`
	PromptEvalDuration int64  `json:"prompt_eval_duration"`
	EvalCount          int    `json:"eval_count"`
	EvalDuration       int64  `json:"eval_duration"`
}

// Result is a finished generation plus its measurements.
type Result struct {
	Model string
	Text  string

	// Measured by us, on the client side.
	TimeToFirstToken time.Duration // request sent -> first piece of text arrived
	Wall             time.Duration // request sent -> final message arrived

	// Reported by the Ollama server.
	Total      time.Duration // everything the server did for this request
	Load       time.Duration // loading the model into RAM (large on a cold start)
	PromptEval time.Duration // reading the prompt ("prefill")
	Eval       time.Duration // producing the answer ("decode")

	PromptTokens int // tokens in the prompt that were actually processed
	GenTokens    int // tokens generated
}

// GenTokensPerSec is the generation (decode) speed: the number people quote
// as "tokens per second". Returns 0 if the server gave no usable numbers.
func (r Result) GenTokensPerSec() float64 { return rate(r.GenTokens, r.Eval) }

// PromptTokensPerSec is the prompt-processing (prefill) speed.
func (r Result) PromptTokensPerSec() float64 { return rate(r.PromptTokens, r.PromptEval) }

func rate(tokens int, d time.Duration) float64 {
	if tokens <= 0 || d <= 0 {
		return 0
	}
	return float64(tokens) / d.Seconds()
}

// Generate streams a completion. onToken (optional) is called with each piece
// of text as it arrives. The request is always streamed, because streaming is
// the only way to measure time to first token.
func (c *Client) Generate(ctx context.Context, req GenerateRequest, onToken func(string)) (Result, error) {
	req.Stream = true
	return c.stream(ctx, "/api/generate", req, req.Model, onToken)
}

// stream POSTs body to path and reads the NDJSON reply until the final
// "done" line. Generate and Chat both use it, so the timing logic and the
// error handling exist exactly once.
func (c *Client) stream(ctx context.Context, path string, body any, model string, onToken func(string)) (Result, error) {
	start := time.Now()

	resp, err := c.do(ctx, http.MethodPost, path, body)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()

	res := Result{Model: model}
	var text strings.Builder
	sawFirst := false

	// json.Decoder reads one JSON value at a time and has no line-length
	// limit, unlike bufio.Scanner (64 KB default), so it is the safer choice
	// for NDJSON.
	dec := json.NewDecoder(resp.Body)
	for {
		var ch streamChunk
		if err := dec.Decode(&ch); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return Result{}, ctxErr
			}
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return Result{}, errors.New("ollama: stream ended before the final 'done' message")
			}
			return Result{}, fmt.Errorf("ollama: decode stream: %w", err)
		}
		if ch.Error != "" {
			return Result{}, &APIError{Status: resp.StatusCode, Message: ch.Error}
		}
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
		if ch.Done {
			res.Text = text.String()
			res.Wall = time.Since(start)
			res.Total = time.Duration(ch.TotalDuration)
			res.Load = time.Duration(ch.LoadDuration)
			res.PromptEval = time.Duration(ch.PromptEvalDuration)
			res.Eval = time.Duration(ch.EvalDuration)
			res.PromptTokens = ch.PromptEvalCount
			res.GenTokens = ch.EvalCount
			return res, nil
		}
	}
}

// Model describes one locally installed model.
type Model struct {
	Name          string
	SizeBytes     int64
	ParameterSize string // e.g. "3.2B"
	Quantization  string // e.g. "Q4_K_M"
}

// ListModels returns the models installed on the server (GET /api/tags).
func (c *Client) ListModels(ctx context.Context) ([]Model, error) {
	resp, err := c.do(ctx, http.MethodGet, "/api/tags", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var body struct {
		Models []struct {
			Name    string `json:"name"`
			Size    int64  `json:"size"`
			Details struct {
				ParameterSize     string `json:"parameter_size"`
				QuantizationLevel string `json:"quantization_level"`
			} `json:"details"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("ollama: decode model list: %w", err)
	}
	out := make([]Model, 0, len(body.Models))
	for _, m := range body.Models {
		out = append(out, Model{
			Name:          m.Name,
			SizeBytes:     m.Size,
			ParameterSize: m.Details.ParameterSize,
			Quantization:  m.Details.QuantizationLevel,
		})
	}
	return out, nil
}

// Version returns the Ollama server version (GET /api/version).
func (c *Client) Version(ctx context.Context) (string, error) {
	resp, err := c.do(ctx, http.MethodGet, "/api/version", nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var body struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("ollama: decode version: %w", err)
	}
	return body.Version, nil
}

// do sends one request and turns every failure into a meaningful error:
//   - context cancelled or timed out -> the context error itself
//   - connection failed              -> ErrUnreachable
//   - HTTP status >= 400             -> *APIError
//
// On success the caller owns resp.Body and must close it.
func (c *Client) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("ollama: encode request: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return nil, fmt.Errorf("ollama: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

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
	return resp, nil
}

func readAPIError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	msg := strings.TrimSpace(string(raw))
	var parsed struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &parsed) == nil && parsed.Error != "" {
		msg = parsed.Error
	}
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	return &APIError{Status: resp.StatusCode, Message: msg}
}
