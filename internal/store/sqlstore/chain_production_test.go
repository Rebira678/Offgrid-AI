package sqlstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"offgrid/internal/task"
)

// TestSeniorQA_CorruptedGenesisHashAtSeqOne tests that altering the genesis link
// of a replica's very first op (seq = 1) is immediately caught as an unlinked chain.
func TestSeniorQA_CorruptedGenesisHashAtSeqOne(t *testing.T) {
	ctx := context.Background()
	s, _ := chained(t, "laptop", 3)

	// Tamper: alter prev on seq 1 to a non-genesis hash
	forgedPrev := strings.Repeat("e", 64)
	tamper(t, s, fmt.Sprintf(`UPDATE op_chain SET prev = '%s' WHERE node_id = 'laptop' AND seq = 1`, forgedPrev))

	_, err := s.VerifyChain(ctx)
	if err == nil {
		t.Fatal("expected VerifyChain to fail on corrupted genesis prev hash, but got nil")
	}
	wantTamper(t, err, "laptop", 1, "not linked")
}

// TestSeniorQA_BatchApplyRemoteRollbackAtomicity verifies that if a batch of ops
// has a forged or invalid hash anywhere in the sequence, the entire transaction
// is rolled back and earlier ops in the same batch are NOT persisted.
func TestSeniorQA_BatchApplyRemoteRollbackAtomicity(t *testing.T) {
	ctx := context.Background()
	s, _ := replicaAt(t, "laptop", epoch)

	// Peer "phone" creates 3 ops locally
	p, cp := replicaAt(t, "phone", epoch.Add(time.Second))
	mk(t, p, "task 1 from phone")
	cp.tick(time.Minute)
	mk(t, p, "task 2 from phone")
	cp.tick(time.Minute)
	mk(t, p, "task 3 from phone")

	remoteOps, err := p.Ops(ctx, 0, 100)
	if err != nil || len(remoteOps) != 3 {
		t.Fatalf("failed to read phone ops: len=%d err=%v", len(remoteOps), err)
	}

	// Forge the hash of op 2 in the batch
	corruptedBatch := make([]Op, len(remoteOps))
	copy(corruptedBatch, remoteOps)
	corruptedBatch[1].Hash = strings.Repeat("f", 64)

	// ApplyRemote should fail with ErrChain on corruptedBatch[1]
	res, err := s.ApplyRemote(ctx, corruptedBatch)
	if !errors.Is(err, ErrChain) {
		t.Fatalf("ApplyRemote expected ErrChain, got res=%+v, err=%v", res, err)
	}

	// Crucial production invariant: op 1 must NOT have been committed to SQLite!
	storedOps, err := s.Ops(ctx, 0, 100)
	if err != nil {
		t.Fatalf("failed to query ops: %v", err)
	}
	if len(storedOps) != 0 {
		t.Fatalf("atomicity violation: %d ops were committed despite batch rejection!", len(storedOps))
	}

	// Verify chain must remain completely clean
	rep, err := s.VerifyChain(ctx)
	if err != nil || rep.Ops != 0 {
		t.Fatalf("VerifyChain after rolled-back batch: ops=%d err=%v", rep.Ops, err)
	}
}

// TestSeniorQA_DuplicateOpIdempotentApplyWithConflictingHash verifies that ApplyRemote
// ignores true duplicates with identical hashes, but strictly rejects duplicate (node, seq)
// that arrive with a conflicting hash (indicating history tampering or forking).
func TestSeniorQA_DuplicateOpIdempotentApplyWithConflictingHash(t *testing.T) {
	ctx := context.Background()
	a, _ := replicaAt(t, "laptop", epoch)
	b, _ := replicaAt(t, "phone", epoch)

	mk(t, b, "phone task A")
	bOps, err := b.Ops(ctx, 0, 10)
	if err != nil || len(bOps) != 1 {
		t.Fatalf("bOps: %v", err)
	}

	// 1. Initial valid apply
	if _, err := a.ApplyRemote(ctx, bOps); err != nil {
		t.Fatalf("initial ApplyRemote: %v", err)
	}

	// 2. Idempotent re-apply with same hash must succeed with 0 new applied ops
	res, err := a.ApplyRemote(ctx, bOps)
	if err != nil || res.Applied != 0 || res.Duplicates != 1 {
		t.Fatalf("idempotent ApplyRemote: res=%+v err=%v", res, err)
	}

	// 3. Re-apply same seq with different hash must fail with ErrChain
	conflicting := bOps[0]
	conflicting.Hash = strings.Repeat("1", 64)
	if _, err := a.ApplyRemote(ctx, []Op{conflicting}); !errors.Is(err, ErrChain) {
		t.Fatalf("conflicting duplicate apply: expected ErrChain, got %v", err)
	}
}

// TestSeniorQA_NotesAndDeletionsChainCoverage tests the complete lifecycle of
// task notes and add-wins task deletions under hash chain protection.
func TestSeniorQA_NotesAndDeletionsChainCoverage(t *testing.T) {
	ctx := context.Background()
	s, clk := replicaAt(t, "laptop", epoch)

	// 1. Create task
	tk, _, err := s.Create(ctx, task.NewTask{Title: "Centrifugal pump overhaul", Priority: task.PriorityHigh})
	if err != nil {
		t.Fatal(err)
	}

	// 2. Add notes
	clk.tick(time.Minute)
	if _, err := s.AddNote(ctx, tk.ID, "Inspected impeller wear ring"); err != nil {
		t.Fatal(err)
	}
	clk.tick(time.Minute)
	if _, err := s.AddNote(ctx, tk.ID, "Replaced mechanical seal"); err != nil {
		t.Fatal(err)
	}

	// 3. Remove note 1
	clk.tick(time.Minute)
	if _, err := s.RemoveNote(ctx, tk.ID, 1); err != nil {
		t.Fatal(err)
	}

	// 4. Delete the task
	clk.tick(time.Minute)
	if err := s.DeleteTask(ctx, tk.ID); err != nil {
		t.Fatal(err)
	}

	// Verify the entire chain of 5 ops
	rep, err := s.VerifyChain(ctx)
	if err != nil || rep.Ops != 5 {
		t.Fatalf("VerifyChain after lifecycle: ops=%d err=%v", rep.Ops, err)
	}

	// Tamper: alter the removed note payload at seq 2
	tamper(t, s, `UPDATE ops SET payload = replace(payload, 'impeller', 'rotor') WHERE seq = 2`)
	_, err = s.VerifyChain(ctx)
	wantTamper(t, err, "laptop", 2, "was changed")
}

// TestSeniorQA_AnswerAndSuggestionChainIntegrity validates that Day 22 Tier 2 answers
// and Day 23 suggestions are strictly incorporated into the hash chain.
func TestSeniorQA_AnswerAndSuggestionChainIntegrity(t *testing.T) {
	ctx := context.Background()
	s, clk := replicaAt(t, "node-qa", epoch)

	// 0. Create base task
	tk, _, err := s.Create(ctx, task.NewTask{Title: "Turbine maintenance task", Priority: task.PriorityMedium})
	if err != nil {
		t.Fatal(err)
	}

	// 1. Record Answer
	clk.tick(time.Minute)
	ok, err := s.RecordAnswer(ctx, "q-turb-1", "Check turbine temperature", "Bearing temp is within normal limits.", "llama3.2:3b", "local")
	if err != nil || !ok {
		t.Fatalf("RecordAnswer: ok=%v err=%v", ok, err)
	}

	// 2. Propose a suggestion
	clk.tick(time.Minute)
	prio := task.PriorityHigh
	sug, created, err := s.Suggest(ctx, task.Draft{
		Task:     tk.ID,
		Priority: &prio,
		Note:     "AI detected heat spike",
		By:       "llama3.2:3b",
	})
	if err != nil || !created {
		t.Fatalf("Suggest: created=%v err=%v", created, err)
	}

	// 3. Dismiss suggestion
	clk.tick(time.Minute)
	if _, err := s.Resolve(ctx, sug.ID, false); err != nil {
		t.Fatal(err)
	}

	// Total ops: 1 (create task) + 1 (answer) + 1 (suggest) + 1 (resolve) = 4 ops
	rep, err := s.VerifyChain(ctx)
	if err != nil || rep.Ops != 4 {
		t.Fatalf("VerifyChain on AI ops: ops=%d err=%v", rep.Ops, err)
	}

	// Tamper with the answer op (seq = 2)
	tamper(t, s, `UPDATE ops SET payload = replace(payload, 'Bearing temp', 'Forged temp') WHERE seq = 2`)
	_, err = s.VerifyChain(ctx)
	wantTamper(t, err, "node-qa", 2, "was changed")
}

// TestSeniorQA_ConcurrentWritesAndVerifyStress tests that active durable appends
// under parallel goroutines do not race, deadlock, or produce false tamper alerts
// when VerifyChain or Heads are executed concurrently.
func TestSeniorQA_ConcurrentWritesAndVerifyStress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	s, _ := replicaAt(t, "node-stress", epoch)

	var wg sync.WaitGroup
	errCh := make(chan error, 20)

	// 2 Writer goroutines
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			i := 0
			for {
				select {
				case <-ctx.Done():
					return
				default:
					i++
					_, _, err := s.Create(ctx, task.NewTask{
						Title:    fmt.Sprintf("stress task w%d-%d", workerID, i),
						Priority: task.PriorityLow,
					})
					if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
						errCh <- fmt.Errorf("writer %d op %d: %w", workerID, i, err)
						return
					}
				}
			}
		}(w)
	}

	// 2 Reader goroutines running VerifyChain and Heads
	for r := 0; r < 2; r++ {
		wg.Add(1)
		go func(readerID int) {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
					if _, err := s.VerifyChain(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
						errCh <- fmt.Errorf("reader %d VerifyChain: %w", readerID, err)
						return
					}
					if _, err := s.Heads(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
						errCh <- fmt.Errorf("reader %d Heads: %w", readerID, err)
						return
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
		}(r)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent stress error: %v", err)
	}

	// Final verification of entire accumulated log
	finalRep, err := s.VerifyChain(context.Background())
	if err != nil {
		t.Fatalf("final VerifyChain failed: %v", err)
	}
	if finalRep.Ops == 0 {
		t.Fatal("expected ops to have been written during stress test")
	}
}
