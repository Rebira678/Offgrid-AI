package syncer

import (
	"context"
	"fmt"
	"testing"
	"time"

	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
)

// TestRingTopologyGossipSyncWithQualifiedTaskIDs verifies that a 4-node ring gossip network
// (N1 -> N2 -> N3 -> N4 -> N1) where each node performs creations, updates, notes, and deletes
// on qualified task IDs converges to identical state and state hashes across all 4 nodes.
func TestRingTopologyGossipSyncWithQualifiedTaskIDs(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{t: t0}

	names := []string{"ring-1", "ring-2", "ring-3", "ring-4"}
	stores := make([]*sqlstore.Store, 4)
	clients := make([]*Client, 4)

	for i, name := range names {
		s := newStore(t, name, clk)
		stores[i] = s
		c, _ := serve(t, s, clk, key)
		c.Batch = 2 // Small batching to exercise pagination
		clients[i] = c
	}

	// Step 1: Every node creates a task
	for i := 0; i < 4; i++ {
		clk.t = clk.t.Add(time.Second)
		title := fmt.Sprintf("Task from %s", names[i])
		_, created, err := stores[i].Create(ctx, task.NewTask{Title: title, Priority: task.PriorityLow, Site: "Main"})
		if err != nil || !created {
			t.Fatalf("Create on %s: %v", names[i], err)
		}
	}

	// Step 2: Node 1 updates its task, Node 2 adds a note to its task, Node 3 deletes its task
	clk.t = clk.t.Add(time.Second)
	priHigh := task.PriorityHigh
	stores[0].Update(ctx, "T1.ring-1", task.Changes{Priority: &priHigh})

	clk.t = clk.t.Add(time.Second)
	stores[1].AddNote(ctx, "T1.ring-2", "Urgent note from ring-2")

	clk.t = clk.t.Add(time.Second)
	if err := stores[2].DeleteTask(ctx, "T1.ring-3"); err != nil {
		t.Fatalf("delete T1.ring-3: %v", err)
	}

	// Step 3: Run 4 full rounds of ring sync (N0 -> N1 -> N2 -> N3 -> N0)
	for round := 0; round < 4; round++ {
		for i := 0; i < 4; i++ {
			next := (i + 1) % 4
			// Node i syncs with Node next (clients[next] points to stores[next])
			if _, err := clients[next].Once(ctx, stores[i]); err != nil {
				t.Fatalf("ring sync round %d (%s -> %s): %v", round, names[i], names[next], err)
			}
		}
	}

	// Step 4: Verification of convergence
	baseHash, err := stores[0].StateHash(ctx)
	if err != nil {
		t.Fatalf("StateHash on ring-1: %v", err)
	}

	for i := 1; i < 4; i++ {
		h, err := stores[i].StateHash(ctx)
		if err != nil {
			t.Fatalf("StateHash on %s: %v", names[i], err)
		}
		if h != baseHash {
			t.Fatalf("hash divergence at %s: got %s, want %s", names[i], h, baseHash)
		}
	}

	// Verify surviving tasks count (3 alive: ring-1, ring-2, ring-4; ring-3 was deleted)
	for i, name := range names {
		all, err := stores[i].List(ctx, task.Filter{})
		if err != nil {
			t.Fatalf("List on %s: %v", name, err)
		}
		if len(all) != 3 {
			t.Fatalf("expected 3 alive tasks on %s, got %d", name, len(all))
		}
		for _, tk := range all {
			if tk.ID == "T1.ring-3" {
				t.Fatalf("deleted task T1.ring-3 still present on %s", name)
			}
		}
	}
}
