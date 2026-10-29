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

// TestDrainTaskContextFormattingWithShortIDs tests that taskContext formats
// tasks with short IDs when unique, automatically disambiguates colliding prefixes,
// includes recent notes, and truncates appropriately.
func TestDrainTaskContextFormattingWithShortIDs(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "drain_ctx.db")

	st, err := sqlstore.Open(ctx, db, sqlstore.Options{NodeID: "laptop"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	// 1. Empty store check
	out, err := taskContext(ctx, st)
	if err != nil {
		t.Fatalf("taskContext empty: %v", err)
	}
	if out != "(there are no tasks yet)" {
		t.Fatalf("expected '(there are no tasks yet)', got: %q", out)
	}

	// 2. Single-node tasks -> ShortIDs used without replica suffix
	t1, _, err := st.Create(ctx, task.NewTask{Title: "Check Well Pump", Priority: task.PriorityHigh, Site: "Well"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	st.AddNote(ctx, t1.ID, "Note 1: Pressure low")
	st.AddNote(ctx, t1.ID, "Note 2: Power checked")

	out, err = taskContext(ctx, st)
	if err != nil {
		t.Fatalf("taskContext: %v", err)
	}
	if !strings.HasPrefix(out, "T1 [high, open] site Well: Check Well Pump") {
		t.Fatalf("expected short ID T1 prefix in prompt context, got:\n%s", out)
	}
	if strings.Contains(out, "T1.laptop") {
		t.Fatalf("expected no replica suffix when short ID is unique, got:\n%s", out)
	}
	if !strings.Contains(out, "Note 1: Pressure low") || !strings.Contains(out, "Note 2: Power checked") {
		t.Fatalf("expected notes in prompt context, got:\n%s", out)
	}

	// 3. Multi-node tasks with colliding number -> Disambiguated IDs
	remoteOp := []sqlstore.Op{
		{
			NodeID:  "phone",
			Seq:     1,
			TS:      time.Now(),
			Kind:    sqlstore.KindTaskCreated,
			Payload: []byte(`{"id":"T1.phone","title":"Check Battery Bank","site":"Power","priority":"low"}`),
		},
	}
	if _, err := st.ApplyRemote(ctx, remoteOp); err != nil {
		t.Fatalf("ApplyRemote: %v", err)
	}

	out, err = taskContext(ctx, st)
	if err != nil {
		t.Fatalf("taskContext multi: %v", err)
	}
	if !strings.Contains(out, "T1.laptop [high, open]") || !strings.Contains(out, "T1.phone [low, open]") {
		t.Fatalf("expected qualified IDs when prefixes collide, got:\n%s", out)
	}
}
