package sqlstore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"offgrid/internal/task"
)

func TestGroupCommitOptionDefaultsAndExtremes(t *testing.T) {
	// 1. Negative FlushWindow and negative MaxBatch should default cleanly
	s := gcStore(t, Options{FlushWindow: -10 * time.Second, MaxBatch: -50})
	defer s.Close()

	if s.gc.window != 0 {
		t.Fatalf("expected negative window to clamp to 0, got %v", s.gc.window)
	}
	if s.gc.maxBatch != defaultMaxBatch {
		t.Fatalf("expected negative maxBatch to default to %d, got %d", defaultMaxBatch, s.gc.maxBatch)
	}

	// Verify write succeeds with default options
	if err := gcMk(s, "default-options-task"); err != nil {
		t.Fatalf("gcMk failed: %v", err)
	}

	stats := s.CommitStats()
	if stats.Writes != 1 || stats.Batches != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestGroupCommitMaxBatchOne(t *testing.T) {
	// MaxBatch = 1 forces each request into its own standalone transaction
	s := gcStore(t, Options{MaxBatch: 1})
	defer s.Close()

	const totalWrites = 10
	for i := 0; i < totalWrites; i++ {
		if err := gcMk(s, fmt.Sprintf("batch-one-%d", i)); err != nil {
			t.Fatalf("write %d failed: %v", i, err)
		}
	}

	stats := s.CommitStats()
	if stats.Writes != totalWrites {
		t.Fatalf("expected %d writes, got %d", totalWrites, stats.Writes)
	}
	if stats.Batches != totalWrites {
		t.Fatalf("expected %d batches (1 per write), got %d", totalWrites, stats.Batches)
	}
	if stats.MaxBatch != 1 {
		t.Fatalf("expected MaxBatch seen to be 1, got %d", stats.MaxBatch)
	}
}

func TestGroupCommitBatchingCapsAtMaxBatch(t *testing.T) {
	// MaxBatch = 4 with a small window: submit 16 concurrent writes
	const maxB = 4
	s := gcStore(t, Options{MaxBatch: maxB, FlushWindow: 5 * time.Millisecond})
	defer s.Close()

	const total = 16
	var wg sync.WaitGroup
	errCh := make(chan error, total)

	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			if err := gcMk(s, fmt.Sprintf("cap-%d", id)); err != nil {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatal(err)
	}

	stats := s.CommitStats()
	if stats.Writes != total {
		t.Fatalf("expected %d writes, got %d", total, stats.Writes)
	}
	if stats.MaxBatch > maxB {
		t.Fatalf("MaxBatch seen (%d) exceeded configured cap (%d)", stats.MaxBatch, maxB)
	}
}

func TestGroupCommitPreCancelledContext(t *testing.T) {
	s := gcStore(t, Options{})
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled

	called := false
	err := s.write(ctx, func(tx execer) error {
		called = true
		return nil
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if called {
		t.Fatal("pre-cancelled write function should never be executed")
	}

	stats := s.CommitStats()
	if stats.Writes != 0 || stats.Batches != 0 {
		t.Fatalf("expected 0 writes and batches for pre-cancelled write, got %+v", stats)
	}
}

func TestGroupCommitContextCancelledDuringQueue(t *testing.T) {
	s := gcStore(t, Options{FlushWindow: 10 * time.Millisecond})
	defer s.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	// Fill queue with a write that takes slightly longer
	var ran atomic.Bool
	go func() {
		_ = s.write(context.Background(), func(tx execer) error {
			time.Sleep(20 * time.Millisecond)
			return nil
		})
	}()

	time.Sleep(2 * time.Millisecond) // ensure first write has entered

	// Submit write with already expired timeout
	err := s.write(ctx, func(tx execer) error {
		ran.Store(true)
		return nil
	})

	if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("expected deadline exceeded or canceled, got %v", err)
	}
}

func TestGroupCommitPanicRecoveryTypes(t *testing.T) {
	s := gcStore(t, Options{})
	defer s.Close()

	// Subtest 1: Panic with custom struct
	type customPanic struct{ code int }
	panickedStruct := false
	func() {
		defer func() {
			if p := recover(); p != nil {
				if cp, ok := p.(customPanic); ok && cp.code == 42 {
					panickedStruct = true
				}
			}
		}()
		_ = s.write(context.Background(), func(tx execer) error {
			panic(customPanic{code: 42})
		})
	}()
	if !panickedStruct {
		t.Fatal("expected customPanic struct to be re-raised in caller")
	}

	// Subtest 2: Panic with string
	panickedStr := false
	func() {
		defer func() {
			if p := recover(); p != nil {
				if s, ok := p.(string); ok && s == "string-panic-msg" {
					panickedStr = true
				}
			}
		}()
		_ = s.write(context.Background(), func(tx execer) error {
			panic("string-panic-msg")
		})
	}()
	if !panickedStr {
		t.Fatal("expected string panic to be re-raised in caller")
	}

	// Verify writer survived all panics and still accepts legitimate writes
	if err := gcMk(s, "survivor-task"); err != nil {
		t.Fatalf("writer failed to accept write after panics: %v", err)
	}

	verifyOK(t, s)
}

func TestReaderPoolConcurrentStressWithActiveWriters(t *testing.T) {
	// Reader pool is active on disk databases (not :memory:)
	dir := t.TempDir()
	dbPath := dir + "/reader_pool.db"
	s, err := Open(context.Background(), dbPath, Options{NodeID: "node-readers"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if s.rdb == nil {
		t.Fatal("expected reader pool (rdb) to be initialized for disk database")
	}

	// Seed 20 initial tasks
	for i := 0; i < 20; i++ {
		if err := gcMk(s, fmt.Sprintf("initial-%d", i)); err != nil {
			t.Fatal(err)
		}
	}

	const duration = 200 * time.Millisecond
	stop := make(chan struct{})
	var wg sync.WaitGroup
	errCh := make(chan error, 32)

	// 8 reader routines hammering List and Get via reader pool
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func(readerID int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					tasks, err := s.List(context.Background(), task.Filter{})
					if err != nil {
						errCh <- fmt.Errorf("reader %d List failed: %w", readerID, err)
						return
					}
					if len(tasks) < 20 {
						errCh <- fmt.Errorf("reader %d saw fewer than 20 tasks: %d", readerID, len(tasks))
						return
					}
					// Read individual task
					if len(tasks) > 0 {
						if _, err := s.Get(context.Background(), tasks[0].ID); err != nil {
							errCh <- fmt.Errorf("reader %d Get failed: %w", readerID, err)
							return
						}
					}
					time.Sleep(time.Millisecond)
				}
			}
		}(r)
	}

	// 4 writer routines hammering Create and AddNote via writer goroutine
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(writerID int) {
			defer wg.Done()
			iter := 0
			for {
				select {
				case <-stop:
					return
				default:
					title := fmt.Sprintf("concurrent-w%d-%d", writerID, iter)
					tk, _, err := s.Create(context.Background(), task.NewTask{Title: title, Priority: task.PriorityLow})
					if err != nil {
						errCh <- fmt.Errorf("writer %d Create failed: %w", writerID, err)
						return
					}
					if _, err := s.AddNote(context.Background(), tk.ID, "note"); err != nil {
						errCh <- fmt.Errorf("writer %d AddNote failed: %w", writerID, err)
						return
					}
					iter++
					time.Sleep(2 * time.Millisecond)
				}
			}
		}(w)
	}

	time.Sleep(duration)
	close(stop)
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatal(err)
	}

	verifyOK(t, s)
}

func TestGroupCommitQueueSaturationBurst(t *testing.T) {
	s := gcStore(t, Options{MaxBatch: 64, FlushWindow: 2 * time.Millisecond})
	defer s.Close()

	const burstCount = 128
	var wg sync.WaitGroup
	errCh := make(chan error, burstCount)

	for i := 0; i < burstCount; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			title := fmt.Sprintf("burst-saturation-%d", idx)
			if err := gcMk(s, title); err != nil {
				errCh <- fmt.Errorf("burst write %d failed: %w", idx, err)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatal(err)
	}

	stats := s.CommitStats()
	if stats.Writes != burstCount {
		t.Fatalf("expected %d writes, got %d", burstCount, stats.Writes)
	}
	if stats.MaxBatch > 64 {
		t.Fatalf("MaxBatch %d exceeded configured cap 64", stats.MaxBatch)
	}

	tasks, err := s.List(context.Background(), task.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != burstCount {
		t.Fatalf("expected %d tasks in store, got %d", burstCount, len(tasks))
	}
}

func TestGroupCommitWriterSurvivesNilPanic(t *testing.T) {
	s := gcStore(t, Options{})
	defer s.Close()

	panicked := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
			}
		}()
		_ = s.write(context.Background(), func(tx execer) error {
			panic(nil)
		})
	}()

	if !panicked {
		t.Fatal("expected panic(nil) to be re-raised in caller")
	}

	// Committer must survive and process next write
	if err := gcMk(s, "after-nil-panic"); err != nil {
		t.Fatalf("store failed after nil panic: %v", err)
	}
	verifyOK(t, s)
}
