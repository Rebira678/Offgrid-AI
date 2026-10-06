package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"offgrid/internal/ollama"
)

// fakeGen records every request and answers from a function, so the
// benchmark logic can be tested with no HTTP and no model.
type fakeGen struct {
	reqs []ollama.GenerateRequest
	fn   func(req ollama.GenerateRequest) (ollama.Result, error)
}

func (f *fakeGen) Generate(_ context.Context, req ollama.GenerateRequest, _ func(string)) (ollama.Result, error) {
	f.reqs = append(f.reqs, req)
	return f.fn(req)
}

func result(genTokens int, eval time.Duration) ollama.Result {
	return ollama.Result{
		GenTokens:        genTokens,
		Eval:             eval,
		PromptTokens:     200,
		PromptEval:       2 * time.Second,
		TimeToFirstToken: 500 * time.Millisecond,
	}
}

func TestRunReportsMedianAndWarmsUp(t *testing.T) {
	// Generation takes 4s, 2s, then 5s on the three "short" runs:
	// 25, 50 and 20 tok/s. The median is 25, not the mean (31.7).
	evalTimes := []time.Duration{4 * time.Second, 2 * time.Second, 5 * time.Second}
	shortCalls := 0
	g := &fakeGen{fn: func(req ollama.GenerateRequest) (ollama.Result, error) {
		switch {
		case req.Prompt == "Say hi.":
			r := result(8, time.Second)
			r.Load = 2 * time.Second
			return r, nil
		case strings.Contains(req.Prompt, "short-text"):
			d := evalTimes[shortCalls]
			shortCalls++
			return result(100, d), nil
		default:
			return result(100, 4*time.Second), nil
		}
	}}

	rows, err := Run(context.Background(), g, Config{
		Models: []string{"m1"},
		Prompts: []Prompt{
			{Name: "short", Text: "short-text", MaxTokens: 50},
			{Name: "long", Text: "long-text", MaxTokens: 50},
		},
		Runs: 3,
		Salt: "s",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if want := 1 + 2*3; len(g.reqs) != want {
		t.Errorf("made %d requests, want %d (1 warm-up + 2 prompts x 3 runs)", len(g.reqs), want)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	short := rows[0]
	if short.GenTokPerSec != 25 {
		t.Errorf("short GenTokPerSec = %v, want median 25", short.GenTokPerSec)
	}
	if short.Runs != 3 || short.GenTokens != 100 || short.PromptTokens != 200 {
		t.Errorf("unexpected row %+v", short)
	}
	if short.LoadSeconds != 2 {
		t.Errorf("LoadSeconds = %v, want 2 (taken from the warm-up)", short.LoadSeconds)
	}

	// Every prompt must be unique so Ollama's prompt cache can never make
	// prompt processing look free.
	seen := map[string]bool{}
	for _, r := range g.reqs {
		if seen[r.Prompt] {
			t.Errorf("duplicate prompt sent twice: %.40q", r.Prompt)
		}
		seen[r.Prompt] = true
	}
	for _, r := range g.reqs {
		if r.Options.Temperature != 0 {
			t.Errorf("temperature = %v, want 0 for repeatable runs", r.Options.Temperature)
		}
	}
}

func TestRunContinuesAfterOneModelFails(t *testing.T) {
	g := &fakeGen{fn: func(req ollama.GenerateRequest) (ollama.Result, error) {
		if req.Model == "broken" {
			return ollama.Result{}, errors.New("model failed to load")
		}
		return result(100, 4*time.Second), nil
	}}
	rows, err := Run(context.Background(), g, Config{
		Models:  []string{"broken", "good"},
		Prompts: []Prompt{{Name: "short", Text: "x", MaxTokens: 10}},
		Runs:    1,
	})
	if err != nil {
		t.Fatalf("Run returned %v; a failing model should be recorded in its row, not abort the run", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0].Model != "broken" || rows[0].Error == "" {
		t.Errorf("first row should record the failure, got %+v", rows[0])
	}
	if rows[1].Model != "good" || rows[1].Error != "" || rows[1].GenTokPerSec != 25 {
		t.Errorf("second row should succeed, got %+v", rows[1])
	}
}

func TestRunStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	g := &fakeGen{fn: func(req ollama.GenerateRequest) (ollama.Result, error) {
		cancel() // the user presses Ctrl-C during the first request
		return ollama.Result{}, context.Canceled
	}}
	rows, err := Run(ctx, g, Config{
		Models:  []string{"a", "b"},
		Prompts: []Prompt{{Name: "short", Text: "x", MaxTokens: 10}},
		Runs:    3,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(g.reqs) != 1 {
		t.Errorf("made %d requests after cancellation, want exactly 1", len(g.reqs))
	}
	if len(rows) != 0 {
		t.Errorf("got %d rows, want none", len(rows))
	}
}

func TestRunValidatesConfig(t *testing.T) {
	g := &fakeGen{fn: func(ollama.GenerateRequest) (ollama.Result, error) { return ollama.Result{}, nil }}
	p := []Prompt{{Name: "x", Text: "x"}}
	cases := map[string]Config{
		"zero runs":  {Models: []string{"m"}, Prompts: p, Runs: 0},
		"no models":  {Prompts: p, Runs: 1},
		"no prompts": {Models: []string{"m"}, Runs: 1},
	}
	for name, cfg := range cases {
		if _, err := Run(context.Background(), g, cfg); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestMedian(t *testing.T) {
	cases := []struct {
		in   []float64
		want float64
	}{
		{nil, 0},
		{[]float64{7}, 7},
		{[]float64{3, 1, 2}, 2},
		{[]float64{4, 1, 3, 2}, 2.5},
	}
	for _, tc := range cases {
		orig := append([]float64(nil), tc.in...)
		if got := median(tc.in); got != tc.want {
			t.Errorf("median(%v) = %v, want %v", tc.in, got, tc.want)
		}
		for i := range orig {
			if tc.in[i] != orig[i] {
				t.Errorf("median modified its input: %v -> %v", orig, tc.in)
			}
		}
	}
}

func TestDefaultPrompts(t *testing.T) {
	if got := len(DefaultPrompts(true)); got != 1 {
		t.Errorf("quick mode has %d prompts, want 1", got)
	}
	all := DefaultPrompts(false)
	if len(all) != 2 || all[0].Name != "short" || all[1].Name != "long" {
		t.Fatalf("unexpected prompts: %+v", all)
	}
	if len(all[1].Text) < 10*len(all[0].Text) {
		t.Error("the long prompt should be much longer than the short one")
	}
	if all[1].Text != longPrompt() {
		t.Error("the long prompt must be deterministic")
	}
}

func TestReports(t *testing.T) {
	meta := Meta{Date: "2026-10-06", OllamaVersion: "0.5.7", Runs: 3}
	rows := []Row{
		{Model: "llama3.2:3b", Prompt: "short", Runs: 3, LoadSeconds: 2.5, TTFTSeconds: 0.8,
			GenTokPerSec: 12.34, PromptTokPerSec: 55.5, PromptTokens: 40, GenTokens: 96},
		{Model: "broken:1b", Prompt: "(warm-up)", Error: "boom"},
	}

	var md bytes.Buffer
	if err := WriteMarkdown(&md, meta, rows); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"| Model |", "llama3.2:3b", "12.3", "40 / 96", "failed", "boom", "**Hardware:** n/a"} {
		if !strings.Contains(md.String(), want) {
			t.Errorf("markdown is missing %q:\n%s", want, md.String())
		}
	}

	var js bytes.Buffer
	if err := WriteJSON(&js, meta, rows); err != nil {
		t.Fatal(err)
	}
	var back struct {
		Meta Meta  `json:"meta"`
		Rows []Row `json:"rows"`
	}
	if err := json.Unmarshal(js.Bytes(), &back); err != nil {
		t.Fatalf("JSON output does not parse: %v", err)
	}
	if len(back.Rows) != 2 || back.Rows[0].GenTokPerSec != 12.34 || back.Meta.OllamaVersion != "0.5.7" {
		t.Errorf("JSON round trip changed the data: %+v", back)
	}
}
