package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	ts      INTEGER NOT NULL,                  -- unix nanoseconds; a PLACEHOLDER until the hybrid logical clock (Day 15)
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
}

// ErrNewerSchema means the database was written by a newer Offgrid than this
// one. Opening it anyway could corrupt it, so the store refuses.
var ErrNewerSchema = errors.New("database was created by a newer version of Offgrid")

// migrate brings db up to the latest version in list.
func migrate(ctx context.Context, db *sql.DB, list []migration) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		name    TEXT NOT NULL
	)`); err != nil {
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

// applyMigration runs one migration and its bookkeeping row in one transaction:
// either both happen or neither does.
func applyMigration(ctx context.Context, db *sql.DB, m migration) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
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
