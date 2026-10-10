package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"offgrid/internal/intent"
)

func intentCreate(title, site, priority string) intent.Command {
	return intent.Command{Intent: intent.CreateTask, Title: title, Site: site, Priority: priority}
}

// sscanRan reads the "N of M ran to a final outcome." line of the report.
func sscanRan(out string, ran, total *int) (int, error) {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasSuffix(line, "ran to a final outcome.") {
			return fmt.Sscanf(line, "%d of %d ran to a final outcome.", ran, total)
		}
	}
	return 0, fmt.Errorf("no \"ran to a final outcome\" line")
}

const nowArg = "2026-10-08T09:00"

func runAgent(t *testing.T, stdin string, args ...string) (code int, out, errOut string) {
	t.Helper()
	var o, e strings.Builder
	code = runIn(append([]string{"agent", "-now", nowArg}, args...), strings.NewReader(stdin), &o, &e)
	return code, o.String(), e.String()
}

func TestAgentRulesAnswerWithoutAnyModel(t *testing.T) {
	code, out, errOut := runAgent(t, "", "-no-model", "-seed", "what's still open?")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{"tier 0 (rules)", "rule list", "ok: 3 tasks", "Leaking pump", " µs]"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestAgentSendsWhatTheRulesCannotReadToTheModel(t *testing.T) {
	srv, bodies := classifyServer(t, []string{"llama3.2:3b"}, func(text string) string {
		return cmdText(intentCreate("Leaking pump", "4", "high"))
	})
	code, out, errOut := runAgent(t, "", "-host", srv.URL, "-model", "llama3.2:3b", "log a leaking pump at site 4, high priority")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{"tier 1 (model)", "1 attempt(s)", "ok: Created T1", "Leaking pump (site 4)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if n := len(drain(bodies)); n != 1 {
		t.Errorf("the model was called %d times, want 1", n)
	}
}

func TestAgentDoesNotCallTheModelForARuleMessage(t *testing.T) {
	srv, bodies := classifyServer(t, []string{"llama3.2:3b"}, func(string) string { return cmdText(intentCreate("x", "", "")) })
	code, out, _ := runAgent(t, "", "-host", srv.URL, "-model", "llama3.2:3b", "-seed", "list open tasks")
	if code != 0 || !strings.Contains(out, "tier 0 (rules)") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if n := len(drain(bodies)); n != 0 {
		t.Errorf("the model was called %d times for a message the rules read", n)
	}
}

func TestAgentRulesStillWorkWhenOllamaIsNotRunning(t *testing.T) {
	// Nothing listens on port 1.
	input := "new task: check the fuel line\nI finished replacing the gasket\nlist open tasks\n"
	code, out, _ := runAgent(t, input, "-host", "127.0.0.1:1")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if strings.Count(out, "tier 0 (rules)") < 3 { // two messages plus the table row
		t.Errorf("rule messages should have worked:\n%s", out)
	}
	for _, want := range []string{"ok: Created T1", "not reachable", "Ollama", "failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestAgentMultipleMessagesShareStateAndPrintATierTable(t *testing.T) {
	script := "# a comment\nnew task: pump seal\n\nmark pump seal as done\nsummary\n"
	code, out, _ := runAgent(t, script, "-no-model")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{"Created T1", "Updated T1 [medium, done]", "1 task finished", "TIER", "REQUESTS", "MEDIAN", "3 ok"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestAgentRefusesAmbiguityEvenFromARule(t *testing.T) {
	code, out, _ := runAgent(t, "new task: pump seal\nnew task: pump motor\nclose the pump ticket\n", "-no-model")
	if code != 0 || !strings.Contains(out, "refused") || !strings.Contains(out, "matches 2 tasks") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func TestAgentLogWritesOneJSONLinePerMessage(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "requests.jsonl")
	code, _, errOut := runAgent(t, "list open tasks\nnew task: x\nsomething only a model reads\n", "-no-model", "-log", logPath)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	f, err := os.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var entries []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e map[string]any
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		entries = append(entries, e)
	}
	if len(entries) != 3 {
		t.Fatalf("%d log lines, want 3", len(entries))
	}
	if entries[0]["tier"].(float64) != 0 || entries[0]["rule"] != "list" || entries[0]["status"] != "ok" {
		t.Errorf("first entry: %v", entries[0])
	}
	if entries[2]["tier"].(float64) != -1 || entries[2]["status"] != "failed" {
		t.Errorf("third entry: %v", entries[2])
	}
	for _, e := range entries {
		for _, k := range []string{"time", "text", "understand_ms", "execute_ms", "total_ms"} {
			if _, ok := e[k]; !ok {
				t.Errorf("entry lacks %q: %v", k, e)
			}
		}
	}
	// A second run appends; it does not overwrite.
	runAgent(t, "", "-no-model", "-log", logPath, "list tasks")
	data, _ := os.ReadFile(logPath)
	if n := strings.Count(string(data), "\n"); n != 4 {
		t.Errorf("%d lines after a second run, want 4", n)
	}
}

func TestAgentEvalNoModelShowsWhatTheRulesAloneCanDo(t *testing.T) {
	code, out, errOut := runAgent(t, "", "-no-model", "-eval")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{"c01", "c30", "0 rules", "Tier 0 (rules) handled", "of 30", "matched the label", "TIER"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "  NO  ") {
		t.Errorf("a rule disagreed with a label:\n%s", out)
	}
}

func TestAgentEvalWithAModelRunsAtLeast20CasesEndToEnd(t *testing.T) {
	srv, bodies := classifyServer(t, []string{"llama3.2:3b"}, oracleReply(t))
	code, out, errOut := runAgent(t, "", "-host", srv.URL, "-model", "llama3.2:3b", "-eval")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "Tier 1 (model) handled") || !strings.Contains(out, "tier 1 (model)") {
		t.Errorf("no model tier in the report:\n%s", out)
	}
	var ran, total int
	if _, err := sscanRan(out, &ran, &total); err != nil || total != 30 || ran < 20 {
		t.Errorf("ran %d of %d (%v):\n%s", ran, total, err, out)
	}
	if n := len(drain(bodies)); n == 0 || n >= 30 {
		t.Errorf("model calls = %d; the rules should have handled some of the 30 and the model the rest", n)
	}
}

func TestAgentEvalLimitAndUsageErrors(t *testing.T) {
	code, out, _ := runAgent(t, "", "-no-model", "-eval", "-limit", "5")
	if code != 0 || !strings.Contains(out, "c05") || strings.Contains(out, "c06") {
		t.Errorf("-limit 5: exit %d:\n%s", code, out)
	}
	if code, _, errOut := runAgent(t, "", "-no-model"); code != 2 || !strings.Contains(errOut, "no message given") {
		t.Errorf("no message: exit %d, %q", code, errOut)
	}
	if code, _, _ := runAgent(t, "", "-attempts", "0", "x"); code != 2 {
		t.Errorf("-attempts 0: exit %d", code)
	}
	var o, e strings.Builder
	if code := runIn([]string{"agent", "-now", "soon", "x"}, strings.NewReader(""), &o, &e); code != 2 {
		t.Errorf("bad -now: exit %d", code)
	}
}

func TestUsageListsTheAgentCommand(t *testing.T) {
	var o, e strings.Builder
	run([]string{"help"}, &o, &e)
	if !strings.Contains(o.String(), "offgrid agent") {
		t.Errorf("usage lacks the agent command:\n%s", o.String())
	}
}

func TestShippedDay5DemoFileShowsTheTiers(t *testing.T) {
	path := filepath.Join("..", "..", "examples", "day5-messages.txt")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skip("examples/ not present")
	}
	code, out, errOut := runAgent(t, string(data), "-seed", "-no-model")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{
		"rule list", "rule create-task", "rule add-note", "rule update-is-status", "rule summary",
		"matches 2 tasks",                 // refuses to guess
		"language model is not available", // needs the model
		"tier 0 (rules)", "none",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("demo output lacks %q", want)
		}
	}
}
