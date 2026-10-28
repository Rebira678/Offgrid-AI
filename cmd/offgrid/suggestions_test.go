package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"offgrid/internal/agent"
	"offgrid/internal/intent"
	"offgrid/internal/structured"
	"offgrid/internal/task"
)

func runCmd(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var o, e bytes.Buffer
	code := run(args, &o, &e)
	return code, o.String(), e.String()
}

func aiSeededDB(t *testing.T) string {
	t.Helper()
	t.Setenv("OFFGRID_DB", "")
	db := filepath.Join(t.TempDir(), "tasks.db")
	if code, _, e := runCmd(t, "exec", "-seed", "-db", db, `{"intent":"list_tasks"}`); code != 0 {
		t.Fatalf("seed: %s", e)
	}
	return db
}

func taskLine(t *testing.T, db string) string {
	t.Helper()
	_, out, _ := runCmd(t, "exec", "-db", db, `{"intent":"list_tasks"}`)
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "T1 ") {
			return strings.TrimSpace(l)
		}
	}
	t.Fatalf("no T1 in:\n%s", out)
	return ""
}

func TestExecAsAIProposesInsteadOfChangingAndASuggestionCanBeAccepted(t *testing.T) {
	db := aiSeededDB(t)
	before := taskLine(t, db)
	code, out, e := runCmd(t, "exec", "-as-ai", "-by", "qwen2.5:7b", "-db", db, `{"intent":"update_task","task_ref":"T1","priority":"low","status":"in_progress"}`)
	if code != 0 || !strings.Contains(out, "suggested, NOT applied") || !strings.Contains(out, "1 accepted, 0 refused") {
		t.Fatalf("%d %s\n%s", code, e, out)
	}
	if got := taskLine(t, db); got != before {
		t.Fatalf("the task changed:\n%s\n%s", before, got)
	}
	code, out, _ = runCmd(t, "suggestions", "-db", db)
	if code != 0 || !strings.Contains(out, "set status to in_progress and set priority to low on T1") || !strings.Contains(out, "proposed by qwen2.5:7b") {
		t.Fatalf("%d\n%s", code, out)
	}
	id := strings.Fields(out)[0]
	if !strings.Contains(id, ".") {
		t.Fatalf("id %q", id)
	}
	code, out, e = runCmd(t, "suggestions", "-db", db, "accept", id)
	if code != 0 || !strings.Contains(out, "accepted "+id) {
		t.Fatalf("%d %s\n%s", code, e, out)
	}
	if got := taskLine(t, db); got == before || !strings.Contains(got, "low") {
		t.Fatalf("accept did not apply: %s", got)
	}
	if code, out, _ = runCmd(t, "suggestions", "-db", db); code != 0 || !strings.Contains(out, "no suggestions") {
		t.Fatalf("%s", out)
	}
	if code, out, _ = runCmd(t, "suggestions", "-db", db, "-all"); !strings.Contains(out, "[accepted]") {
		t.Fatalf("%s", out)
	}
	if code, _, e = runCmd(t, "suggestions", "-db", db, "accept", id); code != 1 || !strings.Contains(e, "already accepted or dismissed") {
		t.Fatalf("%d %s", code, e)
	}
	if code, _, _ = runCmd(t, "store", "-db", db, "verify"); code != 0 {
		t.Fatal("verify failed")
	}
}

func TestAnAIStillCreatesNewTasksDirectlyAndNeedsADatabase(t *testing.T) {
	db := aiSeededDB(t)
	code, out, _ := runCmd(t, "exec", "-as-ai", "-db", db, `{"intent":"create_task","title":"Check valve","site":"2","priority":"high"}`)
	if code != 0 || !strings.Contains(out, "ok: Created") {
		t.Fatalf("%s", out)
	}
	if code, _, e := runCmd(t, "exec", "-as-ai", `{"intent":"list_tasks"}`); code != 2 || !strings.Contains(e, "-db") {
		t.Fatalf("%d %s", code, e)
	}
	code, out, _ = runCmd(t, "exec", "-as-ai", "-json", "-db", db, `{"intent":"add_note","task_ref":"T2","note":"x"}`)
	var r struct {
		OK      bool
		Changed bool
		Error   string
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &r); err != nil || !r.OK || r.Changed || !strings.Contains(r.Error, "suggested, not applied") {
		t.Fatalf("%v %+v\n%s", err, r, out)
	}
}

func TestAStaleSuggestionCannotBeAcceptedButCanBeDismissed(t *testing.T) {
	db := aiSeededDB(t)
	runCmd(t, "exec", "-as-ai", "-db", db, `{"intent":"update_task","task_ref":"T1","priority":"low"}`)
	_, out, _ := runCmd(t, "suggestions", "-db", db)
	id := strings.Fields(out)[0]
	// a person edits the same field afterwards
	runCmd(t, "exec", "-db", db, `{"intent":"update_task","task_ref":"T1","priority":"medium"}`)
	_, out, _ = runCmd(t, "suggestions", "-db", db)
	if !strings.Contains(out, "STALE") {
		t.Fatalf("not marked stale:\n%s", out)
	}
	mid := taskLine(t, db)
	code, _, e := runCmd(t, "suggestions", "-db", db, "accept", id)
	if code != 1 || !strings.Contains(e, "refused") || !strings.Contains(e, "Nothing changed") || taskLine(t, db) != mid {
		t.Fatalf("%d %s", code, e)
	}
	if code, out, e = runCmd(t, "suggestions", "-db", db, "dismiss", id); code != 0 || !strings.Contains(out, "dismissed") {
		t.Fatalf("%d %s %s", code, out, e)
	}
	if taskLine(t, db) != mid {
		t.Fatal("dismiss changed the task")
	}
}

func TestSuggestionsCommandErrors(t *testing.T) {
	db := aiSeededDB(t)
	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"suggestions"}, 2},
		{[]string{"suggestions", "-db", "/nonexistent/x.db"}, 1},
		{[]string{"suggestions", "-db", db, "frobnicate"}, 2},
		{[]string{"suggestions", "-db", db, "accept"}, 2},
		{[]string{"suggestions", "-db", db, "list", "extra"}, 2},
		{[]string{"suggestions", "-db", db, "accept", "nope.9"}, 1},
		{[]string{"suggestions", "-db", db, "dismiss", "garbage"}, 1},
	} {
		if code, _, e := runCmd(t, tc.args...); code != tc.code || !strings.Contains(e, "error") {
			t.Errorf("%v: code %d (want %d) %q", tc.args, code, tc.code, e)
		}
	}
	code, out, _ := runCmd(t, "suggestions", "-db", db, "-json")
	var l []task.Suggestion
	if code != 0 || json.Unmarshal([]byte(out), &l) != nil || l == nil || len(l) != 0 {
		t.Fatalf("%d %q", code, out)
	}
}

type updateClassifier struct{}

func (updateClassifier) Classify(ctx context.Context, text string) (structured.Result[intent.Command], error) {
	return structured.Result[intent.Command]{Value: intent.Command{Intent: intent.UpdateTask, TaskRef: "T1", Priority: "low"}}, nil
}

func TestAQueuedMessageThatUpdatesATaskBecomesASuggestionWhenDrained(t *testing.T) {
	f := useFakeModels(t)
	_ = f
	newClassifier = func(host, model string, attempts int) agent.Model { return updateClassifier{} }
	db, queue := fill(t)
	before := taskLine(t, db)
	code, out, e := runDrain(t, "-db", db, "-queue", queue, "-net", "offline", "-once", "-poll", "10ms")
	if code != 0 {
		t.Fatalf("%d %s\n%s", code, e, out)
	}
	if !strings.Contains(out, "a change to an existing task becomes a suggestion") || !strings.Contains(out, "Suggested, not applied") {
		t.Fatalf("the output does not say so:\n%s", out)
	}
	if got := taskLine(t, db); got != before {
		t.Fatalf("the drain changed a task:\n%s\n%s", before, got)
	}
	_, list, _ := runCmd(t, "suggestions", "-db", db)
	if !strings.Contains(list, "set priority to low on T1") {
		t.Fatalf("%s", list)
	}
	// draining again proposes nothing more
	runDrain(t, "-db", db, "-queue", queue, "-net", "offline", "-once", "-poll", "10ms")
	if _, list2, _ := runCmd(t, "suggestions", "-db", db); strings.Count(list2, "set priority") != 1 {
		t.Fatalf("%s", list2)
	}
}
