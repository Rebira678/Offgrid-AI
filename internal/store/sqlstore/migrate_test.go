package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

func rawDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open(driverName, dsn(newDB(t)))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func TestMigrationsRunInOrderOnceAndAreRecorded(t *testing.T) {
	ctx := context.Background()
	db := rawDB(t)
	if err := migrate(ctx, db, migrations); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"ops", "meta", "schema_migrations"} {
		if !tableExists(t, db, tbl) {
			t.Errorf("table %s missing", tbl)
		}
	}
	if v, _ := schemaVersion(ctx, db); v != len(migrations) {
		t.Errorf("version = %d, want %d", v, len(migrations))
	}
	// Running again does nothing and does not fail.
	if err := migrate(ctx, db, migrations); err != nil {
		t.Fatalf("second run: %v", err)
	}
	var rows int
	db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&rows)
	if rows != len(migrations) {
		t.Errorf("%d bookkeeping rows, want %d", rows, len(migrations))
	}
}

func TestAMigrationAddedLaterIsAppliedWithoutTouchingEarlierData(t *testing.T) {
	ctx := context.Background()
	db := rawDB(t)
	if err := migrate(ctx, db, migrations); err != nil {
		t.Fatal(err)
	}
	db.Exec(`INSERT INTO ops (node_id, seq, ts, kind, payload) VALUES ('a', 1, 0, 'task.created', '{}')`)

	next := append(append([]migration(nil), migrations...), migration{3, "add a column", `ALTER TABLE ops ADD COLUMN note TEXT`})
	if err := migrate(ctx, db, next); err != nil {
		t.Fatal(err)
	}
	if v, _ := schemaVersion(ctx, db); v != 3 {
		t.Errorf("version = %d", v)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM ops`).Scan(&n)
	if n != 1 {
		t.Errorf("existing ops changed: %d", n)
	}
}

func TestAFailingMigrationIsRolledBackCompletely(t *testing.T) {
	ctx := context.Background()
	db := rawDB(t)
	bad := append(append([]migration(nil), migrations...), migration{3, "half done", `
		CREATE TABLE should_not_exist (x INTEGER);
		THIS IS NOT SQL;
	`})
	err := migrate(ctx, db, bad)
	if err == nil || !strings.Contains(err.Error(), "migration 3") {
		t.Fatalf("err = %v, want it to name migration 3", err)
	}
	if tableExists(t, db, "should_not_exist") {
		t.Error("the half-applied migration left a table behind")
	}
	if v, _ := schemaVersion(ctx, db); v != 2 {
		t.Errorf("version = %d; migrations 1 and 2 were fine and must stay applied", v)
	}
	// Fixing the migration and running again works.
	good := append(append([]migration(nil), migrations...), migration{3, "fixed", `CREATE TABLE fine (x INTEGER)`})
	if err := migrate(ctx, db, good); err != nil {
		t.Fatal(err)
	}
	if !tableExists(t, db, "fine") {
		t.Error("the fixed migration did not run")
	}
}

func TestADatabaseFromAFutureVersionIsRefused(t *testing.T) {
	ctx := context.Background()
	path := newDB(t)
	s := open(t, path, Options{NodeID: "a"})
	s.db.Exec(`INSERT INTO schema_migrations (version, name) VALUES (99, 'from the future')`)
	s.Close()
	_, err := Open(ctx, path, Options{})
	if !errors.Is(err, ErrNewerSchema) || !strings.Contains(err.Error(), "99") {
		t.Fatalf("err = %v, want ErrNewerSchema mentioning 99", err)
	}
}

func TestMigrationVersionsAreContiguousAndNamed(t *testing.T) {
	for i, m := range migrations {
		if m.Version != i+1 {
			t.Errorf("migration %d has version %d; versions must be 1, 2, 3... with no gaps", i, m.Version)
		}
		if m.Name == "" || strings.TrimSpace(m.SQL) == "" {
			t.Errorf("migration %d is incomplete", m.Version)
		}
	}
}

func TestDSNEscapesAwkwardPaths(t *testing.T) {
	got := dsn("/tmp/a?b#c%d.db")
	if !strings.HasPrefix(got, "file:/tmp/a%3fb%23c%25d.db?") {
		t.Errorf("dsn = %s", got)
	}
	for _, want := range []string{"_journal_mode=WAL", "_synchronous=FULL", "_busy_timeout=5000", "_txlock=immediate"} {
		if !strings.Contains(got, want) {
			t.Errorf("dsn lacks %s", want)
		}
	}
	if !strings.HasPrefix(dsn(":memory:"), "file::memory:?") {
		t.Errorf("memory dsn = %s", dsn(":memory:"))
	}
}

func TestTheDatabaseIsInWALModeWithFullSynchronous(t *testing.T) {
	s, _ := freshStore(t)
	var mode string
	s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode)
	if mode != "wal" {
		t.Errorf("journal_mode = %q", mode)
	}
	var sync int
	s.db.QueryRow(`PRAGMA synchronous`).Scan(&sync)
	if sync != 2 {
		t.Errorf("synchronous = %d, want 2 (FULL)", sync)
	}
}
