package sqlstore

import (
	"context"
	"testing"
	"time"

	"offgrid/internal/task"
)

// TestMixedLegacyAndQualifiedTaskIDsCoexistence verifies that a store can safely
// operate on legacy unadorned IDs ("T1") alongside modern qualified IDs ("T1.laptop").
func TestMixedLegacyAndQualifiedTaskIDsCoexistence(t *testing.T) {
	ctx := context.Background()
	s, clk := replicaAt(t, "laptop", epoch)

	// 1. Ingest a legacy task created before Day 24
	legacyOp := []Op{
		{
			NodeID:  "old-node",
			Seq:     1,
			TS:      epoch,
			Kind:    KindTaskCreated,
			Payload: []byte(`{"id":"T1","title":"Legacy Generator","site":"1","priority":"low"}`),
		},
	}
	if _, err := s.ApplyRemote(ctx, legacyOp); err != nil {
		t.Fatalf("applying legacy op: %v", err)
	}

	// 2. Create modern task on this replica (will be assigned T2.laptop because 1 created op already exists)
	clk.tick(time.Minute)
	modern, created, err := s.Create(ctx, task.NewTask{Title: "Modern Pump", Priority: task.PriorityHigh})
	if err != nil || !created {
		t.Fatalf("creating modern task: %v", err)
	}
	if modern.ID != "T2.laptop" {
		t.Fatalf("expected ID T2.laptop, got %q", modern.ID)
	}

	// 3. Both tasks must be independently retrievable by their exact IDs
	legTk, err := s.Get(ctx, "T1")
	if err != nil || legTk.Title != "Legacy Generator" {
		t.Fatalf("Get(T1) failed: %+v, %v", legTk, err)
	}

	modTk, err := s.Get(ctx, "T2.laptop")
	if err != nil || modTk.Title != "Modern Pump" {
		t.Fatalf("Get(T2.laptop) failed: %+v, %v", modTk, err)
	}

	// 4. List must return both tasks
	all, err := s.List(ctx, task.Filter{})
	if err != nil || len(all) != 2 {
		t.Fatalf("List returned %d tasks, want 2: %+v", len(all), all)
	}

	// 5. Add notes to both independently
	if _, err := s.AddNote(ctx, "T1", "note on legacy"); err != nil {
		t.Fatalf("AddNote(T1): %v", err)
	}
	if _, err := s.AddNote(ctx, "T2.laptop", "note on modern"); err != nil {
		t.Fatalf("AddNote(T2.laptop): %v", err)
	}

	legTk, _ = s.Get(ctx, "T1")
	modTk, _ = s.Get(ctx, "T2.laptop")
	if len(legTk.Notes) != 1 || legTk.Notes[0].Text != "note on legacy" {
		t.Errorf("legacy notes mismatch: %+v", legTk.Notes)
	}
	if len(modTk.Notes) != 1 || modTk.Notes[0].Text != "note on modern" {
		t.Errorf("modern notes mismatch: %+v", modTk.Notes)
	}

	// 6. Verify and StateHash must hold with zero integrity errors
	mustVerify(t, s)
}
