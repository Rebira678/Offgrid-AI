package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"offgrid/internal/task"
)

func TestEnsureProjectionMissingAnswersRebuilds(t *testing.T) {
	ctx := context.Background()
	s, _ := replicaAt(t, "node-lidia", epoch)
	create(t, s, "Solar Inverter Maintenance")

	if _, err := s.RecordAnswer(ctx, "k-mig-1", "Status?", "Operational", "qwen2.5:7b", "local"); err != nil {
		t.Fatal(err)
	}

	cleanHash := hashOf(t, s)

	// Simulate catastrophic corruption of projection: drop answers table directly
	if _, err := s.db.ExecContext(ctx, `DROP TABLE answers`); err != nil {
		t.Fatal(err)
	}

	// Re-run ensureProjection: it must detect present != 3 and rebuild the missing answers table from the log
	if err := s.ensureProjection(ctx); err != nil {
		t.Fatalf("ensureProjection failed after dropping answers table: %v", err)
	}

	// Verify answers table is recreated and populated
	ans, found, err := s.Answer(ctx, "k-mig-1")
	if err != nil || !found {
		t.Fatalf("answer not found after ensureProjection rebuild: found=%v err=%v", found, err)
	}
	if ans.Text != "Operational" {
		t.Fatalf("answer text mismatch after rebuild: %q", ans.Text)
	}

	rebuiltHash := hashOf(t, s)
	if rebuiltHash != cleanHash {
		t.Fatalf("hash mismatch after ensureProjection rebuild: %s vs %s", rebuiltHash, cleanHash)
	}
}

// TestProjectionV2ToV3AutomaticMigration tests opening an existing database
// created with Projection Version 2 (Day 20/21 schema with tasks, task_notes,
// task_marks, task_removals, but WITHOUT answers table). It asserts that
// opening the store automatically detects projection version "2" != "3",
// rebuilds the projection, creates the answers table, and functions seamlessly.
func TestProjectionV2ToV3AutomaticMigration(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "v2_legacy.db")

	raw, err := sql.Open(driverName, dsn(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, raw, migrations); err != nil {
		t.Fatal(err)
	}

	// Setup V2 projection tables (Day 20/21 schema: no answers table)
	v2DDL := `
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
		done_at    INTEGER NOT NULL DEFAULT 0,
		deleted    INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE task_notes (
		task_id TEXT    NOT NULL,
		n       INTEGER NOT NULL,
		text    TEXT    NOT NULL,
		at      INTEGER NOT NULL,
		node    TEXT    NOT NULL,
		seq     INTEGER NOT NULL,
		removed INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (task_id, n)
	) WITHOUT ROWID;
	CREATE TABLE task_marks (
		task_id TEXT    NOT NULL,
		node    TEXT    NOT NULL,
		seq     INTEGER NOT NULL,
		PRIMARY KEY (task_id, node, seq)
	) WITHOUT ROWID;
	CREATE TABLE task_removals (
		task_id TEXT    NOT NULL,
		node    TEXT    NOT NULL,
		seq     INTEGER NOT NULL,
		seen    TEXT    NOT NULL,
		PRIMARY KEY (task_id, node, seq)
	) WITHOUT ROWID;
	`
	if _, err := raw.ExecContext(ctx, v2DDL); err != nil {
		t.Fatal(err)
	}
	if err := setMeta(ctx, raw, "projection_version", "2"); err != nil {
		t.Fatal(err)
	}
	if err := setMeta(ctx, raw, "node_id", "node-v2"); err != nil {
		t.Fatal(err)
	}

	// Insert legacy v2 ops
	p1, _ := json.Marshal(createdPayload{ID: "T1", Title: "Fix wind turbine brake", Site: "Site-A", Priority: task.PriorityHigh})
	nowNanos := time.Now().UnixNano()
	if _, err := raw.ExecContext(ctx, `INSERT INTO ops (pos, node_id, seq, ts, counter, kind, payload) VALUES (1, 'node-v2', 1, ?, 0, ?, ?)`, nowNanos, KindTaskCreated, p1); err != nil {
		t.Fatal(err)
	}
	if err := setMeta(ctx, raw, "applied_pos", "1"); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	// Now open using Day 22 sqlstore.Open: must detect version "2" and upgrade to "3"
	store, err := Open(ctx, dbPath, Options{NodeID: "node-v2"})
	if err != nil {
		t.Fatalf("failed to open v2 database: %v", err)
	}
	defer store.Close()

	// Verify tasks are present
	tk, err := store.Get(ctx, "T1")
	if err != nil || tk.Title != "Fix wind turbine brake" {
		t.Fatalf("legacy task T1 missing or corrupted: %v, task=%+v", err, tk)
	}

	// Verify answers table now exists and can record new answers
	ok, err := store.RecordAnswer(ctx, "k-v2-mig", "What is the status of turbine?", "Turbine brake inspected and cleared.", "qwen2.5:7b", "local")
	if err != nil || !ok {
		t.Fatalf("failed to record answer in migrated database: ok=%v err=%v", ok, err)
	}

	ans, found, err := store.Answer(ctx, "k-v2-mig")
	if err != nil || !found || ans.Text != "Turbine brake inspected and cleared." {
		t.Fatalf("answer lookup failed in migrated database: found=%v ans=%+v err=%v", found, ans, err)
	}

	// Verify reports clean state
	rep, err := store.Verify(ctx)
	if err != nil || rep.LiveHash != rep.ReplayHash {
		t.Fatalf("verify failed after v2->v3 migration: err=%v rep=%+v", err, rep)
	}
}
