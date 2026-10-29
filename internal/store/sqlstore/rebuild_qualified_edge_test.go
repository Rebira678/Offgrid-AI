package sqlstore

import (
	"context"
	"testing"
	"time"

	"offgrid/internal/task"
)

// TestProjectionRebuildDeterminismWithMultiReplicaQualifiedIDs verifies that
// when multiple replicas create and modify qualified task IDs (e.g. T1.node-a, T1.node-b),
// rebuilding the projection from raw ops produces identical state hash and table rows,
// including tasks.n ordering and note positions.
func TestProjectionRebuildDeterminismWithMultiReplicaQualifiedIDs(t *testing.T) {
	ctx := context.Background()
	sA, clkA := replicaAt(t, "node-a", epoch)
	sB, clkB := replicaAt(t, "node-b", epoch.Add(10*time.Second))

	// Node A creates T1.node-a
	tA1, created, err := sA.Create(ctx, task.NewTask{Title: "Task from A", Priority: task.PriorityHigh, Site: "A"})
	if err != nil || !created {
		t.Fatalf("sA.Create: %v", err)
	}

	// Node B creates T1.node-b
	tB1, created, err := sB.Create(ctx, task.NewTask{Title: "Task from B", Priority: task.PriorityLow, Site: "B"})
	if err != nil || !created {
		t.Fatalf("sB.Create: %v", err)
	}

	// Cross-replicate ops between A and B
	pull(t, sA, sB, 100)
	pull(t, sB, sA, 100)

	// Now Node A updates Node B's task (T1.node-b)
	clkA.tick(time.Minute)
	priHigh := task.PriorityHigh
	if _, _, err := sA.Update(ctx, tB1.ID, task.Changes{Priority: &priHigh}); err != nil {
		t.Fatalf("sA update T1.node-b: %v", err)
	}

	// Node B adds a note to Node A's task (T1.node-a)
	clkB.tick(2 * time.Minute)
	if _, err := sB.AddNote(ctx, tA1.ID, "Note from B on A's task"); err != nil {
		t.Fatalf("sB add note on T1.node-a: %v", err)
	}

	// Sync again
	pull(t, sA, sB, 100)
	pull(t, sB, sA, 100)

	// Both must verify
	mustVerify(t, sA)
	mustVerify(t, sB)

	// Save baseline hash on A
	hashA, err := sA.StateHash(ctx)
	if err != nil {
		t.Fatalf("sA.StateHash: %v", err)
	}

	// Rebuild A 3 times consecutively and assert state hash is completely invariant
	for i := 0; i < 3; i++ {
		rep, err := sA.Rebuild(ctx)
		if err != nil {
			t.Fatalf("rebuild iteration %d failed: %v", i, err)
		}
		if rep.Tasks != 2 {
			t.Fatalf("expected 2 tasks after rebuild, got %d", rep.Tasks)
		}
		newHash, err := sA.StateHash(ctx)
		if err != nil {
			t.Fatalf("StateHash after rebuild %d: %v", i, err)
		}
		if newHash != hashA {
			t.Fatalf("hash drifted after rebuild %d: %s vs %s", i, newHash, hashA)
		}
		mustVerify(t, sA)
	}

	// Wipe projection tables completely and call Rebuild
	_, err = sA.db.ExecContext(ctx, `DROP TABLE tasks; DROP TABLE task_notes;`)
	if err != nil {
		t.Fatalf("dropping tables: %v", err)
	}
	rep, err := sA.Rebuild(ctx)
	if err != nil {
		t.Fatalf("Rebuild after DROP TABLE: %v", err)
	}
	if rep.Tasks != 2 {
		t.Fatalf("expected 2 tasks after dropping tables and rebuilding, got %d", rep.Tasks)
	}
	recoveredHash, err := sA.StateHash(ctx)
	if err != nil {
		t.Fatalf("StateHash after recovery: %v", err)
	}
	if recoveredHash != hashA {
		t.Fatalf("recovered hash mismatch: %s vs %s", recoveredHash, hashA)
	}
}
