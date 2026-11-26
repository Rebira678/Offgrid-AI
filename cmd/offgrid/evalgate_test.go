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

	"offgrid/internal/intent"
)

func evalRun(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := runIn(append([]string{"eval"}, args...), strings.NewReader(""), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestEvalWritesABaselineAndTheGateHoldsThenFailsWithExitFour(t *testing.T) {
	good, _ := classifyServer(t, []string{"m:1b"}, oracleReply(t))
	dir := t.TempDir()
	base := filepath.Join(dir, "base.json")
	if code, _, e := evalRun(t, "-host", good.URL, "-models", "m:1b", "-write-baseline", base); code != 0 {
		t.Fatalf("exit %d\n%s", code, e)
	}
	if code, _, e := evalRun(t, "-host", good.URL, "-models", "m:1b", "-baseline", base); code != 0 || !strings.Contains(e, "holds:") {
		t.Fatalf("an unchanged model must hold: exit %d\n%s", code, e)
	}
	// A model that answers correctly for 3 cases in 4 and with an unknown intent for the rest.
	oracle := oracleReply(t)
	n := 0
	worse, _ := classifyServer(t, []string{"m:1b"}, func(text string) string {
		n++
		if n%4 == 0 {
			return cmdText(intent.Command{Intent: intent.Unknown})
		}
		return oracle(text)
	})
	code, _, e := evalRun(t, "-host", worse.URL, "-models", "m:1b", "-baseline", base)
	if code != 4 || !strings.Contains(e, "REGRESSION: schema=true attempts=3: intent correct fell from 105/105 (100.0%)") {
		t.Fatalf("exit %d\n%s", code, e)
	}
	// the same drop is tolerated under a margin wide enough
	if code, _, e := evalRun(t, "-host", worse.URL, "-models", "m:1b", "-baseline", base, "-margin", "0.5"); code != 0 {
		t.Fatalf("exit %d\n%s", code, e)
	}
}

func TestEvalGateInputsAreChecked(t *testing.T) {
	srv, _ := classifyServer(t, []string{"a", "b"}, oracleReply(t))
	if code, _, e := evalRun(t, "-host", srv.URL, "-models", "a,b", "-baseline", "x.json"); code != 2 || !strings.Contains(e, "exactly one model") {
		t.Fatalf("%d %s", code, e)
	}
	if code, _, _ := evalRun(t, "-host", srv.URL, "-models", "a", "-margin", "2"); code != 2 {
		t.Fatal("margin over 1")
	}
	if code, _, e := evalRun(t, "-host", srv.URL, "-models", "a", "-baseline", filepath.Join(t.TempDir(), "none.json")); code != 1 || !strings.Contains(e, "none.json") {
		t.Fatalf("%d %s", code, e)
	}
	// a baseline measured on a different case set is an error, never a pass
	bad := filepath.Join(t.TempDir(), "b.json")
	os.WriteFile(bad, []byte(`{"model":"a","prompt_version":"2","prompt_fingerprint":"x","cases_fingerprint":"other","setups":[{"use_schema":true,"max_attempts":3,"cases":105,"valid_first_try":105,"succeeded":105,"intent_correct":105,"args_checked":1,"args_correct":1}]}`), 0o644)
	if code, _, e := evalRun(t, "-host", srv.URL, "-models", "a", "-baseline", bad); code != 1 || !strings.Contains(e, "different set of cases") {
		t.Fatalf("%d %s", code, e)
	}
	if code, _, e := evalRun(t, "-host", "gpt@http://x", "-models", "a"); code != 2 || !strings.Contains(e, "unknown provider") {
		t.Fatalf("%d %s", code, e)
	}
}

func TestEvalRecordAppendsToTheHistory(t *testing.T) {
	srv, _ := classifyServer(t, []string{"m:1b"}, oracleReply(t))
	p := filepath.Join(t.TempDir(), "history.csv")
	for i := 0; i < 2; i++ {
		if code, _, e := evalRun(t, "-host", srv.URL, "-models", "m:1b", "-limit", "10", "-record", p); code != 0 {
			t.Fatal(e)
		}
	}
	b, _ := os.ReadFile(p)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "date,model,prompt_version") || !strings.Contains(lines[1], ",m:1b,"+intent.PromptVersion+",") {
		t.Fatalf("%q", lines)
	}
}

// A llama.cpp server on the LAN speaks the OpenAI protocol. The whole eval must run against one with
// no Ollama endpoints at all, and must send the schema as response_format.
func TestEvalAgainstALlamaCppStyleServer(t *testing.T) {
	oracle := oracleReply(t)
	var withSchema, total int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Messages       []struct{ Content string } `json:"messages"`
			ResponseFormat *struct{}                  `json:"response_format"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		total++
		if body.ResponseFormat != nil {
			withSchema++
		}
		reply, _ := json.Marshal(oracle(body.Messages[len(body.Messages)-1].Content))
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%s},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":5}}`, reply)
	}))
	defer srv.Close()
	code, out, e := evalRun(t, "-host", "llamacpp@"+srv.URL, "-models", "any.gguf", "-limit", "12")
	if code != 0 || !strings.Contains(out, "llamacpp server") || total < 12 || withSchema != total { // the warm-up call counts too
		t.Fatalf("exit %d calls %d with schema %d\n%s\n%s", code, total, withSchema, out, e)
	}
}

func TestTheProviderEnvironmentSettingAppliesUnlessHostIsGiven(t *testing.T) {
	t.Setenv("OFFGRID_PROVIDER", "llamacpp@http://box:8080")
	if got := providerSpec(defaultHost()); got != "llamacpp@http://box:8080" {
		t.Fatal(got)
	}
	if got := providerSpec("http://other:1"); got != "http://other:1" {
		t.Fatal("an explicit -host wins over the environment")
	}
}
