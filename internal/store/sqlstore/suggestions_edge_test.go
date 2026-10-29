package sqlstore

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"offgrid/internal/task"
)

func TestSuggestionsEdgeOrthogonalFieldEditDoesNotMakeStale(t *testing.T) {
	ctx := context.Background()
	s, clk := freshStore(t)

	// Step 1: Create task T1 (Status: open, Priority: medium)
	t1, _, err := s.Create(ctx, task.NewTask{
		Title:    "Cooling fan maintenance",
		Site:     "Site-A",
		Priority: task.PriorityMedium,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Step 2: AI proposes setting priority to high (leaves status untouched)
	clk.tick(time.Minute)
	hi := task.PriorityHigh
	sug, created, err := s.Suggest(ctx, task.Draft{
		Task:     t1.ID,
		Priority: &hi,
		By:       "qwen2.5:7b",
		Key:      "job-fan-prio",
	})
	if err != nil || !created {
		t.Fatalf("suggest failed: created=%v err=%v", created, err)
	}
	if sug.Stale {
		t.Fatal("newly proposed suggestion should not be stale")
	}

	// Step 3: A human edits an orthogonal field: Status -> in_progress (Priority is untouched)
	clk.tick(2 * time.Minute)
	inProg := task.StatusInProgress
	if _, _, err := s.Update(ctx, t1.ID, task.Changes{Status: &inProg}); err != nil {
		t.Fatalf("human update status failed: %v", err)
	}

	// Step 4: Verify the suggestion is STILL NOT STALE because the human did not touch priority
	list, err := s.Suggestions(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Stale {
		t.Fatalf("expected 1 non-stale suggestion, got: %+v", list)
	}

	// Step 5: Accepting the suggestion must succeed and apply the priority while preserving the human's status
	updatedTask, err := s.Resolve(ctx, sug.ID, true)
	if err != nil {
		t.Fatalf("resolve accept failed: %v", err)
	}
	if updatedTask.Priority != task.PriorityHigh {
		t.Fatalf("priority was not updated to high: %s", updatedTask.Priority)
	}
	if updatedTask.Status != task.StatusInProgress {
		t.Fatalf("human's in_progress status was overwritten: %s", updatedTask.Status)
	}

	mustVerify(t, s)
}

func TestSuggestionsEdgeDeletedTaskCannotReceiveSuggestionDirectly(t *testing.T) {
	ctx := context.Background()
	s, clk := freshStore(t)

	t1, _, err := s.Create(ctx, task.NewTask{Title: "Generator belt", Site: "Site-1", Priority: task.PriorityLow})
	if err != nil {
		t.Fatal(err)
	}

	clk.tick(time.Minute)
	if err := s.DeleteTask(ctx, t1.ID); err != nil {
		t.Fatal(err)
	}

	// Attempting to suggest on deleted task must fail with task.ErrNotFound
	clk.tick(time.Minute)
	hi := task.PriorityHigh
	_, created, err := s.Suggest(ctx, task.Draft{
		Task:     t1.ID,
		Priority: &hi,
		By:       "qwen",
		Key:      "key-deleted",
	})
	if !errors.Is(err, task.ErrNotFound) {
		t.Fatalf("expected task.ErrNotFound on deleted task, got: created=%v err=%v", created, err)
	}
}

func TestSuggestionsEdgeConcurrentDeleteAndAcceptResurrection(t *testing.T) {
	ctx := context.Background()
	// Two replicas: Lidia and Beka
	lidia, beka, lclk, bclk := pair(t)

	// Step 1: Create T1 on Lidia and sync to Beka
	t1, _, err := lidia.Create(ctx, task.NewTask{Title: "Substation battery", Priority: task.PriorityLow})
	if err != nil {
		t.Fatal(err)
	}
	meet(t, lidia, beka)

	// Step 2: On Lidia (offline), an AI proposes setting priority to high, and Lidia accepts it
	lclk.tick(time.Minute)
	hi := task.PriorityHigh
	sug, created, err := lidia.Suggest(ctx, task.Draft{Task: t1.ID, Priority: &hi, By: "model-lidia", Key: "k-sub"})
	if err != nil || !created {
		t.Fatalf("lidia suggest failed: %v", err)
	}
	lclk.tick(time.Minute)
	if _, err := lidia.Resolve(ctx, sug.ID, true); err != nil {
		t.Fatalf("lidia accept failed: %v", err)
	}

	// Step 3: On Beka (offline, concurrently), Beka deletes T1 without seeing Lidia's accept
	bclk.tick(2 * time.Minute)
	if err := beka.DeleteTask(ctx, t1.ID); err != nil {
		t.Fatalf("beka delete failed: %v", err)
	}

	// Verify before sync: Beka sees T1 as deleted
	if _, err := beka.Get(ctx, t1.ID); !errors.Is(err, task.ErrNotFound) {
		t.Fatalf("beka should see T1 as deleted before sync, got err=%v", err)
	}

	// Step 4: Reconnect and meet
	meet(t, lidia, beka)

	// Step 5: T1 must be resurrected on Beka (Add-wins for human accept op) with PriorityHigh
	for _, node := range []*Store{lidia, beka} {
		tk, err := node.Get(ctx, t1.ID)
		if err != nil {
			t.Fatalf("node %s: expected T1 to be resurrected by accepted suggestion, got err: %v", node.NodeID(), err)
		}
		if tk.Priority != task.PriorityHigh {
			t.Fatalf("node %s: resurrected task has priority %s, want High", node.NodeID(), tk.Priority)
		}
	}

	if hashOf(t, lidia) != hashOf(t, beka) {
		t.Fatalf("state hash divergence between Lidia and Beka: %s != %s", hashOf(t, lidia), hashOf(t, beka))
	}
}

func TestSuggestionsEdgeThreeNodeSplitBrainConvergence(t *testing.T) {
	ctx := context.Background()
	a, aclk := replicaAt(t, "node-lidia", epoch)
	b, bclk := replicaAt(t, "node-beka", epoch)
	c, cclk := replicaAt(t, "node-charlie", epoch)

	// Seed task on Lidia and propagate
	t1, _, err := a.Create(ctx, task.NewTask{Title: "Primary inverter", Priority: task.PriorityLow})
	if err != nil {
		t.Fatal(err)
	}
	meet(t, a, b)
	meet(t, b, c)

	// Lidia proposes PriorityHigh at T+1m
	aclk.tick(time.Minute)
	hi := task.PriorityHigh
	sugA, _, err := a.Suggest(ctx, task.Draft{Task: t1.ID, Priority: &hi, By: "ai-lidia", Key: "sug-1"})
	if err != nil {
		t.Fatal(err)
	}

	// Beka accepts sugA when received, but offline Beka proposes Note
	bclk.tick(2 * time.Minute)
	sugB, _, err := b.Suggest(ctx, task.Draft{Task: t1.ID, Note: "Inverter voltage fluctuating", By: "ai-beka", Key: "sug-2"})
	if err != nil {
		t.Fatal(err)
	}

	// Charlie proposes StatusDone at T+3m
	cclk.tick(3 * time.Minute)
	done := task.StatusDone
	sugC, _, err := c.Suggest(ctx, task.Draft{Task: t1.ID, Status: &done, By: "ai-charlie", Key: "sug-3"})
	if err != nil {
		t.Fatal(err)
	}

	// Reconnect in ring and sync
	meet(t, a, b)
	meet(t, b, c)
	meet(t, c, a)
	meet(t, a, b)
	meet(t, b, c)

	// Now all nodes have all 3 suggestions pending
	for _, node := range []*Store{a, b, c} {
		p := pending(t, node)
		if len(p) != 3 {
			t.Fatalf("node %s has %d pending suggestions, want 3", node.NodeID(), len(p))
		}
	}

	// Node Lidia accepts sugA (sets priority to high)
	aclk.tick(5 * time.Minute)
	if _, err := a.Resolve(ctx, sugA.ID, true); err != nil {
		t.Fatalf("a accept sugA: %v", err)
	}

	// Node Beka dismisses sugC
	bclk.tick(6 * time.Minute)
	if _, err := b.Resolve(ctx, sugC.ID, false); err != nil {
		t.Fatalf("b dismiss sugC: %v", err)
	}

	// Node Charlie accepts sugB (adds note)
	cclk.tick(7 * time.Minute)
	if _, err := c.Resolve(ctx, sugB.ID, true); err != nil {
		t.Fatalf("c accept sugB: %v", err)
	}

	// Ring sync again
	meet(t, a, b)
	meet(t, b, c)
	meet(t, c, a)
	meet(t, a, b)
	meet(t, b, c)

	// State hashes must be 100% identical across all 3 nodes
	hashA := hashOf(t, a)
	hashB := hashOf(t, b)
	hashC := hashOf(t, c)
	if hashA != hashB || hashB != hashC {
		t.Fatalf("state hash divergence across 3 nodes: A=%s B=%s C=%s", hashA, hashB, hashC)
	}

	// All nodes must see T1 with PriorityHigh and 1 note
	for _, node := range []*Store{a, b, c} {
		tk, err := node.Get(ctx, t1.ID)
		if err != nil {
			t.Fatal(err)
		}
		if tk.Priority != task.PriorityHigh {
			t.Fatalf("node %s priority = %s, want High", node.NodeID(), tk.Priority)
		}
		if len(tk.Notes) != 1 || tk.Notes[0].Text != "Inverter voltage fluctuating" {
			t.Fatalf("node %s notes = %+v, want 1 note", node.NodeID(), tk.Notes)
		}
		// 0 pending suggestions
		if len(pending(t, node)) != 0 {
			t.Fatalf("node %s has %d pending suggestions, want 0", node.NodeID(), len(pending(t, node)))
		}
	}
}

func TestSuggestionsEdgeTamperingAndRebuild(t *testing.T) {
	ctx := context.Background()
	s, _ := replicaAt(t, "node-lidia", epoch)
	tk := create(t, s, "Generator rotor")

	hi := task.PriorityHigh
	sug, _, err := s.Suggest(ctx, task.Draft{Task: tk.ID, Priority: &hi, By: "qwen", Key: "key-tamper"})
	if err != nil {
		t.Fatal(err)
	}
	cleanHash := hashOf(t, s)

	// Tamper directly with the suggestions projection table behind the store's back
	if _, err := s.db.ExecContext(ctx, `UPDATE suggestions SET note = 'TAMPERED_NOTE' WHERE node = ? AND seq = ?`, "node-lidia", 2); err != nil {
		t.Fatal(err)
	}

	// Verify must detect discrepancy and report ErrDiverged
	rep, err := s.Verify(ctx)
	if !errors.Is(err, ErrDiverged) {
		t.Fatalf("expected ErrDiverged, got err=%v rep=%+v", err, rep)
	}

	// Rebuild repairs state from the op-log
	if _, err := s.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild failed: %v", err)
	}

	rep, err = s.Verify(ctx)
	if err != nil {
		t.Fatalf("Verify failed after rebuild: %v", err)
	}
	if rep.LiveHash != cleanHash || rep.ReplayHash != cleanHash {
		t.Fatalf("rebuilt hashes do not match clean hash %s: live=%s replay=%s", cleanHash, rep.LiveHash, rep.ReplayHash)
	}

	// Check suggestion is intact
	foundSug, err := s.Suggestions(ctx, false)
	if err != nil || len(foundSug) != 1 {
		t.Fatalf("suggestions after rebuild: %+v", foundSug)
	}
	if foundSug[0].ID != sug.ID || foundSug[0].Note != "" {
		t.Fatalf("tampered note persisted after rebuild: %+v", foundSug[0])
	}
}

func TestSuggestionsEdgeDismissSuggestionOnDeletedTask(t *testing.T) {
	ctx := context.Background()
	s, clk := freshStore(t)

	// Step 1: Create task T1
	t1, _, err := s.Create(ctx, task.NewTask{Title: "Wind turbine brake", Site: "Ridge-1", Priority: task.PriorityLow})
	if err != nil {
		t.Fatal(err)
	}

	// Step 2: AI proposes priority update
	clk.tick(time.Minute)
	hi := task.PriorityHigh
	sug, created, err := s.Suggest(ctx, task.Draft{Task: t1.ID, Priority: &hi, By: "ai-rotor", Key: "k-brake"})
	if err != nil || !created {
		t.Fatalf("suggest failed: created=%v err=%v", created, err)
	}

	// Step 3: Human deletes T1
	clk.tick(time.Minute)
	if err := s.DeleteTask(ctx, t1.ID); err != nil {
		t.Fatal(err)
	}

	// Step 4: Accepting it must be refused with ErrNotFound
	clk.tick(time.Minute)
	_, err = s.Resolve(ctx, sug.ID, true)
	if !errors.Is(err, task.ErrNotFound) {
		t.Fatalf("expected ErrNotFound when accepting proposal on deleted task, got: %v", err)
	}

	// Step 6: Dismissing it must SUCCEED cleanly
	clk.tick(time.Minute)
	taskRet, err := s.Resolve(ctx, sug.ID, false)
	if err != nil {
		t.Fatalf("dismissing suggestion on deleted task should succeed, got: %v", err)
	}
	if taskRet.ID != t1.ID {
		t.Fatalf("expected returned task ID %s, got %s", t1.ID, taskRet.ID)
	}

	// Verify suggestion is no longer pending
	pendingList, err := s.Suggestions(ctx, false)
	if err != nil || len(pendingList) != 0 {
		t.Fatalf("expected 0 pending suggestions after dismissal, got: %+v", pendingList)
	}

	mustVerify(t, s)
}

func TestSuggestionsEdgeConcurrentProposalsSameKeyIdempotency(t *testing.T) {
	ctx := context.Background()
	s, _ := freshStore(t)

	t1, _, err := s.Create(ctx, task.NewTask{Title: "Solar inverter", Priority: task.PriorityMedium})
	if err != nil {
		t.Fatal(err)
	}

	const goroutines = 10
	type res struct {
		sug     task.Suggestion
		created bool
		err     error
	}
	results := make([]res, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		idx := i
		go func() {
			defer wg.Done()
			hi := task.PriorityHigh
			sug, created, err := s.Suggest(ctx, task.Draft{
				Task:     t1.ID,
				Priority: &hi,
				By:       "idempotent-worker",
				Key:      "idempotent-key-42",
			})
			results[idx] = res{sug: sug, created: created, err: err}
		}()
	}
	wg.Wait()

	createdCount := 0
	var firstID string
	for _, r := range results {
		if r.err != nil {
			t.Fatalf("unexpected error in concurrent suggest: %v", r.err)
		}
		if r.created {
			createdCount++
			firstID = r.sug.ID
		}
	}

	if createdCount != 1 {
		t.Fatalf("expected exactly 1 proposal created, got %d", createdCount)
	}

	for _, r := range results {
		if r.sug.ID != firstID {
			t.Fatalf("mismatched suggestion ID returned: got %s, want %s", r.sug.ID, firstID)
		}
	}

	mustVerify(t, s)
}

func TestSuggestionsEdgeDoubleResolutionOpReplayPurity(t *testing.T) {
	ctx := context.Background()
	lidia, beka, lclk, bclk := pair(t)

	// Step 1: Create task on Lidia and sync
	t1, _, err := lidia.Create(ctx, task.NewTask{Title: "Hydro sensor", Priority: task.PriorityLow})
	if err != nil {
		t.Fatal(err)
	}
	meet(t, lidia, beka)

	// Step 2: Propose suggestion on Lidia and sync to Beka
	lclk.tick(time.Minute)
	hi := task.PriorityHigh
	sug, _, err := lidia.Suggest(ctx, task.Draft{Task: t1.ID, Priority: &hi, By: "model-x", Key: "k-hydro"})
	if err != nil {
		t.Fatal(err)
	}
	meet(t, lidia, beka)

	// Step 3: Concurrently while disconnected:
	// Lidia accepts sug
	lclk.tick(time.Minute)
	if _, err := lidia.Resolve(ctx, sug.ID, true); err != nil {
		t.Fatalf("lidia accept: %v", err)
	}

	// Beka dismisses sug
	bclk.tick(2 * time.Minute)
	if _, err := beka.Resolve(ctx, sug.ID, false); err != nil {
		t.Fatalf("beka dismiss: %v", err)
	}

	// Step 4: Reconnect and meet in both directions
	meet(t, lidia, beka)
	meet(t, beka, lidia)

	// State hashes must be 100% identical and verify clean
	if hashOf(t, lidia) != hashOf(t, beka) {
		t.Fatalf("hash mismatch on double resolution: %s != %s", hashOf(t, lidia), hashOf(t, beka))
	}

	mustVerify(t, lidia)
	mustVerify(t, beka)
}
