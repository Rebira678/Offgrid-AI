package sqlstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"offgrid/internal/task"
)

// TestTamperingDetectionOnQualifiedTaskIDs tests that direct manipulation of SQLite
// projection rows (tasks, task_notes) containing Day 24 qualified task IDs (e.g. T1.lidia)
// is rigorously caught by Verify() as ErrDiverged, and that Rebuild() restores exact state.
func TestTamperingDetectionOnQualifiedTaskIDs(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name      string
		tamperSQL string
	}{
		{
			name:      "modify title of qualified task directly",
			tamperSQL: `UPDATE tasks SET title = 'Hacked Title' WHERE id = 'T1.lidia'`,
		},
		{
			name:      "modify priority of qualified task directly",
			tamperSQL: `UPDATE tasks SET priority = 'urgent' WHERE id = 'T1.lidia'`,
		},
		{
			name:      "delete projection row of qualified task directly",
			tamperSQL: `DELETE FROM tasks WHERE id = 'T1.lidia'`,
		},
		{
			name:      "insert phantom unlogged task with qualified ID",
			tamperSQL: `INSERT INTO tasks (id, n, title, title_key, site, site_key, priority, status, created_at, updated_at) VALUES ('T999.lidia', 999, 'Rogue', 'rogue', '1', '1', 'low', 'open', 1000, 1000)`,
		},
		{
			name:      "insert phantom unlogged note referencing qualified task",
			tamperSQL: `INSERT INTO task_notes (task_id, n, text, at, node, seq) VALUES ('T1.lidia', 99, 'unlogged note', 1000, 'intruder', 1)`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, clk := replicaAt(t, "lidia", epoch)

			// Create a legitimate task (will get ID T1.lidia)
			_, created, err := s.Create(ctx, task.NewTask{Title: "Lidia Solar Pump", Priority: task.PriorityHigh, Site: "Site-A"})
			if err != nil || !created {
				t.Fatalf("Create: %v", err)
			}
			clk.tick(time.Minute)
			if _, err := s.AddNote(ctx, "T1.lidia", "Legitimate note from Lidia"); err != nil {
				t.Fatalf("AddNote: %v", err)
			}

			// Clean baseline
			mustVerify(t, s)
			hashBefore, err := s.StateHash(ctx)
			if err != nil {
				t.Fatalf("StateHash before tampering: %v", err)
			}

			// Apply tampering SQL directly to projection tables
			res, err := s.db.ExecContext(ctx, tc.tamperSQL)
			if err != nil {
				t.Fatalf("tampering exec failed: %v", err)
			}
			affected, _ := res.RowsAffected()
			if affected == 0 {
				t.Fatalf("tampering affected 0 rows")
			}

			// Verify must detect divergence
			_, err = s.Verify(ctx)
			if !errors.Is(err, ErrDiverged) {
				t.Fatalf("expected ErrDiverged, got: %v", err)
			}

			// Rebuild must recover exactly
			rep, err := s.Rebuild(ctx)
			if err != nil {
				t.Fatalf("Rebuild: %v", err)
			}
			if rep.Tasks != 1 {
				t.Fatalf("expected 1 task after rebuild, got %d", rep.Tasks)
			}

			// StateHash after rebuild must match hashBefore
			hashAfter, err := s.StateHash(ctx)
			if err != nil {
				t.Fatalf("StateHash after rebuild: %v", err)
			}
			if hashAfter != hashBefore {
				t.Fatalf("StateHash mismatch after rebuild: %s != %s", hashAfter, hashBefore)
			}

			// Rebuilt store must verify cleanly
			mustVerify(t, s)
		})
	}
}
