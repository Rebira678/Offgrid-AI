package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"offgrid/internal/ollama"
)

// fakeOllama imitates just enough of Ollama for an end-to-end CLI test.
func fakeOllama(t *testing.T, installed ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/version":
			fmt.Fprint(w, `{"version":"9.9.9-test"}`)
		case "/api/tags":
			var items []string
			for _, n := range installed {
				items = append(items, fmt.Sprintf(`{"name":%q,"size":1300000000,`+
					`"details":{"parameter_size":"1.2B","quantization_level":"Q8_0"}}`, n))
			}
			fmt.Fprintf(w, `{"models":[%s]}`, strings.Join(items, ","))
		case "/api/generate":
			fmt.Fprintln(w, `{"response":"ok","done":false}`)
			fmt.Fprintln(w, `{"response":"","done":true,"load_duration":1500000000,`+
				`"prompt_eval_count":40,"prompt_eval_duration":500000000,`+
				`"eval_count":80,"eval_duration":4000000000}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestBenchEndToEnd(t *testing.T) {
	srv := fakeOllama(t, "tiny:1b")
	dir := t.TempDir()
	mdPath := filepath.Join(dir, "bench.md")
	jsonPath := filepath.Join(dir, "bench.json")

	var out, errOut bytes.Buffer
	code := run([]string{"bench", "-host", srv.URL, "-models", "tiny:1b", "-runs", "2", "-quick",
		"-md", mdPath, "-json", jsonPath}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}

	// 80 tokens in 4s = 20 tok/s; 40 prompt tokens in 0.5s = 80 tok/s.
	for _, want := range []string{"tiny:1b", "short", "20.0", "80.0", "9.9.9-test", "1.5"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output is missing %q:\n%s", want, out.String())
		}
	}
	if md, err := os.ReadFile(mdPath); err != nil || !strings.Contains(string(md), "20.0") {
		t.Errorf("markdown file not written correctly: %v", err)
	}
	raw, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Rows []struct {
			GenTokPerSec float64 `json:"gen_tokens_per_sec"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || len(parsed.Rows) != 1 || parsed.Rows[0].GenTokPerSec != 20 {
		t.Errorf("json file wrong: %v %+v", err, parsed)
	}
}

func TestBenchRejectsMissingModel(t *testing.T) {
	srv := fakeOllama(t, "tiny:1b")
	var out, errOut bytes.Buffer
	code := run([]string{"bench", "-host", srv.URL, "-models", "tiny:1b,huge:70b"}, &out, &errOut)
	if code != 1 {
		t.Errorf("exit code %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "ollama pull huge:70b") {
		t.Errorf("stderr should tell the user how to install the model:\n%s", errOut.String())
	}
}

func TestModelsCommand(t *testing.T) {
	srv := fakeOllama(t, "tiny:1b")
	var out, errOut bytes.Buffer
	if code := run([]string{"models", "-host", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	for _, want := range []string{"tiny:1b", "1.2B", "Q8_0", "1.3 GB"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output is missing %q:\n%s", want, out.String())
		}
	}
}

func TestOllamaNotRunning(t *testing.T) {
	srv := fakeOllama(t)
	url := srv.URL
	srv.Close()

	var out, errOut bytes.Buffer
	if code := run([]string{"models", "-host", url}, &out, &errOut); code != 1 {
		t.Errorf("exit code %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "ollama serve") {
		t.Errorf("error should explain how to start Ollama:\n%s", errOut.String())
	}
}

func TestUsageAndUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(nil, &out, &errOut); code != 2 {
		t.Errorf("no args: exit %d, want 2", code)
	}
	if code := run([]string{"frobnicate"}, &out, &errOut); code != 2 {
		t.Errorf("unknown command: exit %d, want 2", code)
	}
	out.Reset()
	if code := run([]string{"version"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "offgrid") {
		t.Errorf("version: exit %d, output %q", code, out.String())
	}
}

func TestNormalizeHost(t *testing.T) {
	cases := map[string]string{
		"":                       "http://127.0.0.1:11434",
		"127.0.0.1:11434":        "http://127.0.0.1:11434",
		"http://localhost:11434": "http://localhost:11434",
		"http://box:11434/":      "http://box:11434",
		"  myhost:1234 ":         "http://myhost:1234",
	}
	for in, want := range cases {
		if got := normalizeHost(in); got != want {
			t.Errorf("normalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMissingModelsTreatsLatestAsDefault(t *testing.T) {
	installed := []ollama.Model{{Name: "llama3.2:latest"}, {Name: "llama3.2:3b"}}
	got := missingModels(installed, []string{"llama3.2", "llama3.2:3b", "llama3.2:1b"})
	if len(got) != 1 || got[0] != "llama3.2:1b" {
		t.Errorf("missing = %v, want [llama3.2:1b]", got)
	}
}
