package sqlstore

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"offgrid/internal/task"
)

// TestQualifiedIDGenerationSpecialNodeNames tests that Create formats task IDs correctly
// across stores initialized with complex node names (hyphens, underscores, dots).
func TestQualifiedIDGenerationSpecialNodeNames(t *testing.T) {
	ctx := context.Background()

	nodeNames := []string{
		"laptop-alpha",
		"phone_worker_1",
		"server.site4",
		"node123",
		"n",
	}

	for _, node := range nodeNames {
		s, _ := replicaAt(t, node, epoch)
		tk1, created, err := s.Create(ctx, task.NewTask{Title: "Task 1", Priority: task.PriorityLow})
		if err != nil || !created {
			t.Fatalf("node %s: Create 1 failed: %v", node, err)
		}
		wantID1 := fmt.Sprintf("T1.%s", node)
		if tk1.ID != wantID1 {
			t.Errorf("node %s: expected ID %q, got %q", node, wantID1, tk1.ID)
		}

		tk2, created, err := s.Create(ctx, task.NewTask{Title: "Task 2", Priority: task.PriorityMedium})
		if err != nil || !created {
			t.Fatalf("node %s: Create 2 failed: %v", node, err)
		}
		wantID2 := fmt.Sprintf("T2.%s", node)
		if tk2.ID != wantID2 {
			t.Errorf("node %s: expected ID %q, got %q", node, wantID2, tk2.ID)
		}
	}
}

// TestQualifiedIDCounterNeverReusesDeletedIDs verifies that deleting a task
// does not decrement the ID generation counter; the next task gets the next sequence number.
func TestQualifiedIDCounterNeverReusesDeletedIDs(t *testing.T) {
	ctx := context.Background()
	s, _ := replicaAt(t, "node-seq", epoch)

	tk1, _, _ := s.Create(ctx, task.NewTask{Title: "First", Priority: task.PriorityLow})
	tk2, _, _ := s.Create(ctx, task.NewTask{Title: "Second", Priority: task.PriorityLow})

	if tk1.ID != "T1.node-seq" || tk2.ID != "T2.node-seq" {
		t.Fatalf("unexpected IDs: %s, %s", tk1.ID, tk2.ID)
	}

	// Delete T2
	if err := s.DeleteTask(ctx, tk2.ID); err != nil {
		t.Fatal(err)
	}

	// Create next task: must be T3.node-seq, NOT T2.node-seq
	tk3, _, err := s.Create(ctx, task.NewTask{Title: "Third", Priority: task.PriorityHigh})
	if err != nil {
		t.Fatal(err)
	}
	if tk3.ID != "T3.node-seq" {
		t.Fatalf("expected T3.node-seq, got reused/wrong ID: %s", tk3.ID)
	}
	if !strings.HasSuffix(tk3.ID, ".node-seq") {
		t.Fatalf("ID does not have replica suffix: %s", tk3.ID)
	}
}
