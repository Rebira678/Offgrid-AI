package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"offgrid/internal/task"
)

// TestConcurrentWritesAndRevocationRace stresses concurrent write attempts
// on a worker node while the admin revokes it and concurrent syncs occur.
func TestConcurrentWritesAndRevocationRace(t *testing.T) {
	ctx := context.Background()
	e := trio(t)

	kept := create(t, e.b, "baseline task")
	pull(t, e.a, e.b, 50)

	var wg sync.WaitGroup
	errCh := make(chan error, 32)
	stop := make(chan struct{})

	// 4 workers hammering writes on B
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for round := 0; ; round++ {
				select {
				case <-stop:
					return
				default:
					_, _, err := e.b.Create(ctx, task.NewTask{
						Title:    fmt.Sprintf("worker-%d-task-%d", workerID, round),
						Priority: task.PriorityLow,
					})
					if err != nil {
						if errors.Is(err, ErrRevoked) || errors.Is(err, sql.ErrTxDone) {
							// Expected once revocation is active
							return
						}
						errCh <- err
						return
					}
					time.Sleep(time.Millisecond)
				}
			}
		}(i)
	}

	// Wait briefly, then Admin revokes B
	time.Sleep(10 * time.Millisecond)
	e.clkA.tick(time.Minute)
	if _, err := e.a.Revoke(ctx, e.idb.ID()); err != nil {
		t.Fatalf("Revoke failed: %v", err)
	}

	// Admin pushes revocation to B
	pull(t, e.b, e.a, 50)
	close(stop)
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("unexpected concurrent write error: %v", err)
	}

	// At this point, B knows it is revoked. All subsequent writes MUST fail with ErrRevoked.
	_, _, err := e.b.Create(ctx, task.NewTask{Title: "should-fail", Priority: task.PriorityHigh})
	if !errors.Is(err, ErrRevoked) {
		t.Fatalf("expected ErrRevoked after race, got %v", err)
	}

	// Kept task must remain safe
	mustVerify(t, e.b)
	mustVerify(t, e.a)
	_ = kept
}

// TestMassiveVoidSetRebuildAndSyncStress exercises sync and rebuild under 100 void tasks
// and 100 inert dependent notes and suggestions.
func TestMassiveVoidSetRebuildAndSyncStress(t *testing.T) {
	ctx := context.Background()
	e := trio(t)

	// Admin revokes B
	e.clkA.tick(time.Minute)
	if _, err := e.a.Revoke(ctx, e.idb.ID()); err != nil {
		t.Fatal(err)
	}

	// B (before receiving revocation) creates 100 tasks (all void past cutoff)
	var voidTasks []task.Task
	for i := 1; i <= 100; i++ {
		e.clkB.tick(time.Second)
		tk := create(t, e.b, fmt.Sprintf("void-task-%03d", i))
		voidTasks = append(voidTasks, tk)
	}

	// C pulls from B before hearing of the revocation
	pull(t, e.c, e.b, 200)

	// C adds dependent notes and suggestions to all 100 void tasks
	for i, tk := range voidTasks {
		if _, err := e.c.AddNote(ctx, tk.ID, fmt.Sprintf("dependent note %d", i)); err != nil {
			t.Fatalf("AddNote %d on void task: %v", i, err)
		}
		st := task.StatusDone
		if _, _, err := e.c.Suggest(ctx, task.Draft{
			Task:   tk.ID,
			Note:   fmt.Sprintf("dependent sug %d", i),
			By:     "tab",
			Key:    fmt.Sprintf("k-%d", i),
			Status: &st,
		}); err != nil {
			t.Fatalf("Suggest %d on void task: %v", i, err)
		}
	}

	// Broadcast revocation from A to C
	pull(t, e.c, e.a, 50)
	// Sync C's work to A
	pull(t, e.a, e.c, 300)

	// Ensure view on A and C contains 0 void tasks
	for _, s := range []*Store{e.a, e.c} {
		titles := viewTitles(t, s)
		if titles != "" {
			t.Fatalf("%s view has %q, want empty view", s.node, titles)
		}
		mustVerify(t, s)

		rep, err := s.Rebuild(ctx)
		if err != nil {
			t.Fatalf("%s rebuild failed: %v", s.node, err)
		}
		if rep.Tasks != 0 {
			t.Fatalf("%s rebuild tasks = %d, want 0", s.node, rep.Tasks)
		}
	}

	if hashOf(t, e.a) != hashOf(t, e.c) {
		t.Fatal("hash mismatch between A and C after massive void sync")
	}

	voids := voidOf(t, e.a)
	if len(voids) != 100 {
		t.Fatalf("void count = %d, want 100", len(voids))
	}
}

// TestSuggestionIsVoidWithCorruptJSONPayload ensures that if the database contains
// an unexpected payload shape for KindSuggestionMade, suggestionIsVoid returns (false, nil)
// without panicking.
func TestSuggestionIsVoidWithCorruptJSONPayload(t *testing.T) {
	ctx := context.Background()
	e := trio(t)

	// Insert an op with unexpected payload schema (valid JSON, but task is number or missing)
	err := e.a.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO ops (node_id, seq, ts, counter, kind, payload) VALUES (?, ?, ?, ?, ?, ?)`,
			"corrupt_node", 1, 1000, 1, KindSuggestionMade, `{"task": 12345, "invalid": true}`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	isVoid, err := suggestionIsVoid(ctx, e.a.db, "corrupt_node", 1)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if isVoid {
		t.Fatal("corrupt payload reported as void, want false")
	}
}

// FuzzTaskIsVoid fuzzes task IDs to ensure taskIsVoid never panics or errs on arbitrary input.
func FuzzTaskIsVoid(f *testing.F) {
	f.Add("")
	f.Add("T1.laptop")
	f.Add("T9999.pi")
	f.Add("T' OR 1=1 --")
	f.Add(`{"id":"injection"}`)
	f.Add("\x00\xff\xfe")

	ctx := context.Background()
	f.Fuzz(func(t *testing.T, id string) {
		s, err := Open(ctx, t.TempDir()+"/fuzz.db", Options{NodeID: "fuzz_node"})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()

		_, _ = taskIsVoid(ctx, s.db, id)
	})
}
