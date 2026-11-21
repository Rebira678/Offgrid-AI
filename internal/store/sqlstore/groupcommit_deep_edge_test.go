package sqlstore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"offgrid/internal/task"
)

func TestGroupCommitBeforeCommitFailureRollsBackBatch(t *testing.T) {
	ctx := context.Background()
	s := open(t, newDB(t), Options{NodeID: "n"})
	defer s.Close()

	commitErr := errors.New("simulated commit hook failure")
	s.gc.beforeCommit = func() error {
		return commitErr
	}

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, _, err := s.Create(ctx, task.NewTask{Title: fmt.Sprintf("t-%d", idx), Priority: task.PriorityMedium})
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if !errors.Is(err, commitErr) {
			t.Fatalf("caller %d expected %v, got %v", i, commitErr, err)
		}
	}

	// Verify no tasks were written
	all, err := s.List(ctx, task.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatalf("expected 0 tasks after commit failure, got %d", len(all))
	}

	// Clear hook and verify store accepts new writes cleanly
	s.gc.beforeCommit = nil
	created, _, err := s.Create(ctx, task.NewTask{Title: "recovered", Priority: task.PriorityMedium})
	if err != nil {
		t.Fatalf("expected write to succeed after clearing hook: %v", err)
	}
	got, err := s.Get(ctx, created.ID)
	if err != nil || got.Title != "recovered" {
		t.Fatalf("expected task recovered, got %v, err=%v", got, err)
	}
}

func TestGroupCommitSingleWritePanicRollbackAndReraise(t *testing.T) {
	ctx := context.Background()
	s := open(t, newDB(t), Options{NodeID: "n", MaxBatch: 1})
	defer s.Close()

	panicMsg := "critical unhandled logic panic"
	didPanic := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				didPanic = true
				if r != panicMsg {
					t.Fatalf("unexpected panic value: %v", r)
				}
			}
		}()
		_ = s.write(ctx, func(tx execer) error {
			_, _ = tx.ExecContext(ctx, "CREATE TABLE t_dummy (x INT)")
			panic(panicMsg)
		})
	}()

	if !didPanic {
		t.Fatal("expected panic to be re-raised in caller goroutine")
	}

	// Verify committer is still healthy and accepts subsequent writes
	created, _, err := s.Create(ctx, task.NewTask{Title: "after-single-panic", Priority: task.PriorityMedium})
	if err != nil {
		t.Fatalf("store failed after single panic: %v", err)
	}
	if got, err := s.Get(ctx, created.ID); err != nil || got.Title != "after-single-panic" {
		t.Fatalf("unexpected task state: %v, err=%v", got, err)
	}
}

func TestGroupCommitBatchPartialPanicAndSuccess(t *testing.T) {
	ctx := context.Background()
	s := open(t, newDB(t), Options{NodeID: "n", FlushWindow: 20 * time.Millisecond, MaxBatch: 10})
	defer s.Close()

	var wg sync.WaitGroup
	var panicCaught bool
	var normalErr error

	// Goroutine 1: panics
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				panicCaught = true
				if r != "goroutine-panic-target" {
					t.Errorf("unexpected panic caught: %v", r)
				}
			}
		}()
		_ = s.write(ctx, func(tx execer) error {
			panic("goroutine-panic-target")
		})
	}()

	// Goroutine 2: succeeds in the same batch window
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _, normalErr = s.Create(ctx, task.NewTask{Title: "survivor-in-batch", Priority: task.PriorityMedium})
	}()

	wg.Wait()

	if !panicCaught {
		t.Fatal("expected panic to be caught by panic handler")
	}
	if normalErr != nil {
		t.Fatalf("expected concurrent write to succeed, got %v", normalErr)
	}

	// Verify survivor is present
	tasks, err := s.List(ctx, task.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Title != "survivor-in-batch" {
		t.Fatalf("expected survivor task, got %+v", tasks)
	}
}

func TestGroupCommitClaimedContextCancellationReportsCommitResult(t *testing.T) {
	ctx := context.Background()
	s := open(t, newDB(t), Options{NodeID: "n", FlushWindow: 50 * time.Millisecond})
	defer s.Close()

	// Create a context that will be cancelled right after submission
	callCtx, cancel := context.WithCancel(ctx)

	claimedStarted := make(chan struct{})
	unblockWrite := make(chan struct{})

	// Submit a write that signals when it has started executing
	done := make(chan error, 1)
	go func() {
		done <- s.write(callCtx, func(tx execer) error {
			close(claimedStarted)
			<-unblockWrite
			_, err := tx.ExecContext(context.Background(), "INSERT INTO meta (key, value) VALUES ('test_claim', 'persisted')")
			return err
		})
	}()

	// Wait for the write to be claimed and running in the committer
	<-claimedStarted

	// Now cancel the caller's context while the write is already claimed!
	cancel()

	// Unblock the write
	close(unblockWrite)

	// Caller should get nil (or the actual commit result), because once claimed,
	// group commit runs to completion and finishes the batch!
	err := <-done
	if err != nil {
		t.Fatalf("expected claimed write to finish successfully, got %v", err)
	}

	// Verify that the mutation actually landed on disk
	val, ok, err := getMeta(context.Background(), s.db, "test_claim")
	if err != nil || !ok || val != "persisted" {
		t.Fatalf("expected meta test_claim='persisted', got val=%q ok=%v err=%v", val, ok, err)
	}
}

func TestGroupCommitStoreDoubleCloseAndSubmitToClosed(t *testing.T) {
	ctx := context.Background()
	s := open(t, newDB(t), Options{NodeID: "n"})

	// First close succeeds
	if err := s.Close(); err != nil {
		t.Fatalf("first close failed: %v", err)
	}

	// Subsequent close on committer is idempotent
	s.gc.close()

	// Submit on closed store returns ErrClosed
	err := s.write(ctx, func(tx execer) error {
		return nil
	})
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("expected ErrClosed on closed store, got %v", err)
	}
}

func TestGroupCommitStatsCounting(t *testing.T) {
	ctx := context.Background()
	s := open(t, newDB(t), Options{NodeID: "n", MaxBatch: 4})
	defer s.Close()

	initialStats := s.CommitStats()
	if initialStats.Writes != 0 || initialStats.Batches != 0 {
		t.Fatalf("initial stats not zero: %+v", initialStats)
	}

	// 1. One successful write
	_, _, err := s.Create(ctx, task.NewTask{Title: "stats-1", Priority: task.PriorityMedium})
	if err != nil {
		t.Fatal(err)
	}

	// 2. One failing write
	failingErr := errors.New("deliberate failure")
	_ = s.write(ctx, func(tx execer) error {
		return failingErr
	})

	// 3. One pre-cancelled write
	cancCtx, cancel := context.WithCancel(ctx)
	cancel()
	_ = s.write(cancCtx, func(tx execer) error {
		return nil
	})

	stats := s.CommitStats()
	if stats.Writes < 1 {
		t.Fatalf("expected at least 1 write in stats, got %d", stats.Writes)
	}
	if stats.Failed < 1 {
		t.Fatalf("expected at least 1 failed in stats, got %d", stats.Failed)
	}
	if stats.Batches < 1 {
		t.Fatalf("expected at least 1 batch in stats, got %d", stats.Batches)
	}
}
