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
	"sync/atomic"
	"testing"

	"offgrid/internal/eval"
	"offgrid/internal/intent"
)

// classifyServer imitates Ollama for the parse and eval commands. Only the
// listed models exist. respond receives the worker's message (the last message
// of the conversation) and returns the model's reply text. Every /api/chat
// request body is delivered on the returned channel.
func classifyServer(t *testing.T, models []string, respond func(lastMessage string) string) (*httptest.Server, <-chan map[string]any) {
	t.Helper()
	bodies := make(chan map[string]any, 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/version":
			fmt.Fprint(w, `{"version":"9.9.9-test"}`)
		case "/api/tags":
			var items []string
			for _, m := range models {
				items = append(items, fmt.Sprintf(`{"name":%q,"size":1,"details":{}}`, m))
			}
			fmt.Fprintf(w, `{"models":[%s]}`, strings.Join(items, ","))
		case "/api/chat":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			select {
			case bodies <- body:
			default:
			}
			known := false
			for _, m := range models {
				known = known || m == body["model"]
			}
			if !known {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"error":"model not found"}`)
				return
			}
			msgs, _ := body["messages"].([]any)
			last, _ := msgs[len(msgs)-1].(map[string]any)
			text, _ := last["content"].(string)
			line, _ := json.Marshal(map[string]any{
				"message": map[string]any{"role": "assistant", "content": respond(text)},
				"done":    false,
			})
			fmt.Fprintln(w, string(line))
			fmt.Fprintln(w, `{"done":true,"eval_count":10,"eval_duration":1000000000}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, bodies
}

func cmdText(c intent.Command) string {
	b, _ := json.Marshal(c)
	return string(b)
}

var listOpen = cmdText(intent.Command{Intent: intent.ListTasks, Status: "open"})

// oracleReply answers every evaluation case correctly.
func oracleReply(t *testing.T) func(string) string {
	t.Helper()
	cases, err := eval.LoadCases()
	if err != nil {
		t.Fatal(err)
	}
	byText := map[string]eval.Case{}
	for _, c := range cases {
		byText[c.Text] = c
	}
	return func(text string) string {
		c, ok := byText[text]
		if !ok {
			return cmdText(intent.Command{Intent: intent.Unknown})
		}
		return cmdText(intent.Command{Intent: c.Intent, Title: "x", TaskRef: "x", Note: "x",
			Site: c.Site, Priority: c.Priority, Status: c.Status, When: c.When})
	}
}

func drain(ch <-chan map[string]any) []map[string]any {
	var out []map[string]any
	for {
		select {
		case b := <-ch:
			out = append(out, b)
		default:
			return out
		}
	}
}

// ---- offgrid parse ---------------------------------------------------------

func TestParsePrintsAValidatedCommand(t *testing.T) {
	srv, bodies := classifyServer(t, []string{"tiny:1b"}, func(string) string { return listOpen })
	var out, errOut bytes.Buffer

	code := runIn([]string{"parse", "-host", srv.URL, "-model", "tiny:1b", "what's", "still", "open?"},
		strings.NewReader(""), &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, errOut.String())
	}

	var got intent.Command
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not a JSON command: %v\n%s", err, out.String())
	}
	if got.Intent != intent.ListTasks || got.Status != "open" {
		t.Errorf("command = %+v", got)
	}
	if !strings.Contains(errOut.String(), "1 attempt(s)") {
		t.Errorf("stderr should report the attempts: %q", errOut.String())
	}

	body := (drain(bodies))[0]
	format, _ := body["format"].(map[string]any)
	if format["type"] != "object" {
		t.Errorf("format = %v, want the command schema to be sent by default", body["format"])
	}
	opts, _ := body["options"].(map[string]any)
	if opts["temperature"] != float64(0) {
		t.Errorf("temperature = %v, want 0 for a decision", opts["temperature"])
	}
}

func TestParseCanTurnTheSchemaOff(t *testing.T) {
	srv, bodies := classifyServer(t, []string{"tiny:1b"}, func(string) string { return listOpen })
	var out, errOut bytes.Buffer
	code := runIn([]string{"parse", "-host", srv.URL, "-model", "tiny:1b", "-schema=false", "x"},
		strings.NewReader(""), &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if body := drain(bodies)[0]; body["format"] != nil {
		t.Errorf("format = %v, want none with -schema=false", body["format"])
	}
}

func TestParseRetriesAndShowsWhy(t *testing.T) {
	var calls int32
	srv, _ := classifyServer(t, []string{"tiny:1b"}, func(string) string {
		if atomic.AddInt32(&calls, 1) == 1 {
			return cmdText(intent.Command{Intent: intent.CreateTask}) // no title: rejected
		}
		return cmdText(intent.Command{Intent: intent.CreateTask, Title: "Leaking pump"})
	})
	var out, errOut bytes.Buffer
	code := runIn([]string{"parse", "-host", srv.URL, "-model", "tiny:1b", "-v", "log a pump"},
		strings.NewReader(""), &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Leaking pump") || !strings.Contains(errOut.String(), "2 attempt(s)") {
		t.Errorf("stdout %q, stderr %q", out.String(), errOut.String())
	}
	for _, want := range []string{"attempt 1", "rejected: create_task needs a non-empty title", "attempt 2", "accepted"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("-v output is missing %q:\n%s", want, errOut.String())
		}
	}
}

func TestParseGivesUpClearly(t *testing.T) {
	srv, _ := classifyServer(t, []string{"tiny:1b"}, func(string) string { return "I am not sure" })
	var out, errOut bytes.Buffer
	code := runIn([]string{"parse", "-host", srv.URL, "-model", "tiny:1b", "-attempts", "2", "hello"},
		strings.NewReader(""), &out, &errOut)
	if code != 1 {
		t.Errorf("exit code %d, want 1", code)
	}
	if out.Len() != 0 {
		t.Errorf("stdout should be empty on failure, got %q", out.String())
	}
	for _, want := range []string{"attempt 1", "attempt 2", "I am not sure", "no valid command in 2 attempt(s)"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr is missing %q:\n%s", want, errOut.String())
		}
	}
}

func TestParseReadsStdinAndChecksInput(t *testing.T) {
	var seen atomic.Value
	srv, _ := classifyServer(t, []string{"tiny:1b"}, func(text string) string { seen.Store(text); return listOpen })
	var out, errOut bytes.Buffer

	if code := runIn([]string{"parse", "-host", srv.URL, "-model", "tiny:1b"},
		strings.NewReader("show open tasks\n"), &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if seen.Load() != "show open tasks" {
		t.Errorf("model saw %q, want the piped text", seen.Load())
	}

	errOut.Reset()
	if code := runIn([]string{"parse"}, strings.NewReader("  "), &out, &errOut); code != 2 {
		t.Errorf("no message: exit %d, want 2", code)
	}
	if code := runIn([]string{"parse", "-attempts", "0", "x"}, strings.NewReader(""), &out, &errOut); code != 2 {
		t.Errorf("-attempts 0: exit %d, want 2", code)
	}
}

func TestParseModelNotInstalled(t *testing.T) {
	srv, _ := classifyServer(t, []string{"tiny:1b"}, func(string) string { return listOpen })
	var out, errOut bytes.Buffer
	code := runIn([]string{"parse", "-host", srv.URL, "-model", "nope:1b", "hi"}, strings.NewReader(""), &out, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "ollama pull nope:1b") {
		t.Errorf("exit %d, stderr %q", code, errOut.String())
	}
}

// ---- offgrid eval ----------------------------------------------------------

func TestEvalEndToEnd(t *testing.T) {
	srv, _ := classifyServer(t, []string{"tiny:1b"}, oracleReply(t))
	dir := t.TempDir()
	mdPath, jsonPath := filepath.Join(dir, "eval.md"), filepath.Join(dir, "eval.json")
	var out, errOut bytes.Buffer

	code := runIn([]string{"eval", "-host", srv.URL, "-models", "tiny:1b", "-md", mdPath, "-json", jsonPath},
		strings.NewReader(""), &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, errOut.String())
	}
	for _, want := range []string{"| tiny:1b | + schema + retries (up to 3) | 30/30 = 100%", "9.9.9-test", "cases per run:** 30"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report is missing %q:\n%s", want, out.String())
		}
	}
	if md, err := os.ReadFile(mdPath); err != nil || !strings.Contains(string(md), "30/30") {
		t.Errorf("markdown file: %v", err)
	}
	raw, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Runs []struct {
			Cases   int        `json:"cases"`
			Results []struct{} `json:"results"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || len(parsed.Runs) != 1 || parsed.Runs[0].Cases != 30 || len(parsed.Runs[0].Results) != 30 {
		t.Errorf("json file wrong: %v %+v", err, parsed)
	}
}

func TestEvalAblationMeasuresThreeSetups(t *testing.T) {
	srv, bodies := classifyServer(t, []string{"tiny:1b"}, oracleReply(t))
	var out, errOut bytes.Buffer
	code := runIn([]string{"eval", "-host", srv.URL, "-models", "tiny:1b", "-ablation", "-limit", "5"},
		strings.NewReader(""), &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, errOut.String())
	}
	for _, want := range []string{"| tiny:1b | prompt only |", "| tiny:1b | + schema |", "| tiny:1b | + schema + retries (up to 3) |"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report is missing %q:\n%s", want, out.String())
		}
	}

	// 1 warm-up + 5 cases, three times over = 16 requests; the first setup
	// (prompt only) must have sent no schema and the others must have.
	var withSchema, without int
	for _, b := range drain(bodies) {
		if b["format"] != nil {
			withSchema++
		} else {
			without++
		}
	}
	if without != 6 || withSchema != 10 {
		t.Errorf("%d requests without a schema and %d with, want 6 and 10", without, withSchema)
	}
}

func TestEvalRefusesMissingModelsBeforeStarting(t *testing.T) {
	srv, bodies := classifyServer(t, []string{"tiny:1b"}, oracleReply(t))
	var out, errOut bytes.Buffer
	code := runIn([]string{"eval", "-host", srv.URL, "-models", "tiny:1b,nope:1b"}, strings.NewReader(""), &out, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "ollama pull nope:1b") {
		t.Errorf("exit %d, stderr %q", code, errOut.String())
	}
	if n := len(drain(bodies)); n != 0 {
		t.Errorf("%d model calls were made before the check, want 0", n)
	}
}

func TestEvalInputChecks(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runIn([]string{"eval", "-attempts", "0"}, strings.NewReader(""), &out, &errOut); code != 2 {
		t.Errorf("-attempts 0: exit %d, want 2", code)
	}
	if code := runIn([]string{"eval", "-models", " , "}, strings.NewReader(""), &out, &errOut); code != 2 {
		t.Errorf("empty -models: exit %d, want 2", code)
	}
}

func TestEvalWhenOllamaIsDown(t *testing.T) {
	srv, _ := classifyServer(t, []string{"tiny:1b"}, oracleReply(t))
	url := srv.URL
	srv.Close()
	var out, errOut bytes.Buffer
	code := runIn([]string{"eval", "-host", url, "-models", "tiny:1b"}, strings.NewReader(""), &out, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "ollama serve") {
		t.Errorf("exit %d, stderr %q", code, errOut.String())
	}
}

func TestEvalSetups(t *testing.T) {
	cases := []struct {
		schema   bool
		attempts int
		ablation bool
		want     []setup
	}{
		{true, 3, false, []setup{{true, 3}}},
		{false, 2, false, []setup{{false, 2}}},
		{true, 3, true, []setup{{false, 1}, {true, 1}, {true, 3}}},
		{true, 1, true, []setup{{false, 1}, {true, 1}}}, // no duplicate when retries are off
	}
	for _, tc := range cases {
		got := evalSetups(tc.schema, tc.attempts, tc.ablation)
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("evalSetups(%v, %d, %v) = %v, want %v", tc.schema, tc.attempts, tc.ablation, got, tc.want)
		}
	}
}
