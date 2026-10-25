package syncer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"offgrid/internal/task"
)

// TestThreeReplicaRingConvergenceWithDeletes tests three replicas (A, B, C)
// communicating in a ring topology. While disconnected, A deletes T1, B updates
// T1 concurrently, and C deletes T2. Once the ring reconnects and performs
// multi-hop sync, all three converge to 100% identical state hashes.
func TestThreeReplicaRingConvergenceWithDeletes(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{t0}

	a := newStore(t, "node-a", clk)
	b := newStore(t, "node-b", clk)
	c := newStore(t, "node-c", clk)

	// A creates seed tasks
	add(t, a, "Task Alpha")
	add(t, a, "Task Beta")
	add(t, a, "Task Gamma")

	srvA := httptest.NewServer(Handler(a, ServerOptions{Key: key, Now: clk.now}))
	defer srvA.Close()
	srvB := httptest.NewServer(Handler(b, ServerOptions{Key: key, Now: clk.now}))
	defer srvB.Close()
	srvC := httptest.NewServer(Handler(c, ServerOptions{Key: key, Now: clk.now}))
	defer srvC.Close()

	clBtoA := &Client{Peer: srvA.URL, Key: key, Now: clk.now}
	clCtoB := &Client{Peer: srvB.URL, Key: key, Now: clk.now}
	clAtoC := &Client{Peer: srvC.URL, Key: key, Now: clk.now}

	// Sync initial state A -> B -> C
	if _, err := clBtoA.Once(ctx, b); err != nil {
		t.Fatal(err)
	}
	if _, err := clCtoB.Once(ctx, c); err != nil {
		t.Fatal(err)
	}

	if !inStep(t, a, b) || !inStep(t, b, c) {
		t.Fatal("initial seed sync failed to bring all 3 in step")
	}

	// Partition all nodes:
	// A deletes T1 (Task Alpha)
	clk.t = clk.t.Add(time.Minute)
	if err := a.DeleteTask(ctx, "T1"); err != nil {
		t.Fatal(err)
	}

	// B concurrently updates T1 priority to High (unseen edit, resurrects T1)
	clk.t = clk.t.Add(time.Minute)
	prio := task.PriorityHigh
	if _, _, err := b.Update(ctx, "T1", task.Changes{Priority: &prio}); err != nil {
		t.Fatal(err)
	}
	b.AddNote(ctx, "T1", "critical fix needed")

	// C deletes T2 (Task Beta) with no concurrent edits
	clk.t = clk.t.Add(time.Minute)
	if err := c.DeleteTask(ctx, "T2"); err != nil {
		t.Fatal(err)
	}

	// Reconnect ring: A -> B -> C -> A
	// Perform rounds until transitive convergence
	for round := 0; round < 4; round++ {
		clBtoA.Once(ctx, b)
		clCtoB.Once(ctx, c)
		clAtoC.Once(ctx, a)
	}

	if hash(t, a) != hash(t, b) || hash(t, b) != hash(t, c) {
		t.Fatalf("ring failed to converge:\nA: %s\nB: %s\nC: %s", hash(t, a), hash(t, b), hash(t, c))
	}

	verify(t, a)
	verify(t, b)
	verify(t, c)

	got := shown(t, a)
	// T1 must exist (resurrected by B) with high priority and note
	t1, ok := got["T1"]
	if !ok {
		t.Fatal("T1 was not resurrected by B's concurrent edit")
	}
	if t1.Priority != task.PriorityHigh {
		t.Errorf("T1 priority = %v, want High", t1.Priority)
	}
	if len(t1.Notes) != 1 || t1.Notes[0].Text != "critical fix needed" {
		t.Errorf("T1 notes = %+v, want ['critical fix needed']", t1.Notes)
	}

	// T2 must be deleted (deleted by C, no concurrent edits)
	if _, ok := got["T2"]; ok {
		t.Error("T2 was deleted by C and should not be present")
	}

	// T3 must exist intact
	if _, ok := got["T3"]; !ok {
		t.Error("T3 should still exist")
	}
}

// TestAlternatingPartitionCycles verifies that multiple consecutive
// offline-edit-reconnect cycles preserve state hash equality and verify integrity.
func TestAlternatingPartitionCycles(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{t0}

	a := newStore(t, "node-a", clk)
	b := newStore(t, "node-b", clk)

	add(t, a, "Base task")
	srv := httptest.NewServer(Handler(a, ServerOptions{Key: key, Now: clk.now}))
	defer srv.Close()
	cl := &Client{Peer: srv.URL, Key: key, Now: clk.now, HTTP: &http.Client{Timeout: 5 * time.Second}}

	if _, err := cl.Once(ctx, b); err != nil {
		t.Fatal(err)
	}

	for cycle := 1; cycle <= 5; cycle++ {
		// Offline cycle:
		clk.t = clk.t.Add(time.Minute)
		tA := add(t, a, fmt.Sprintf("A-cycle-%d", cycle))
		tB := add(t, b, fmt.Sprintf("B-cycle-%d", cycle))

		if cycle%2 == 0 {
			a.DeleteTask(ctx, tA.ID)
		} else {
			b.DeleteTask(ctx, tB.ID)
		}

		// Reconnect & sync:
		if _, err := cl.Once(ctx, b); err != nil {
			t.Fatalf("cycle %d sync failed: %v", cycle, err)
		}

		if hash(t, a) != hash(t, b) {
			t.Fatalf("cycle %d: replicas did not converge", cycle)
		}
		verify(t, a)
		verify(t, b)
	}
}
