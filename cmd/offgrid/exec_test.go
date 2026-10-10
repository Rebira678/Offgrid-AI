package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const fixedNow = "2026-10-08T09:00"

func runExec(t *testing.T, stdin string, args ...string) (code int, out, errOut string) {
	t.Helper()
	var o, e strings.Builder
	code = runIn(append([]string{"exec", "-now", fixedNow}, args...), strings.NewReader(stdin), &o, &e)
	return code, o.String(), e.String()
}

func TestExecRunsACommandWithNoModel(t *testing.T) {
	code, out, _ := runExec(t, "", `{"intent":"create_task","title":"Leaking pump","site":"4","priority":"high"}`)
	if code != 0 {
		t.Fatalf("exit %d, output:\n%s", code, out)
	}
	for _, want := range []string{"ok: Created T1", "Leaking pump", "1 accepted, 0 refused"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestExecSeedGivesYesterdaysTasks(t *testing.T) {
	_, out, _ := runExec(t, "", "-seed", `{"intent":"list_tasks","when":"yesterday"}`)
	if !strings.Contains(out, "3 tasks") || !strings.Contains(out, "Leaking pump") {
		t.Errorf("seeded tasks were not created yesterday:\n%s", out)
	}
	_, out, _ = runExec(t, "", "-seed", `{"intent":"list_tasks","when":"today"}`)
	if !strings.Contains(out, "No matching tasks.") {
		t.Errorf("nothing should have been created today:\n%s", out)
	}
}

func TestExecScriptFromStdinKeepsStateBetweenLines(t *testing.T) {
	script := "# comment\n\n" +
		`{"intent":"create_task","title":"a"}` + "\n" +
		`{"intent":"update_task","task_ref":"a","status":"done"}` + "\n" +
		`{"intent":"list_tasks","status":"done"}` + "\n"
	code, out, _ := runExec(t, script, "-script", "-")
	if code != 0 || !strings.Contains(out, "1 task") || !strings.Contains(out, "3 accepted, 0 refused") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func TestExecRefusesBadCommandsAndSaysWhy(t *testing.T) {
	cases := map[string][]string{
		"empty title":     {`{"intent":"create_task","title":""}`, "refused", "non-empty title"},
		"bad priority":    {`{"intent":"create_task","title":"x","priority":"urgent"}`, "refused", "priority"},
		"unknown field":   {`{"intent":"create_task","title":"x","run":"rm -rf /"}`, "refused", "unknown field"},
		"not json":        {`hello`, "refused", "not a valid command"},
		"trailing text":   {`{"intent":"list_tasks"} extra`, "refused", "extra text"},
		"unknown intent":  {`{"intent":"unknown"}`, "declined", "not something Offgrid can do"},
		"missing task":    {`{"intent":"update_task","task_ref":"boiler","status":"done"}`, "no such task", "nothing was changed"},
		"invented intent": {`{"intent":"delete_everything"}`, "refused", "not allowed"},
	}
	for name, c := range cases {
		code, out, _ := runExec(t, "", c[0])
		if code != 0 {
			t.Errorf("%s: exit %d; refusals are a normal outcome unless -strict", name, code)
		}
		for _, want := range c[1:] {
			if !strings.Contains(out, want) {
				t.Errorf("%s: output lacks %q:\n%s", name, want, out)
			}
		}
		if !strings.Contains(out, "0 accepted, 1 refused") {
			t.Errorf("%s: wrong totals:\n%s", name, out)
		}
	}
}

func TestExecStrictExitsNonZeroOnRefusal(t *testing.T) {
	code, _, _ := runExec(t, "", "-strict", `{"intent":"create_task","title":""}`)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	code, _, _ = runExec(t, "", "-strict", `{"intent":"list_tasks"}`)
	if code != 0 {
		t.Errorf("exit = %d, want 0 when everything was accepted", code)
	}
}

func TestExecJSONOutput(t *testing.T) {
	_, out, _ := runExec(t, "", "-json", `{"intent":"create_task","title":"a"}`, `{"intent":"create_task","title":""}`)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("want one line per command, got:\n%s", out)
	}
	if !strings.Contains(lines[0], `"ok":true`) || !strings.Contains(lines[0], `"changed":true`) {
		t.Errorf("first line: %s", lines[0])
	}
	if !strings.Contains(lines[1], `"ok":false`) || !strings.Contains(lines[1], `"error"`) {
		t.Errorf("second line: %s", lines[1])
	}
}

func TestExecUsageErrors(t *testing.T) {
	var o, e strings.Builder
	if code := runIn([]string{"exec"}, strings.NewReader(""), &o, &e); code != 2 || !strings.Contains(e.String(), "no commands given") {
		t.Errorf("no commands: exit %d, stderr %q", code, e.String())
	}
	e.Reset()
	if code := runIn([]string{"exec", "-now", "yesterday-ish", `{}`}, strings.NewReader(""), &o, &e); code != 2 {
		t.Errorf("bad -now: exit %d", code)
	}
	e.Reset()
	if code := runIn([]string{"exec", "-script", filepath.Join(t.TempDir(), "missing.jsonl")}, strings.NewReader(""), &o, &e); code != 1 {
		t.Errorf("missing script: exit %d", code)
	}
}

func TestShippedDemoScriptRunsAndShowsEverySafetyBehaviour(t *testing.T) {
	path := filepath.Join("..", "..", "examples", "day4-commands.jsonl")
	if _, err := os.Stat(path); err != nil {
		t.Skip("examples/ not present")
	}
	code, out, _ := runExec(t, "", "-script", path)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{
		"ok: Created T1",
		"Already on the list, nothing added", // idempotent create
		"refused (ambiguous)",                // will not guess between two pumps
		"refused (no such task)",             // unknown reference
		"non-empty title",                    // empty title
		"not a valid site code",              // injection attempt
		"control character",                  // escape code
		"declined",                           // unknown intent
		"unknown field",                      // extra key
		"Summary for today",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("demo output lacks %q", want)
		}
	}
	// "pump" was ambiguous, so T1 must still be open after the refusal, and
	// only then finished by its ID.
	if strings.Index(out, "refused (ambiguous)") > strings.Index(out, "Updated T1 [high, done]") {
		t.Errorf("the ambiguous update must come before the ID-based one")
	}
}

func TestToolsCommandListsAllFiveAndNothingElse(t *testing.T) {
	var o, e strings.Builder
	if code := run([]string{"tools"}, &o, &e); code != 0 {
		t.Fatalf("exit %d: %s", code, e.String())
	}
	for _, want := range []string{"create_task", "update_task", "list_tasks", "add_note", "daily_summary", "task_ref", "required", "optional", "is refused and touches nothing"} {
		if !strings.Contains(o.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, o.String())
		}
	}
}

func TestVersionHasTheExpectedShapeAndUsageListsTheDay4Commands(t *testing.T) {
	var o, e strings.Builder
	run([]string{"version"}, &o, &e)
	// A pattern, not a number, so this test does not break every day.
	if !regexp.MustCompile(`^offgrid \d+\.\d+\.\d+-day\d+\n$`).MatchString(o.String()) {
		t.Errorf("version = %q", o.String())
	}
	o.Reset()
	run([]string{"help"}, &o, &e)
	if !strings.Contains(o.String(), "offgrid exec") || !strings.Contains(o.String(), "offgrid tools") {
		t.Errorf("usage does not list the new commands:\n%s", o.String())
	}
}
