package sqlstore

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"offgrid/internal/task"
)

// TestResurrectionByConcurrentNoteAddition tests that if replica A deletes a
// task, but replica B concurrently adds a note without having seen the delete,
// the task resurrects upon synchronization and carries B's new note.
func TestResurrectionByConcurrentNoteAddition(t *testing.T) {
	ctx := context.Background()
	a, b, aclk, bclk := pair(t)

	// Offline: A deletes T1
	aclk.tick(2 * time.Minute)
	if err := a.DeleteTask(ctx, "T1.node-a"); err != nil {
		t.Fatal(err)
	}

	// Offline: B concurrently adds a note to T1 without seeing A's delete
	bclk.tick(time.Minute)
	if _, err := b.AddNote(ctx, "T1.node-a", "critical safety note"); err != nil {
		t.Fatal(err)
	}

	// A sees T1 as deleted
	if len(ids(t, a)) != 0 {
		t.Fatal("replica A should see T1 as deleted before sync")
	}

	// B sees T1 as alive with 1 note
	bTasks := ids(t, b)
	if len(bTasks) != 1 || bTasks[0] != "T1.node-a" {
		t.Fatalf("replica B tasks = %v, want [T1.node-a]", bTasks)
	}

	// Reconnect and sync
	meet(t, a, b)

	for _, s := range []*Store{a, b} {
		tk, err := s.Get(ctx, "T1.node-a")
		if err != nil {
			t.Fatalf("%s: expected T1 to be resurrected, got err: %v", s.node, err)
		}
		if len(tk.Notes) != 1 || tk.Notes[0].Text != "critical safety note" {
			t.Fatalf("%s: expected resurrected note 'critical safety note', got %+v", s.node, tk.Notes)
		}
	}
}

// TestResurrectionByConcurrentStatusUpdate tests that if replica A deletes a
// task while replica B concurrently updates its status, the task is preserved
// with the updated status upon sync (add-wins).
func TestResurrectionByConcurrentStatusUpdate(t *testing.T) {
	ctx := context.Background()
	a, b, aclk, bclk := pair(t)

	// A deletes T1
	aclk.tick(2 * time.Minute)
	if err := a.DeleteTask(ctx, "T1.node-a"); err != nil {
		t.Fatal(err)
	}

	// B concurrently changes status
	bclk.tick(time.Minute)
	newStatus := task.StatusInProgress
	if _, _, err := b.Update(ctx, "T1.node-a", task.Changes{Status: &newStatus}); err != nil {
		t.Fatal(err)
	}

	meet(t, a, b)

	for _, s := range []*Store{a, b} {
		tk, err := s.Get(ctx, "T1.node-a")
		if err != nil {
			t.Fatalf("%s: expected T1 to survive, got %v", s.node, err)
		}
		if tk.Status != newStatus {
			t.Fatalf("%s: expected status %q, got %q", s.node, newStatus, tk.Status)
		}
	}
}

// TestResurrectionByConcurrentPriorityUpdate tests that concurrent priority edits
// resurrect deleted tasks.
func TestResurrectionByConcurrentPriorityUpdate(t *testing.T) {
	ctx := context.Background()
	a, b, aclk, bclk := pair(t)

	aclk.tick(2 * time.Minute)
	if err := a.DeleteTask(ctx, "T1.node-a"); err != nil {
		t.Fatal(err)
	}

	bclk.tick(time.Minute)
	newPrio := task.PriorityLow
	if _, _, err := b.Update(ctx, "T1.node-a", task.Changes{Priority: &newPrio}); err != nil {
		t.Fatal(err)
	}

	meet(t, a, b)

	for _, s := range []*Store{a, b} {
		tk, err := s.Get(ctx, "T1.node-a")
		if err != nil {
			t.Fatalf("%s: expected T1 to survive, got %v", s.node, err)
		}
		if tk.Priority != newPrio {
			t.Fatalf("%s: expected priority %q, got %q", s.node, newPrio, tk.Priority)
		}
	}
}

// TestDualConcurrentDeletes tests that if both replicas independently delete
// the same task while partitioned, reconnection converges cleanly with the task
// remaining deleted and identical state hashes.
func TestDualConcurrentDeletes(t *testing.T) {
	ctx := context.Background()
	a, b, aclk, bclk := pair(t)

	aclk.tick(time.Minute)
	if err := a.DeleteTask(ctx, "T1.node-a"); err != nil {
		t.Fatal(err)
	}

	bclk.tick(time.Minute)
	if err := b.DeleteTask(ctx, "T1.node-a"); err != nil {
		t.Fatal(err)
	}

	meet(t, a, b)

	for _, s := range []*Store{a, b} {
		if got := ids(t, s); len(got) != 0 {
			t.Fatalf("%s: tasks = %v, want empty", s.node, got)
		}
		if _, err := s.Get(ctx, "T1.node-a"); !errors.Is(err, task.ErrNotFound) {
			t.Fatalf("%s: expected ErrNotFound for T1, got %v", s.node, err)
		}
	}
}

// TestRedundantDeleteRefusal tests that calling DeleteTask on an already-deleted
// task returns ErrNotFound.
func TestRedundantDeleteRefusal(t *testing.T) {
	ctx := context.Background()
	a, _, aclk, _ := pair(t)

	aclk.tick(time.Minute)
	if err := a.DeleteTask(ctx, "T1.node-a"); err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteTask(ctx, "T1.node-a"); !errors.Is(err, task.ErrNotFound) {
		t.Fatalf("second delete: expected ErrNotFound, got %v", err)
	}
}

// TestBidirectionalConcurrentNoteRemovals verifies that when two replicas
// simultaneously remove different notes from the same task while partitioned,
// synchronization retains the union of deletions (OR-Set tombstones).
func TestBidirectionalConcurrentNoteRemovals(t *testing.T) {
	ctx := context.Background()
	a, b, aclk, bclk := pair(t)

	// Add three notes to T1
	for _, text := range []string{"first note", "second note", "third note"} {
		if _, err := a.AddNote(ctx, "T1.node-a", text); err != nil {
			t.Fatal(err)
		}
	}
	// Sync initial notes to B
	pull(t, b, a, 100)

	// Partition:
	// A removes note 1 ("first note")
	aclk.tick(time.Minute)
	if _, err := a.RemoveNote(ctx, "T1.node-a", 1); err != nil {
		t.Fatal(err)
	}

	// B removes note 2 ("second note")
	bclk.tick(time.Minute)
	if _, err := b.RemoveNote(ctx, "T1.node-a", 2); err != nil {
		t.Fatal(err)
	}

	meet(t, a, b)

	for _, s := range []*Store{a, b} {
		notes := shownNotes(t, s, "T1.node-a")
		if len(notes) != 1 || notes[0] != "third note" {
			t.Fatalf("%s: expected remaining note ['third note'], got %v", s.node, notes)
		}
	}
}

// TestDeleteAndRecreateSameTitle verifies that deleting a task allows
// creating a new task with the exact same title without triggering the
// duplicate task prevention check.
func TestDeleteAndRecreateSameTitle(t *testing.T) {
	ctx := context.Background()
	a, b, aclk, _ := pair(t)

	aclk.tick(time.Minute)
	if err := a.DeleteTask(ctx, "T1.node-a"); err != nil {
		t.Fatal(err)
	}

	// Recreate with identical title "pump"
	created, isNew, err := a.Create(ctx, task.NewTask{Title: "pump", Priority: task.PriorityMedium})
	if err != nil {
		t.Fatal(err)
	}
	if !isNew {
		t.Fatal("expected isNew=true for recreated task with same title")
	}
	if created.ID == "T1.node-a" {
		t.Fatalf("expected new task ID (e.g. T2.node-a), got reused ID %s", created.ID)
	}

	meet(t, a, b)

	for _, s := range []*Store{a, b} {
		active := ids(t, s)
		if len(active) != 1 || active[0] != created.ID {
			t.Fatalf("%s: expected active tasks [%s], got %v", s.node, created.ID, active)
		}
	}
}

// TestDeleteMarkPruningAndVerification checks that rebuild and verify correctly
// compute the projection and hashes when multiple mark and removal records exist.
func TestDeleteMarkPruningAndVerification(t *testing.T) {
	ctx := context.Background()
	a, aclk := replicaAt(t, "node-a", epoch)

	for i := 0; i < 10; i++ {
		aclk.tick(time.Second)
		create(t, a, fmt.Sprintf("task-%d", i))
	}

	// Delete even tasks
	for i := 0; i < 10; i += 2 {
		aclk.tick(time.Second)
		id := fmt.Sprintf("T%d.node-a", i+1)
		if err := a.DeleteTask(ctx, id); err != nil {
			t.Fatalf("delete %s: %v", id, err)
		}
	}

	hashBefore := hashOf(t, a)
	mustVerify(t, a)

	// Rebuild projection from log ops
	if _, err := a.Rebuild(ctx); err != nil {
		t.Fatalf("rebuild failed: %v", err)
	}

	hashAfter := hashOf(t, a)
	if hashBefore != hashAfter {
		t.Fatalf("state hash changed across rebuild: %s vs %s", hashBefore, hashAfter)
	}
	mustVerify(t, a)

	active := ids(t, a)
	if len(active) != 5 {
		t.Fatalf("expected 5 active tasks, got %d: %v", len(active), active)
	}
}

// TestRandomOracleResurrectionFuzz performs a randomized differential check
// simulating random operations across 3 replicas with network cuts and asserts
// that all replicas converge to identical state hashes when fully connected.
func TestRandomOracleResurrectionFuzz(t *testing.T) {
	ctx := context.Background()
	rng := rand.New(rand.NewSource(42))

	rA, cA := replicaAt(t, "node-a", epoch)
	rB, cB := replicaAt(t, "node-b", epoch)
	rC, cC := replicaAt(t, "node-c", epoch)

	replicas := []*Store{rA, rB, rC}
	clocks := []*clock{cA, cB, cC}

	// Seed task
	create(t, rA, "shared seed task")
	pull(t, rB, rA, 100)
	pull(t, rC, rA, 100)

	for step := 0; step < 100; step++ {
		idx := rng.Intn(3)
		rep := replicas[idx]
		clk := clocks[idx]
		clk.tick(time.Duration(1+rng.Intn(5)) * time.Second)

		active := ids(t, rep)
		action := rng.Intn(6)

		switch action {
		case 0, 1: // Create
			create(t, rep, fmt.Sprintf("task-%d-%s", step, rep.node))
		case 2: // Update
			if len(active) > 0 {
				target := active[rng.Intn(len(active))]
				prio := []task.Priority{task.PriorityLow, task.PriorityMedium, task.PriorityHigh}[rng.Intn(3)]
				rep.Update(ctx, target, task.Changes{Priority: &prio})
			}
		case 3: // Add note
			if len(active) > 0 {
				target := active[rng.Intn(len(active))]
				rep.AddNote(ctx, target, fmt.Sprintf("note-%d", step))
			}
		case 4: // Remove note
			if len(active) > 0 {
				target := active[rng.Intn(len(active))]
				tk, err := rep.Get(ctx, target)
				if err == nil && len(tk.Notes) > 0 {
					rep.RemoveNote(ctx, target, 1+rng.Intn(len(tk.Notes)))
				}
			}
		case 5: // Delete task
			if len(active) > 0 && rng.Intn(2) == 0 {
				target := active[rng.Intn(len(active))]
				rep.DeleteTask(ctx, target)
			}
		}

		// Partial syncs
		if step%5 == 0 {
			src := rng.Intn(3)
			dst := rng.Intn(3)
			if src != dst {
				pull(t, replicas[dst], replicas[src], 50)
			}
		}
	}

	// Full mesh reconciliation
	for round := 0; round < 5; round++ {
		for i := 0; i < 3; i++ {
			for j := 0; j < 3; j++ {
				if i != j {
					pull(t, replicas[i], replicas[j], 200)
				}
			}
		}
	}

	// Assert convergence
	hA := hashOf(t, rA)
	hB := hashOf(t, rB)
	hC := hashOf(t, rC)

	if hA != hB || hB != hC {
		t.Fatalf("replicas failed to converge:\nA: %s\nB: %s\nC: %s", hA, hB, hC)
	}

	for _, r := range replicas {
		mustVerify(t, r)
	}
}

// BenchmarkDeleteTask measures the cost of a single task deletion.
func BenchmarkDeleteTask(b *testing.B) {
	ctx := context.Background()
	s, _ := replicaAt(nil, "node-a", epoch)
	var tids []string
	for i := 0; i < b.N; i++ {
		t, _, _ := s.Create(ctx, task.NewTask{Title: fmt.Sprintf("bench-task-%d", i), Priority: task.PriorityLow})
		tids = append(tids, t.ID)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.DeleteTask(ctx, tids[i]); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRemoveNote measures the cost of removing a note.
func BenchmarkRemoveNote(b *testing.B) {
	ctx := context.Background()
	s, _ := replicaAt(nil, "node-a", epoch)
	t, _, _ := s.Create(ctx, task.NewTask{Title: "note-bench", Priority: task.PriorityLow})
	for i := 0; i < b.N; i++ {
		s.AddNote(ctx, t.ID, fmt.Sprintf("note-%d", i))
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.RemoveNote(ctx, t.ID, 1); err != nil {
			b.Fatal(err)
		}
	}
}
