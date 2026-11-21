package sqlstore

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"offgrid/internal/task"
)

func TestGroupCommitExtremeBurstConcurrency(t *testing.T) {
	ctx := context.Background()
	s := open(t, newDB(t), Options{NodeID: "n-burst", MaxBatch: 32, FlushWindow: 2 * time.Millisecond})
	defer s.Close()

	const workers = 64
	const opsPerWorker = 5
	var wg sync.WaitGroup

	var (
		validAcked   sync.Map
		invalidCount sync.Map
	)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(workerID*1000 + 42)))
			for i := 0; i < opsPerWorker; i++ {
				// Randomize between valid write, invalid priority write, and normal task
				choice := rng.Intn(4)
				switch choice {
				case 0, 1, 2:
					// Valid task
					title := fmt.Sprintf("burst-w%d-%d", workerID, i)
					prio := task.PriorityLow
					if i%2 == 0 {
						prio = task.PriorityMedium
					}
					tk, _, err := s.Create(ctx, task.NewTask{Title: title, Priority: prio})
					if err != nil {
						t.Errorf("worker %d op %d Create failed: %v", workerID, i, err)
						return
					}
					validAcked.Store(tk.ID, title)
				case 3:
					// Deliberately invalid task priority
					badTitle := fmt.Sprintf("invalid-w%d-%d", workerID, i)
					_, _, err := s.Create(ctx, task.NewTask{Title: badTitle, Priority: "critical"})
					if err == nil {
						t.Errorf("worker %d expected error for invalid priority, got nil", workerID)
						return
					}
					invalidCount.Store(badTitle, true)
				}
			}
		}(w)
	}
	wg.Wait()

	// Verify database integrity
	verifyOK(t, s)

	// Verify that all valid tasks were committed and retrieved
	tasks, err := s.List(ctx, task.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	taskMap := map[string]bool{}
	for _, tk := range tasks {
		taskMap[tk.ID] = true
	}

	validCount := 0
	validAcked.Range(func(key, value any) bool {
		id := key.(string)
		title := value.(string)
		validCount++
		if !taskMap[id] {
			t.Fatalf("acknowledged task %s (%s) was missing from store List", id, title)
		}
		return true
	})

	stats := s.CommitStats()
	if stats.Writes != int64(validCount) {
		t.Fatalf("CommitStats.Writes mismatch: have %d, expected %d", stats.Writes, validCount)
	}
}

func TestStmtPoolReopenStoreStress(t *testing.T) {
	ctx := context.Background()
	dbPath := newDB(t)

	// Round 1: Open, prepare statements, exercise pool
	s1, err := Open(ctx, dbPath, Options{NodeID: "n1", FlushWindow: 0})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 20; i++ {
		_, _, err := s1.Create(ctx, task.NewTask{Title: fmt.Sprintf("task-round1-%d", i), Priority: task.PriorityLow})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("s1.Close failed: %v", err)
	}

	// Round 2: Reopen same database, ensure pool rebuilds and operates cleanly
	s2, err := Open(ctx, dbPath, Options{NodeID: "n1", FlushWindow: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	for i := 0; i < 20; i++ {
		_, _, err := s2.Create(ctx, task.NewTask{Title: fmt.Sprintf("task-round2-%d", i), Priority: task.PriorityMedium})
		if err != nil {
			t.Fatal(err)
		}
	}

	allTasks, err := s2.List(ctx, task.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(allTasks) != 40 {
		t.Fatalf("expected 40 tasks across two reopen cycles, got %d", len(allTasks))
	}
	verifyOK(t, s2)
}

func TestReaderPoolWALSnapshotIsolation(t *testing.T) {
	// Active only on disk databases where s.rdb is initialized
	dir := t.TempDir()
	dbPath := dir + "/wal_isolation.db"

	s, err := Open(context.Background(), dbPath, Options{NodeID: "node-wal"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if s.rdb == nil {
		t.Fatal("expected reader pool to be initialized for disk database")
	}

	ctx := context.Background()

	// Initial task
	tk1, _, err := s.Create(ctx, task.NewTask{Title: "committed-init", Priority: task.PriorityLow})
	if err != nil {
		t.Fatal(err)
	}

	// Read via reader pool
	tasks, err := s.List(ctx, task.Filter{})
	if err != nil || len(tasks) != 1 || tasks[0].ID != tk1.ID {
		t.Fatalf("reader pool failed to read initial task: %v, tasks=%+v", err, tasks)
	}

	// Direct check on reader connection: verify uncommitted transactions are isolated
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	// Modify inside writer's open transaction without committing
	_, err = tx.ExecContext(ctx, "INSERT INTO meta (key, value) VALUES ('isolation_check', 'uncommitted_val')")
	if err != nil {
		t.Fatal(err)
	}

	// Query from reader connection: must NOT see 'isolation_check'
	var rVal string
	rErr := s.rdb.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'isolation_check'").Scan(&rVal)
	if rErr == nil {
		t.Fatal("reader pool observed uncommitted write; WAL isolation violated")
	}

	// Commit writer transaction
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Now reader must see committed value
	if err := s.rdb.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'isolation_check'").Scan(&rVal); err != nil || rVal != "uncommitted_val" {
		t.Fatalf("reader pool failed to observe committed value: val=%q err=%v", rVal, err)
	}
}

func TestGroupCommitContextTimeoutDuringBatchCollection(t *testing.T) {
	// Set a window of 50ms so the committer is waiting for more requests
	s := gcStore(t, Options{FlushWindow: 50 * time.Millisecond, MaxBatch: 16})
	defer s.Close()

	// Submit a request with a 2ms timeout that expires while the 50ms collection window is active
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Millisecond)
	defer cancel()

	err := s.write(ctx, func(tx execer) error {
		return nil
	})

	if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context timeout or cancellation, got %v", err)
	}

	// Wait for collection window to finish and verify store accepts subsequent writes cleanly
	time.Sleep(60 * time.Millisecond)
	if err := gcMk(s, "healthy-after-timeout"); err != nil {
		t.Fatalf("store failed to accept write after timeout: %v", err)
	}
}
