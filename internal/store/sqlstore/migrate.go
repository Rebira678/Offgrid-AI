package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Migrations change the SOURCE OF TRUTH: the ops log and the small meta table.
// They are applied in order, each in its own transaction, and recorded in
// schema_migrations. Never edit a migration that has shipped; add a new one.
//
// The materialized tasks view is NOT migrated. It is derived data: when its
// shape changes, bump projectionVersion and the store rebuilds it from ops.
// That is a large part of why event sourcing is pleasant to evolve: the
// irreplaceable data (ops) rarely changes shape, and everything else can be
// thrown away and recomputed.
type migration struct {
	Version int
	Name    string
	SQL     string
}

var migrations = []migration{
	{1, "create the append-only ops log", `
CREATE TABLE ops (
	pos     INTEGER PRIMARY KEY AUTOINCREMENT, -- local log position: the order ops were recorded here
	node_id TEXT    NOT NULL,                  -- which replica created the op
	seq     INTEGER NOT NULL CHECK (seq >= 1), -- that replica's own counter: 1, 2, 3... with no gaps
	ts      INTEGER NOT NULL,                  -- unix nanoseconds. Wall part of the hybrid logical clock since migration 3 (a plain wall reading before it)
	kind    TEXT    NOT NULL,
	payload TEXT    NOT NULL CHECK (json_valid(payload)),
	UNIQUE (node_id, seq)                      -- an op's identity; makes applying the same op twice detectable
);
CREATE TRIGGER ops_append_only_update BEFORE UPDATE ON ops
BEGIN SELECT RAISE(ABORT, 'ops is append-only: updates are not allowed'); END;
CREATE TRIGGER ops_append_only_delete BEFORE DELETE ON ops
BEGIN SELECT RAISE(ABORT, 'ops is append-only: deletes are not allowed'); END;
`},
	{2, "create the meta table", `
CREATE TABLE meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
) WITHOUT ROWID;
`},
	{3, "hybrid logical clock: a counter beside ts, and an index to find the newest", `
-- From this migration on, ts is the wall part of a hybrid logical clock timestamp
-- (nanoseconds) and counter is its logical part, so (ts, counter) is the op's
-- timestamp. Ops written before it have counter 0, which is a valid
-- timestamp: ts was already a wall reading. Nothing is rewritten, so the log
-- stays append-only; the DEFAULT only fills the new column for old rows.
ALTER TABLE ops ADD COLUMN counter INTEGER NOT NULL DEFAULT 0 CHECK (counter >= 0 AND counter <= 4294967295);
-- On start-up the store restores its clock from the newest timestamp.
CREATE INDEX ops_hlc ON ops (ts, counter);
`},
}

// ErrNewerSchema means the database was written by a newer Offgrid than this
// one. Opening it anyway could corrupt it, so the store refuses.
var ErrNewerSchema = errors.New("database was created by a newer version of Offgrid")

// migrate brings db up to the latest version in list.
func migrate(ctx context.Context, db *sql.DB, list []migration) error {
	if err := createSchemaMigrations(ctx, db); err != nil {
		return fmt.Errorf("sqlstore: create schema_migrations: %w", err)
	}
	var current int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("sqlstore: read schema version: %w", err)
	}
	latest := 0
	if len(list) > 0 {
		latest = list[len(list)-1].Version
	}
	if current > latest {
		return fmt.Errorf("%w (database is at version %d, this program knows up to %d)", ErrNewerSchema, current, latest)
	}
	for _, m := range list {
		if m.Version <= current {
			continue
		}
		if err := applyMigration(ctx, db, m); err != nil {
			return fmt.Errorf("sqlstore: migration %d (%s): %w", m.Version, m.Name, err)
		}
	}
	return nil
}

func createSchemaMigrations(ctx context.Context, db *sql.DB) error {
	var err error
	for i := 0; i < 20; i++ {
		_, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			name    TEXT NOT NULL
		)`)
		if err == nil {
			return nil
		}
		if !strings.Contains(err.Error(), "database is locked") && !strings.Contains(err.Error(), "busy") {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(10*(i+1)) * time.Millisecond):
		}
	}
	return err
}

// applyMigration runs one migration and its bookkeeping row in one transaction:
// either both happen or neither does.
func applyMigration(ctx context.Context, db *sql.DB, m migration) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	// Double-check inside the write transaction: another process might have applied
	// this migration while we waited for the immediate lock.
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, m.Version).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return tx.Rollback()
	}
	if _, err = tx.ExecContext(ctx, m.SQL); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, name) VALUES (?, ?)`, m.Version, m.Name); err != nil {
		return err
	}
	return tx.Commit()
}

// schemaVersion reports the version the database is at.
func schemaVersion(ctx context.Context, x execer) (int, error) {
	var v int
	err := x.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v)
	return v, err
}
