package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"offgrid/internal/ollama"
)

func TestOllamaMapsRequestAndUsage(t *testing.T) {
	bodies := make(chan map[string]any, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		bodies <- body
		fmt.Fprintln(w, `{"message":{"content":"ok"},"done":false}`)
		fmt.Fprintln(w, `{"done":true,"prompt_eval_count":50,"prompt_eval_duration":1000000000,`+
			`"eval_count":100,"eval_duration":4000000000}`)
	}))
	defer srv.Close()
	c := NewOllama(ollama.NewClient(srv.URL))

	req := validRequest()
	req.MaxTokens = 50
	req.Temperature = 0.3
	resp, err := c.Chat(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}

	body := <-bodies
	opts, _ := body["options"].(map[string]any)
	if body["stream"] != true || body["model"] != "m" || body["keep_alive"] != "10m" {
		t.Errorf("unexpected request body: %v", body)
	}
	if opts["num_predict"] != float64(50) || opts["temperature"] != 0.3 {
		t.Errorf("options = %v, want num_predict 50 and temperature 0.3", opts)
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("sent %d messages, want 2", len(msgs))
	}
	if second, _ := msgs[1].(map[string]any); second["role"] != "user" || second["content"] != "hi" {
		t.Errorf("second message = %v", second)
	}

	if resp.Usage.PromptTokens != 50 || resp.Usage.CompletionTokens != 100 {
		t.Errorf("usage tokens = %+v", resp.Usage)
	}
	if got := resp.Usage.TokensPerSecond(); got != 25 {
		t.Errorf("TokensPerSecond = %v, want 25", got)
	}

	// "Zero means default": no limit becomes -1, and an unset seed becomes a
	// different non-zero number each time.
	req.MaxTokens = 0
	if _, err := c.Chat(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Chat(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	second, third := <-bodies, <-bodies
	o2, _ := second["options"].(map[string]any)
	o3, _ := third["options"].(map[string]any)
	if o2["num_predict"] != float64(-1) {
		t.Errorf("num_predict = %v, want -1 (unlimited) when MaxTokens is 0", o2["num_predict"])
	}
	if o2["seed"] == float64(0) || o2["seed"] == o3["seed"] {
		t.Errorf("seeds %v and %v should be random and non-zero when Seed is 0", o2["seed"], o3["seed"])
	}
}

func TestOllamaKeepsExplicitSeed(t *testing.T) {
	bodies := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		bodies <- body
		fmt.Fprintln(w, `{"message":{"content":"ok"},"done":false}`)
		fmt.Fprintln(w, `{"done":true}`)
	}))
	defer srv.Close()

	req := validRequest()
	req.Seed = 42
	if _, err := NewOllama(ollama.NewClient(srv.URL)).Chat(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	opts, _ := (<-bodies)["options"].(map[string]any)
	if opts["seed"] != float64(42) {
		t.Errorf("seed = %v, want 42", opts["seed"])
	}
}

func TestOllamaReportsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing is listening any more

	_, err := NewOllama(ollama.NewClient(url)).Chat(context.Background(), validRequest(), nil)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v, want ErrUnavailable", err)
	}
	if !errors.Is(err, ollama.ErrUnreachable) {
		t.Error("the original cause should stay in the error chain")
	}
}

func TestOllamaReportsMissingModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"model not found"}`)
	}))
	defer srv.Close()

	_, err := NewOllama(ollama.NewClient(srv.URL)).Chat(context.Background(), validRequest(), nil)
	var nf *ModelNotFoundError
	if !errors.As(err, &nf) || nf.Model != "m" {
		t.Fatalf("got %v, want a *ModelNotFoundError for model m", err)
	}
	if errors.Is(err, ErrUnavailable) {
		t.Error("a missing model is not an outage")
	}
}

func TestOllamaTimeoutIsNotAnOutage(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) // lets the server notice when the client hangs up
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := NewOllama(ollama.NewClient(srv.URL)).Chat(ctx, validRequest(), nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want context.DeadlineExceeded", err)
	}
	if errors.Is(err, ErrUnavailable) {
		t.Error("a slow model must not be reported as an unavailable backend")
	}
}

func TestOllamaInvalidRequestNeverCallsTheServer(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()

	_, err := NewOllama(ollama.NewClient(srv.URL)).Chat(context.Background(), Request{Model: "m"}, nil)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("got %v, want ErrInvalidRequest", err)
	}
	if hits.Load() != 0 {
		t.Errorf("server was contacted %d times, want 0", hits.Load())
	}
}
