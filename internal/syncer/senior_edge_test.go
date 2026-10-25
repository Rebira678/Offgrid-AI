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

// TestFourNodeSplitBrainLatticeConvergence tests an N=4 replica cluster under
// a network partition into two separate sub-clusters {A, B} and {C, D}.
// In Cluster 1, A deletes T1 and creates T2.
// In Cluster 2, C concurrently edits T1 (status & priority) and D adds a note.
// When the network heals across an arbitrary multi-hop topology, all 4 replicas
// converge to 100% cryptographic state hash parity with T1 resurrected (add-wins).
func TestFourNodeSplitBrainLatticeConvergence(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{t0}

	a := newStore(t, "node-a", clk)
	b := newStore(t, "node-b", clk)
	c := newStore(t, "node-c", clk)
	d := newStore(t, "node-d", clk)

	// A creates ancestor task T1
	add(t, a, "Ancestor task T1")

	srvA := httptest.NewServer(Handler(a, ServerOptions{Key: key, Now: clk.now}))
	defer srvA.Close()
	srvB := httptest.NewServer(Handler(b, ServerOptions{Key: key, Now: clk.now}))
	defer srvB.Close()
	srvC := httptest.NewServer(Handler(c, ServerOptions{Key: key, Now: clk.now}))
	defer srvC.Close()
	srvD := httptest.NewServer(Handler(d, ServerOptions{Key: key, Now: clk.now}))
	defer srvD.Close()

	// Initial sync from A to B, C, D
	cl := func(url string) *Client {
		return &Client{Peer: url, Key: key, Now: clk.now, HTTP: &http.Client{Timeout: 5 * time.Second}}
	}

	if _, err := cl(srvA.URL).Once(ctx, b); err != nil {
		t.Fatal(err)
	}
	if _, err := cl(srvA.URL).Once(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := cl(srvA.URL).Once(ctx, d); err != nil {
		t.Fatal(err)
	}

	if !inStep(t, a, b) || !inStep(t, a, c) || !inStep(t, a, d) {
		t.Fatal("initial 4-node sync failed to align")
	}

	// === SPLIT-BRAIN PARTITION: {A, B} vs {C, D} ===

	// Cluster 1 edits:
	clk.t = clk.t.Add(time.Minute)
	if err := a.DeleteTask(ctx, "T1"); err != nil {
		t.Fatal(err)
	}
	t2 := add(t, a, "Task Two from A")

	// B syncs with A in Cluster 1
	if _, err := cl(srvA.URL).Once(ctx, b); err != nil {
		t.Fatal(err)
	}
	if !inStep(t, a, b) {
		t.Fatal("cluster 1 nodes A and B did not sync")
	}

	// Cluster 2 edits (concurrent, unaware of T1 delete):
	clk.t = clk.t.Add(time.Minute)
	prioHigh := task.PriorityHigh
	stInProgress := task.StatusInProgress
	if _, _, err := c.Update(ctx, "T1", task.Changes{Priority: &prioHigh, Status: &stInProgress}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AddNote(ctx, "T1", "critical cluster 2 note"); err != nil {
		t.Fatal(err)
	}
	t3 := add(t, d, "Task Three from D")

	// C and D sync within Cluster 2
	if _, err := cl(srvD.URL).Once(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := cl(srvC.URL).Once(ctx, d); err != nil {
		t.Fatal(err)
	}
	if !inStep(t, c, d) {
		t.Fatal("cluster 2 nodes C and D did not sync")
	}

	// Verify clusters differ while partitioned
	if inStep(t, a, c) {
		t.Fatal("clusters should differ while partitioned")
	}

	// === NETWORK HEALS: Cross-cluster gossip ===
	// Topology: Line A <-> C <-> D <-> B
	for round := 0; round < 6; round++ {
		cl(srvC.URL).Once(ctx, a)
		cl(srvA.URL).Once(ctx, c)

		cl(srvD.URL).Once(ctx, c)
		cl(srvC.URL).Once(ctx, d)

		cl(srvB.URL).Once(ctx, d)
		cl(srvD.URL).Once(ctx, b)
	}

	// Verify all 4 replicas converged to identical state
	hA := hash(t, a)
	hB := hash(t, b)
	hC := hash(t, c)
	hD := hash(t, d)

	if hA != hB || hB != hC || hC != hD {
		t.Fatalf("4-node cluster failed to converge:\nA: %s\nB: %s\nC: %s\nD: %s", hA, hB, hC, hD)
	}

	verify(t, a)
	verify(t, b)
	verify(t, c)
	verify(t, d)

	// Check domain invariant: T1 must be resurrected with C's and D's edits
	allA := shown(t, a)
	t1, ok := allA["T1"]
	if !ok {
		t.Fatal("T1 was not resurrected on converged cluster")
	}
	if t1.Status != task.StatusInProgress || t1.Priority != task.PriorityHigh {
		t.Errorf("T1 status=%s, prio=%s; want in_progress/high", t1.Status, t1.Priority)
	}
	if len(t1.Notes) != 1 || t1.Notes[0].Text != "critical cluster 2 note" {
		t.Errorf("T1 notes = %+v, want ['critical cluster 2 note']", t1.Notes)
	}

	// Both newly created tasks exist
	if _, ok := allA[t2.ID]; !ok {
		t.Errorf("task T2 (%s) missing on converged cluster", t2.ID)
	}
	if _, ok := allA[t3.ID]; !ok {
		t.Errorf("task T3 (%s) missing on converged cluster", t3.ID)
	}
}

// TestSyncPaginationWithManyDeletions verifies that sync client batching
// accurately paginates deletions across small chunk boundaries (e.g. batch size 5)
// without dropping, reordering, or skipping tombstones.
func TestSyncPaginationWithManyDeletions(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{t0}

	a := newStore(t, "node-a", clk)
	b := newStore(t, "node-b", clk)

	// Create 30 tasks on A
	var createdIDs []string
	for i := 0; i < 30; i++ {
		clk.t = clk.t.Add(time.Second)
		tk := add(t, a, fmt.Sprintf("batch-task-%d", i))
		createdIDs = append(createdIDs, tk.ID)
	}

	srvA := httptest.NewServer(Handler(a, ServerOptions{Key: key, Now: clk.now}))
	defer srvA.Close()

	// Initial sync A -> B with small batch size 4
	clSmall := &Client{Peer: srvA.URL, Key: key, Now: clk.now, Batch: 4}
	if _, err := clSmall.Once(ctx, b); err != nil {
		t.Fatal(err)
	}
	if !inStep(t, a, b) {
		t.Fatal("failed initial sync with batch=4")
	}

	// A deletes even-indexed tasks (15 deletions) and adds notes to odd ones
	for i := 0; i < len(createdIDs); i++ {
		clk.t = clk.t.Add(time.Second)
		id := createdIDs[i]
		if i%2 == 0 {
			if err := a.DeleteTask(ctx, id); err != nil {
				t.Fatalf("delete %s: %v", id, err)
			}
		} else {
			if _, err := a.AddNote(ctx, id, fmt.Sprintf("note for %s", id)); err != nil {
				t.Fatalf("add note %s: %v", id, err)
			}
		}
	}

	// Sync B from A with batch=3 (forces 10+ HTTP pagination chunks)
	clTiny := &Client{Peer: srvA.URL, Key: key, Now: clk.now, Batch: 3}
	rep, err := clTiny.Once(ctx, b)
	if err != nil {
		t.Fatalf("sync with batch=3 failed: %v", err)
	}
	if rep.Pulled < 30 {
		t.Fatalf("expected at least 30 pulled ops, got %d", rep.Pulled)
	}

	if !inStep(t, a, b) {
		t.Fatal("replicas did not converge after paginated sync")
	}

	verify(t, a)
	verify(t, b)

	shownB := shown(t, b)
	if len(shownB) != 15 {
		t.Fatalf("expected exactly 15 surviving tasks, got %d", len(shownB))
	}
	for i, id := range createdIDs {
		if i%2 == 0 {
			if _, ok := shownB[id]; ok {
				t.Fatalf("task %s was deleted but is shown on B", id)
			}
		} else {
			tk, ok := shownB[id]
			if !ok || len(tk.Notes) != 1 {
				t.Fatalf("task %s should survive with 1 note, got %+v", id, tk)
			}
		}
	}
}
