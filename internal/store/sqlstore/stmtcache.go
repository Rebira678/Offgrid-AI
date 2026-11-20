package sqlstore

import (
	"context"
	"database/sql"
)

// A statement cache for the paths that apply many ops in one transaction.
//
// database/sql prepares a statement for every ExecContext/QueryContext on a transaction and
// throws it away after. Profiling the suite (docs/day-45.md) put sqlite3_prepare_v2 at about a
// fifth of all CPU in Rebuild and ApplyRemote, where one op runs the same handful of statements
// (select task, insert mark, count removals, insert note) over and over. Preparing each of them
// once per transaction and reusing it removes most of that, with no change to what runs.
//
// The cache lives and dies with one transaction: statements prepared on a Tx are closed by
// database/sql when it commits or rolls back, so nothing leaks and nothing is shared between
// transactions or goroutines. It is not safe for concurrent use, like the Tx it wraps.

const maxCachedStmts = 64 // a bound, so a caller that builds its SQL text per call cannot grow it forever

type stmtCache struct {
	tx *sql.Tx
	m  map[string]*sql.Stmt
}

func newStmtCache(tx *sql.Tx) *stmtCache {
	return &stmtCache{tx: tx, m: make(map[string]*sql.Stmt, 16)}
}

// cached wraps x in a statement cache when it is a transaction, and returns it unchanged
// otherwise (a *sql.DB runs each statement on its own connection, where a cache would be wrong).
func cached(x execer) execer {
	if tx, ok := x.(*sql.Tx); ok {
		return newStmtCache(tx)
	}
	return x
}

// stmt returns the prepared statement for q, preparing it on first use. It returns nil when q
// cannot be cached (the cache is full, or preparing failed); the caller then runs q the plain
// way, which reports the same error a plain call would.
func (c *stmtCache) stmt(ctx context.Context, q string) *sql.Stmt {
	if s, ok := c.m[q]; ok {
		return s
	}
	if len(c.m) >= maxCachedStmts {
		return nil
	}
	s, err := c.tx.PrepareContext(ctx, q)
	if err != nil {
		return nil
	}
	c.m[q] = s
	return s
}

func (c *stmtCache) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	if s := c.stmt(ctx, q); s != nil {
		return s.ExecContext(ctx, args...)
	}
	return c.tx.ExecContext(ctx, q, args...)
}

func (c *stmtCache) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	if s := c.stmt(ctx, q); s != nil {
		return s.QueryContext(ctx, args...)
	}
	return c.tx.QueryContext(ctx, q, args...)
}

func (c *stmtCache) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	if s := c.stmt(ctx, q); s != nil {
		return s.QueryRowContext(ctx, args...)
	}
	return c.tx.QueryRowContext(ctx, q, args...)
}
