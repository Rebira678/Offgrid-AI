package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// writeLines streams NDJSON the way Ollama does: one JSON object per line,
// flushed immediately.
func writeLines(w http.ResponseWriter, lines ...string) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	for _, l := range lines {
		fmt.Fprintln(w, l)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

const finalChunk = `{"model":"m","response":"","done":true,"done_reason":"stop",` +
	`"total_duration":5000000000,"load_duration":1000000000,` +
	`"prompt_eval_count":50,"prompt_eval_duration":1000000000,` +
	`"eval_count":100,"eval_duration":4000000000}`

func TestGenerateStreamsTextAndComputesRates(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		writeLines(w,
			`{"model":"m","response":"Hel","done":false}`,
			`{"model":"m","response":"lo","done":false}`,
			finalChunk,
		)
	}))
	defer srv.Close()

	var pieces []string
	res, err := NewClient(srv.URL).Generate(context.Background(), GenerateRequest{
		Model:   "m",
		Prompt:  "hi",
		Options: Options{NumPredict: 32, Temperature: 0, Seed: 0},
	}, func(s string) { pieces = append(pieces, s) })
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if res.Text != "Hello" {
		t.Errorf("Text = %q, want %q", res.Text, "Hello")
	}
	if len(pieces) != 2 {
		t.Errorf("onToken called %d times, want 2", len(pieces))
	}
	if got := res.GenTokensPerSec(); got != 25 {
		t.Errorf("GenTokensPerSec = %v, want 25 (100 tokens / 4s)", got)
	}
	if got := res.PromptTokensPerSec(); got != 50 {
		t.Errorf("PromptTokensPerSec = %v, want 50 (50 tokens / 1s)", got)
	}
	if res.Load != time.Second {
		t.Errorf("Load = %v, want 1s", res.Load)
	}
	if res.TimeToFirstToken <= 0 || res.TimeToFirstToken > res.Wall {
		t.Errorf("TTFT %v should be positive and no larger than Wall %v", res.TimeToFirstToken, res.Wall)
	}

	// The request must always stream, and zero-valued options must still be
	// sent (temperature 0 and seed 0 are meaningful).
	if gotBody["stream"] != true {
		t.Errorf("stream = %v, want true", gotBody["stream"])
	}
	opts, _ := gotBody["options"].(map[string]any)
	for _, key := range []string{"num_predict", "temperature", "seed"} {
		if _, ok := opts[key]; !ok {
			t.Errorf("options.%s was not sent", key)
		}
	}
	if opts["num_predict"] != float64(32) {
		t.Errorf("options.num_predict = %v, want 32", opts["num_predict"])
	}
}

func TestGenerateModelNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"model 'nope' not found"}`)
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL).Generate(context.Background(), GenerateRequest{Model: "nope"}, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v is not an *APIError", err)
	}
	if apiErr.Status != 404 || !strings.Contains(apiErr.Message, "not found") {
		t.Errorf("got %+v, want status 404 and a 'not found' message", apiErr)
	}
}

func TestGenerateErrorInsideStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeLines(w,
			`{"response":"par","done":false}`,
			`{"error":"model runner crashed"}`,
		)
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL).Generate(context.Background(), GenerateRequest{Model: "m"}, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !strings.Contains(apiErr.Message, "crashed") {
		t.Fatalf("got %v, want an APIError mentioning the crash", err)
	}
}

func TestGenerateTruncatedStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeLines(w, `{"response":"par","done":false}`) // connection ends, no "done"
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL).Generate(context.Background(), GenerateRequest{Model: "m"}, nil)
	if err == nil || !strings.Contains(err.Error(), "before the final") {
		t.Fatalf("got %v, want a truncated-stream error", err)
	}
}

func TestUnreachableServer(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens on this address any more

	_, err := NewClient(url).Version(context.Background())
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("got %v, want ErrUnreachable", err)
	}
}

func TestGenerateHonoursContextDeadline(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read the body first: Go's server only notices that the client
		// hung up once the request body has been consumed.
		io.Copy(io.Discard, r.Body)
		select { // a model that never answers
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release) // runs before srv.Close, so Close can never block on this handler

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := NewClient(srv.URL).Generate(ctx, GenerateRequest{Model: "m"}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want context.DeadlineExceeded", err)
	}
	if errors.Is(err, ErrUnreachable) {
		t.Error("a timeout must not be reported as 'unreachable'")
	}
}

func TestListModelsAndVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			fmt.Fprint(w, `{"models":[{"name":"llama3.2:3b","size":2019393189,`+
				`"details":{"parameter_size":"3.2B","quantization_level":"Q4_K_M"}}]}`)
		case "/api/version":
			fmt.Fprint(w, `{"version":"0.5.7"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL + "/") // trailing slash must be tolerated

	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	want := Model{Name: "llama3.2:3b", SizeBytes: 2019393189, ParameterSize: "3.2B", Quantization: "Q4_K_M"}
	if len(models) != 1 || models[0] != want {
		t.Errorf("models = %+v, want [%+v]", models, want)
	}
	if v, err := c.Version(context.Background()); err != nil || v != "0.5.7" {
		t.Errorf("Version = %q, %v; want 0.5.7", v, err)
	}
}

func TestRateWithMissingNumbers(t *testing.T) {
	cases := []struct {
		name string
		r    Result
	}{
		{"no tokens", Result{GenTokens: 0, Eval: time.Second}},
		{"no duration", Result{GenTokens: 10, Eval: 0}},
		{"negative duration", Result{GenTokens: 10, Eval: -time.Second}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.GenTokensPerSec(); got != 0 {
				t.Errorf("got %v, want 0 (no division by zero)", got)
			}
		})
	}
}
