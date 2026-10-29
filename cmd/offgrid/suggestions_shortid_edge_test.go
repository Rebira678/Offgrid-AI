package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
)

func runSuggestions(t *testing.T, db string, args ...string) (int, string, string) {
	t.Helper()
	var o, e strings.Builder
	code := run(append([]string{"suggestions", "-db", db}, args...), &o, &e)
	return code, o.String(), e.String()
}

// TestSuggestionsAcceptAndDismissWithShortTaskIDs tests that suggestions accept/dismiss
// formats task descriptions using ShortID, prevents accepting stale or resolved suggestions,
// and properly reports status.
func TestSuggestionsAcceptAndDismissWithShortTaskIDs(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "sug.db")

	st, err := sqlstore.Open(ctx, db, sqlstore.Options{NodeID: "laptop"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// 1. Create task T1.laptop
	t1, _, err := st.Create(ctx, task.NewTask{Title: "Fix Radiator", Priority: task.PriorityHigh, Site: "Engine"})
	if err != nil {
		t.Fatalf("Create T1: %v", err)
	}

	// Propose a suggestion to set priority low on T1.laptop
	priLow := task.PriorityLow
	sug1, _, err := st.Suggest(ctx, task.Draft{
		Task:     t1.ID,
		Priority: &priLow,
		By:       "qwen2.5:7b",
	})
	if err != nil {
		t.Fatalf("Suggest: %v", err)
	}

	// Accept suggestion via CLI
	code, out, errOut := runSuggestions(t, db, "accept", sug1.ID)
	if code != 0 {
		t.Fatalf("suggestions accept failed: %d, %s, %s", code, out, errOut)
	}
	// Must contain short ID "T1" and updated priority
	if !strings.Contains(out, "accepted "+sug1.ID) || !strings.Contains(out, "T1 [low, open]") {
		t.Fatalf("expected accepted output with short ID T1 [low, open], got:\n%s", out)
	}

	// Attempt to re-accept must fail with already resolved error
	code, _, errOut = runSuggestions(t, db, "accept", sug1.ID)
	if code == 0 || !strings.Contains(errOut, "already") {
		t.Fatalf("expected error on duplicate accept, got code %d, err: %s", code, errOut)
	}

	// 2. Create task T2.laptop and test dismiss
	t2, _, err := st.Create(ctx, task.NewTask{Title: "Check Coolant", Priority: task.PriorityMedium, Site: "Engine"})
	if err != nil {
		t.Fatalf("Create T2: %v", err)
	}
	sug2, _, err := st.Suggest(ctx, task.Draft{
		Task:     t2.ID,
		Priority: &priLow,
		By:       "qwen2.5:7b",
	})
	if err != nil {
		t.Fatalf("Suggest 2: %v", err)
	}

	code, out, errOut = runSuggestions(t, db, "dismiss", sug2.ID)
	if code != 0 {
		t.Fatalf("suggestions dismiss failed: %d, %s, %s", code, out, errOut)
	}
	if !strings.Contains(out, "dismissed "+sug2.ID) || !strings.Contains(out, "T2 is unchanged") {
		t.Fatalf("expected dismissed output with short ID T2 is unchanged, got:\n%s", out)
	}

	// 3. Test stale suggestion refusal
	t3, _, err := st.Create(ctx, task.NewTask{Title: "Inspect Pipes", Priority: task.PriorityHigh, Site: "Engine"})
	if err != nil {
		t.Fatalf("Create T3: %v", err)
	}
	sug3, _, err := st.Suggest(ctx, task.Draft{
		Task:     t3.ID,
		Priority: &priLow,
		By:       "qwen2.5:7b",
	})
	if err != nil {
		t.Fatalf("Suggest 3: %v", err)
	}

	// Human modifies T3 after AI proposed suggestion
	priMed := task.PriorityMedium
	st.Update(ctx, t3.ID, task.Changes{Priority: &priMed})
	st.Close()

	// Accepting now must be refused as stale
	code, out, errOut = runSuggestions(t, db, "accept", sug3.ID)
	if code == 0 || !strings.Contains(errOut, "refused") {
		t.Fatalf("expected stale refusal error, got code %d, out: %s, err: %s", code, out, errOut)
	}
}
