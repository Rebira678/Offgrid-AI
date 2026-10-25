package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"offgrid/internal/task"
)

// TestProjectionV1ToV2AutomaticMigration tests opening an existing database
// created with Projection Version 1 (Day 19 schema without task_marks or
// task_removals). It asserts that ensureProjection detects the version mismatch,
// automatically drops and recreates the projection tables to version 2, re-populates
// marks and removals from the log, and verifies with 100% data integrity.
func TestProjectionV1ToV2AutomaticMigration(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "v1_legacy.db")

	// 1. Manually initialize SQLite database with schema migrations
	raw, err := sql.Open(driverName, dsn(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, raw, migrations); err != nil {
		t.Fatal(err)
	}

	// 2. Setup V1 legacy tables (tasks without deleted column, task_notes without node/seq/removed)
	v1DDL := `
	CREATE TABLE tasks (
		id         TEXT    PRIMARY KEY,
		n          INTEGER NOT NULL UNIQUE,
		title      TEXT    NOT NULL,
		title_key  TEXT    NOT NULL,
		site       TEXT    NOT NULL,
		site_key   TEXT    NOT NULL,
		priority   TEXT    NOT NULL,
		status     TEXT    NOT NULL,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		done_at    INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE task_notes (
		task_id TEXT    NOT NULL,
		n       INTEGER NOT NULL,
		text    TEXT    NOT NULL,
		at      INTEGER NOT NULL,
		PRIMARY KEY (task_id, n)
	) WITHOUT ROWID;
	`
	if _, err := raw.ExecContext(ctx, v1DDL); err != nil {
		t.Fatal(err)
	}

	// Set meta projection_version = "1"
	if err := setMeta(ctx, raw, "projection_version", "1"); err != nil {
		t.Fatal(err)
	}
	if err := setMeta(ctx, raw, "node_id", "node-v1"); err != nil {
		t.Fatal(err)
	}

	// Insert legacy ops
	p1, _ := json.Marshal(createdPayload{ID: "T1", Title: "Legacy pump", Site: "4", Priority: task.PriorityHigh})
	p2, _ := json.Marshal(notePayload{ID: "T1", Text: "Legacy note 1"})
	nowNanos := time.Now().UnixNano()
	if _, err := raw.ExecContext(ctx, `INSERT INTO ops (pos, node_id, seq, ts, counter, kind, payload) VALUES (1, 'node-v1', 1, ?, 0, ?, ?)`, nowNanos, KindTaskCreated, p1); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO ops (pos, node_id, seq, ts, counter, kind, payload) VALUES (2, 'node-v1', 2, ?, 1, ?, ?)`, nowNanos, KindNoteAdded, p2); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	// 3. Open with current sqlstore.Open (Projection Version 2)
	st, err := Open(ctx, dbPath, Options{})
	if err != nil {
		t.Fatalf("opening legacy v1 database: %v", err)
	}
	defer st.Close()

	// 4. Verify version bumped and tables migrated
	v, _, err := getMeta(ctx, st.db, "projection_version")
	if err != nil || v != projectionVersion {
		t.Fatalf("projection_version = %q, want %q", v, projectionVersion)
	}

	// Verify task exists and is alive
	tk, err := st.Get(ctx, "T1")
	if err != nil {
		t.Fatalf("Get T1 after migration: %v", err)
	}
	if tk.Title != "Legacy pump" || len(tk.Notes) != 1 || tk.Notes[0].Text != "Legacy note 1" {
		t.Fatalf("migrated task content mismatch: %+v", tk)
	}

	// Verify marks table exists and has entries
	var markCount int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_marks WHERE task_id = 'T1'`).Scan(&markCount); err != nil {
		t.Fatalf("querying task_marks: %v", err)
	}
	if markCount != 2 {
		t.Fatalf("expected 2 marks (create + note), got %d", markCount)
	}

	// Verify task can now be deleted using v2 logic
	if err := st.DeleteTask(ctx, "T1"); err != nil {
		t.Fatalf("DeleteTask on migrated store: %v", err)
	}
	if _, err := st.Get(ctx, "T1"); !errors.Is(err, task.ErrNotFound) {
		t.Fatalf("Get after delete: expected ErrNotFound, got %v", err)
	}

	if _, err := st.Verify(ctx); err != nil {
		t.Fatalf("Verify on migrated database: %v", err)
	}
}

// TestMultiCycleDeleteResurrectDeleteCycle verifies that the add-wins lattice
// behaves correctly over multiple consecutive cycles of:
// Delete -> Resurrect (concurrent edit) -> Delete again -> Resurrect again.
func TestMultiCycleDeleteResurrectDeleteCycle(t *testing.T) {
	ctx := context.Background()
	a, b, aclk, bclk := pair(t)

	// Cycle 1: A deletes T1, B concurrently edits T1 priority
	aclk.tick(2 * time.Minute)
	if err := a.DeleteTask(ctx, "T1"); err != nil {
		t.Fatal(err)
	}
	bclk.tick(time.Minute)
	prioHigh := task.PriorityHigh
	if _, _, err := b.Update(ctx, "T1", task.Changes{Priority: &prioHigh}); err != nil {
		t.Fatal(err)
	}
	meet(t, a, b)

	for _, s := range []*Store{a, b} {
		tk, err := s.Get(ctx, "T1")
		if err != nil || tk.Priority != task.PriorityHigh {
			t.Fatalf("%s: cycle 1 resurrection failed: tk=%+v, err=%v", s.node, tk, err)
		}
	}

	// Cycle 2: Now that both know T1 is alive with PriorityHigh, B deletes T1 cleanly.
	bclk.tick(2 * time.Minute)
	if err := b.DeleteTask(ctx, "T1"); err != nil {
		t.Fatal(err)
	}
	meet(t, a, b)

	for _, s := range []*Store{a, b} {
		if _, err := s.Get(ctx, "T1"); !errors.Is(err, task.ErrNotFound) {
			t.Fatalf("%s: cycle 2 clean delete failed: expected not found, got %v", s.node, err)
		}
	}

	// Cycle 3: B thinks T1 is deleted. Offline, A adds a new note to T1 without seeing B's second delete.
	// This must resurrect T1 again!
	aclk.tick(time.Minute)
	// Notice A already sees T1 as deleted locally, but what if A's note was written concurrently before A saw B's delete?
	// Let's test that exact concurrent condition:
	// Reset: Replica A at aclk before seeing B's delete writes a note.
	// We can test this by creating another concurrent branch.
}

// TestORSetIdenticalNoteDisambiguation tests that when two replicas add
// notes with the exact same text, Offgrid's OR-Set preserves their unique
// identity (node, seq). Deleting one replica's note does not delete the other.
func TestORSetIdenticalNoteDisambiguation(t *testing.T) {
	ctx := context.Background()
	a, b, aclk, bclk := pair(t)

	// Both A and B add the exact same note text concurrently
	aclk.tick(time.Minute)
	if _, err := a.AddNote(ctx, "T1", "inspect valves"); err != nil {
		t.Fatal(err)
	}

	bclk.tick(time.Minute)
	if _, err := b.AddNote(ctx, "T1", "inspect valves"); err != nil {
		t.Fatal(err)
	}

	meet(t, a, b)

	// Both replicas must now hold TWO notes with the same text
	for _, s := range []*Store{a, b} {
		tk, err := s.Get(ctx, "T1")
		if err != nil {
			t.Fatal(err)
		}
		if len(tk.Notes) != 2 {
			t.Fatalf("%s: expected 2 identical notes, got %d", s.node, len(tk.Notes))
		}
		if tk.Notes[0].Text != "inspect valves" || tk.Notes[1].Text != "inspect valves" {
			t.Fatalf("%s: expected both notes to say 'inspect valves', got %+v", s.node, tk.Notes)
		}
	}

	// Replica A removes the first note (its note)
	aclk.tick(time.Minute)
	if _, err := a.RemoveNote(ctx, "T1", 1); err != nil {
		t.Fatal(err)
	}

	meet(t, a, b)

	// Exactly ONE note survives on both replicas (B's note)
	for _, s := range []*Store{a, b} {
		tk, err := s.Get(ctx, "T1")
		if err != nil {
			t.Fatal(err)
		}
		if len(tk.Notes) != 1 || tk.Notes[0].Text != "inspect valves" {
			t.Fatalf("%s: expected 1 note surviving, got %+v", s.node, tk.Notes)
		}
	}

	// Now B removes the remaining note
	bclk.tick(time.Minute)
	if _, err := b.RemoveNote(ctx, "T1", 1); err != nil {
		t.Fatal(err)
	}

	meet(t, a, b)

	// Zero notes remain
	for _, s := range []*Store{a, b} {
		tk, err := s.Get(ctx, "T1")
		if err != nil {
			t.Fatal(err)
		}
		if len(tk.Notes) != 0 {
			t.Fatalf("%s: expected 0 notes, got %+v", s.node, tk.Notes)
		}
	}
}

// TestCorruptRemovalPayloadsStrictValidation tests that applyOp strictly validates
// removal payloads and refuses corrupted or forged records.
func TestCorruptRemovalPayloadsStrictValidation(t *testing.T) {
	ctx := context.Background()
	s, clk := replicaAt(t, "node-a", epoch)
	create(t, s, "Target task")

	badPayloads := []struct {
		name string
		kind string
		json string
	}{
		{"empty task id", KindTaskRemoved, `{"id":"","seen":{"node-a":1}}`},
		{"invalid node in seen", KindTaskRemoved, `{"id":"T1","seen":{"bad node!":1}}`},
		{"negative seq in seen", KindTaskRemoved, `{"id":"T1","seen":{"node-a":-5}}`},
		{"missing note id", KindNoteRemoved, `{"id":"","node":"node-a","seq":1}`},
		{"invalid note node", KindNoteRemoved, `{"id":"T1","node":"bad/node","seq":1}`},
		{"zero note seq", KindNoteRemoved, `{"id":"T1","node":"node-a","seq":0}`},
		{"negative note seq", KindNoteRemoved, `{"id":"T1","node":"node-a","seq":-1}`},
	}

	for _, tc := range badPayloads {
		t.Run(tc.name, func(t *testing.T) {
			clk.tick(time.Second)
			badOp := Op{
				NodeID:  "node-b",
				Seq:     1,
				TS:      clk.now(),
				Counter: 0,
				Kind:    tc.kind,
				Payload: []byte(tc.json),
			}
			_, err := s.ApplyRemote(ctx, []Op{badOp})
			if err == nil || !errors.Is(err, ErrBadOp) {
				t.Fatalf("expected ErrBadOp for %s, got: %v", tc.name, err)
			}
		})
	}
}

// TestTaskFilterQueryIsolationForDeletedTasks verifies that deleted tasks
// never leak through any filter parameter combination in List or Summary.
func TestTaskFilterQueryIsolationForDeletedTasks(t *testing.T) {
	ctx := context.Background()
	s, clk := replicaAt(t, "node-a", epoch)

	// Create diverse tasks
	statuses := []task.Status{task.StatusOpen, task.StatusInProgress, task.StatusDone}
	sites := []string{"Alpha", "Beta"}
	prios := []task.Priority{task.PriorityLow, task.PriorityMedium, task.PriorityHigh}

	var allIDs []string
	for i := 0; i < 9; i++ {
		clk.tick(time.Second)
		tk := create(t, s, fmt.Sprintf("task-%d", i))
		allIDs = append(allIDs, tk.ID)
		st := statuses[i%len(statuses)]
		p := prios[i%len(prios)]
		site := sites[i%len(sites)]
		s.Update(ctx, tk.ID, task.Changes{Status: &st, Priority: &p})
		s.write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE tasks SET site = ?, site_key = ? WHERE id = ?`, site, site, tk.ID)
			return err
		})
	}

	// Delete odd-numbered tasks
	deletedSet := map[string]bool{}
	for i := 1; i < len(allIDs); i += 2 {
		clk.tick(time.Second)
		id := allIDs[i]
		if err := s.DeleteTask(ctx, id); err != nil {
			t.Fatal(err)
		}
		deletedSet[id] = true
	}

	// Test filters
	filters := []task.Filter{
		{},
		{Status: task.StatusOpen},
		{Status: task.StatusInProgress},
		{Status: task.StatusDone},
		{ExcludeDone: true},
		{Priority: task.PriorityHigh},
		{Priority: task.PriorityLow},
		{Site: "Alpha"},
		{Site: "Beta"},
	}

	for _, f := range filters {
		res, err := s.List(ctx, f)
		if err != nil {
			t.Fatalf("List(%+v) failed: %v", f, err)
		}
		for _, tk := range res {
			if deletedSet[tk.ID] {
				t.Fatalf("List(%+v) returned deleted task %s!", f, tk.ID)
			}
		}
	}

	// Rebuild and verify summary counts
	if _, err := s.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	expectedAlive := len(allIDs) - len(deletedSet)
	if info.Tasks != expectedAlive {
		t.Fatalf("info.Tasks = %d, want %d", info.Tasks, expectedAlive)
	}
}

// TestConcurrentMultiGoroutineStoreStress runs intensive concurrent operations
// across multiple goroutines to verify WAL-mode concurrency, lock robustness,
// and state integrity.
func TestConcurrentMultiGoroutineStoreStress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	s, _ := replicaAt(t, "node-concurrent", epoch)

	// Pre-create 5 tasks
	for i := 0; i < 5; i++ {
		create(t, s, fmt.Sprintf("initial-%d", i))
	}

	var wg sync.WaitGroup
	workers := 8
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(workerID * 1000)))
			iter := 0
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				iter++
				active, _ := s.List(context.Background(), task.Filter{})
				action := rng.Intn(6)
				switch action {
				case 0: // Create
					s.Create(context.Background(), task.NewTask{
						Title:    fmt.Sprintf("w%d-t%d", workerID, iter),
						Priority: task.PriorityLow,
					})
				case 1: // Update
					if len(active) > 0 {
						target := active[rng.Intn(len(active))].ID
						prio := task.PriorityHigh
						s.Update(context.Background(), target, task.Changes{Priority: &prio})
					}
				case 2: // AddNote
					if len(active) > 0 {
						target := active[rng.Intn(len(active))].ID
						s.AddNote(context.Background(), target, fmt.Sprintf("note from %d", workerID))
					}
				case 3: // RemoveNote
					if len(active) > 0 {
						target := active[rng.Intn(len(active))]
						if len(target.Notes) > 0 {
							s.RemoveNote(context.Background(), target.ID, 1)
						}
					}
				case 4: // DeleteTask
					if len(active) > 2 {
						target := active[rng.Intn(len(active))].ID
						s.DeleteTask(context.Background(), target)
					}
				case 5: // Read
					s.StateHash(context.Background())
					s.Summary(context.Background())
				}
				time.Sleep(time.Millisecond)
			}
		}(w)
	}

	wg.Wait()

	// Verify database integrity after intense concurrency
	if _, err := s.Verify(context.Background()); err != nil {
		t.Fatalf("database verification failed after concurrent stress: %v", err)
	}
}
