package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
)

// TestTaskListDeleteAndNoteWithShortIDsAndDisambiguation tests the CLI behavior
// with short IDs, disambiguation when duplicates occur, and fallback to qualified IDs.
func TestTaskListDeleteAndNoteWithShortIDsAndDisambiguation(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "shortid.db")

	// Open store with node "laptop"
	st, err := sqlstore.Open(ctx, db, sqlstore.Options{NodeID: "laptop"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Create task on laptop -> T1.laptop
	t1, _, err := st.Create(ctx, task.NewTask{Title: "Fix Laptop Screen", Priority: task.PriorityHigh, Site: "A"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if t1.ID != "T1.laptop" {
		t.Fatalf("expected T1.laptop, got %s", t1.ID)
	}
	st.AddNote(ctx, t1.ID, "Screen flicker on cold boot")

	// 1. When only T1.laptop exists, list should show short ID "T1"
	code, out, _ := runTask(t, db, "list")
	if code != 0 {
		t.Fatalf("list failed: %d, out: %s", code, out)
	}
	if !strings.Contains(out, "T1 ") || strings.Contains(out, "T1.laptop") {
		t.Fatalf("expected short ID T1 in list output without ambiguity:\n%s", out)
	}

	// 2. Add remote task T1.phone
	remoteOp := []sqlstore.Op{
		{
			NodeID:  "phone",
			Seq:     1,
			TS:      time.Now(),
			Kind:    sqlstore.KindTaskCreated,
			Payload: []byte(`{"id":"T1.phone","title":"Replace Phone Battery","site":"B","priority":"medium"}`),
		},
	}
	if _, err := st.ApplyRemote(ctx, remoteOp); err != nil {
		t.Fatalf("ApplyRemote: %v", err)
	}
	st.Close()

	// 3. Now that both T1.laptop and T1.phone exist, list must show qualified IDs
	code, out, _ = runTask(t, db, "list")
	if code != 0 {
		t.Fatalf("list failed: %d, out: %s", code, out)
	}
	if !strings.Contains(out, "T1.laptop") || !strings.Contains(out, "T1.phone") {
		t.Fatalf("expected disambiguated qualified IDs in list output:\n%s", out)
	}

	// 4. Attempting to delete using ambiguous "T1" should fail
	code, out, errOut := runTask(t, db, "delete", "T1")
	if code == 0 {
		t.Fatalf("expected delete T1 to fail with ambiguity error, got success: %s", out)
	}
	if !strings.Contains(errOut, "matches 2 tasks") {
		t.Fatalf("expected error mentioning matches 2 tasks, got: %s", errOut)
	}

	// 5. Deleting with fully qualified ID "T1.phone" succeeds
	code, out, _ = runTask(t, db, "delete", "T1.phone")
	if code != 0 || !strings.Contains(out, `deleted T1 "Replace Phone Battery"`) {
		t.Fatalf("delete T1.phone failed: %d, %s", code, out)
	}

	// 6. After deletion of T1.phone, removing note on remaining T1 using short ID succeeds
	code, out, errOut = runTask(t, db, "remove-note", "T1", "1")
	if code != 0 || !strings.Contains(out, "removed note 1 of T1") {
		t.Fatalf("remove-note T1 1 failed: %d, out: %s, err: %s", code, out, errOut)
	}
}
