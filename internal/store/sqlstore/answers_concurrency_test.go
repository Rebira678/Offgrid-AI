package sqlstore

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"offgrid/internal/task"
)

func TestConcurrentAnswerAndTaskModifications(t *testing.T) {
	ctx := context.Background()
	s, _ := replicaAt(t, "node-lidia", epoch)

	const workers = 10
	var wg sync.WaitGroup
	wg.Add(workers * 2)

	// Workers creating and updating tasks
	for i := 0; i < workers; i++ {
		workerID := i
		go func() {
			defer wg.Done()
			tk, _, err := s.Create(ctx, task.NewTask{
				Title:    fmt.Sprintf("Generator task from worker %d", workerID),
				Site:     fmt.Sprintf("site-%d", workerID),
				Priority: task.PriorityMedium,
			})
			if err != nil {
				t.Errorf("task create worker %d: %v", workerID, err)
				return
			}
			if _, err := s.AddNote(ctx, tk.ID, fmt.Sprintf("Note from worker %d", workerID)); err != nil {
				t.Errorf("add note worker %d: %v", workerID, err)
			}
		}()
	}

	// Workers recording answers concurrently
	for i := 0; i < workers; i++ {
		workerID := i
		go func() {
			defer wg.Done()
			key := fmt.Sprintf("k-conc-%d", workerID)
			rec, err := s.RecordAnswer(ctx, key, "telemetry check", fmt.Sprintf("All metrics normal %d", workerID), "qwen2.5:7b", "local")
			if err != nil || !rec {
				t.Errorf("record answer worker %d: rec=%v err=%v", workerID, rec, err)
			}
		}()
	}

	wg.Wait()

	// Verify all answers and tasks persisted cleanly
	answers, err := s.Answers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(answers) != workers {
		t.Fatalf("expected %d answers, found %d", workers, len(answers))
	}

	tasks, err := s.List(ctx, task.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != workers {
		t.Fatalf("expected %d tasks, found %d", workers, len(tasks))
	}

	// Rebuild and Verify must pass with 100% hash parity
	mustVerify(t, s)
	cleanHash := hashOf(t, s)
	if _, err := s.Rebuild(ctx); err != nil {
		t.Fatalf("rebuild failed: %v", err)
	}
	if rebuilt := hashOf(t, s); rebuilt != cleanHash {
		t.Fatalf("hash mismatch after rebuild: %s vs %s", rebuilt, cleanHash)
	}
}
