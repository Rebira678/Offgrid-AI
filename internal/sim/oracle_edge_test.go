package sim

import (
	"context"
	"strings"
	"testing"
	"time"

	"offgrid/internal/task"
)

// TestOracleDetectsTamperedTaskDeletion verifies that if SQLite projection deletes
// a task that the raw op log indicates is alive, the oracle flags a delete-rule failure.
func TestOracleDetectsTamperedTaskDeletion(t *testing.T) {
	ctx := context.Background()
	clk := &tclock{epoch}
	s := rep(t, "node-oracle-tamper", clk)

	tk, _, err := s.Create(ctx, task.NewTask{Title: "critical pump", Priority: task.PriorityHigh})
	if err != nil {
		t.Fatal(err)
	}

	opsBeforeDelete := opsOf(t, s)
	// Normal check passes
	if f := checkAgainstTheLog(ctx, s, opsBeforeDelete); f != nil {
		t.Fatalf("expected check to pass, got: %v", f)
	}

	// Store deletes the task
	if err := s.DeleteTask(ctx, tk.ID); err != nil {
		t.Fatal(err)
	}

	// Oracle checking against opsBeforeDelete sees that the task should be alive according to the log,
	// but the store view has deleted it. It must flag a delete-rule failure.
	f := checkAgainstTheLog(ctx, s, opsBeforeDelete)
	if f == nil || f.Class != "delete-rule" || !strings.Contains(f.Msg, "deleted") {
		t.Fatalf("expected delete-rule failure on missing task, got %+v", f)
	}
}

// TestOracleSuggestionAcceptanceCountsAsEdit tests that an accepted suggestion
// acts as a causal mark preventing earlier deletes from killing the task.
func TestOracleSuggestionAcceptanceCountsAsEdit(t *testing.T) {
	ctx := context.Background()
	clk := &tclock{epoch}
	a, b := rep(t, "node-a", clk), rep(t, "node-b", clk)

	tk, _, _ := a.Create(ctx, task.NewTask{Title: "substation breaker", Priority: task.PriorityLow})
	pullAll(t, b, a)

	// A proposes a suggestion
	hi := task.PriorityHigh
	sug, _, err := a.Suggest(ctx, task.Draft{Task: tk.ID, Priority: &hi, By: "model"})
	if err != nil {
		t.Fatal(err)
	}
	pullAll(t, b, a)

	// B accepts the suggestion
	clk.t = clk.t.Add(time.Minute)
	if _, err := b.Resolve(ctx, sug.ID, true); err != nil {
		t.Fatal(err)
	}

	// Meanwhile A deletes the task concurrently without having seen the accept
	clk.t = clk.t.Add(2 * time.Minute)
	if err := a.DeleteTask(ctx, tk.ID); err != nil {
		t.Fatal(err)
	}

	pullAll(t, a, b)
	pullAll(t, b, a)

	// Both replicas must keep the task alive, and oracle checkAgainstTheLog must confirm
	ops := opsOf(t, a)
	if f := checkAgainstTheLog(ctx, a, ops); f != nil {
		t.Fatalf("oracle check failed on accepted suggestion edit: %v", f)
	}
}
