package sqlstore

import (
	"context"
	"database/sql"
	"strings"
	"sync"
)

// Prepared statements.
//
// database/sql prepares a statement for every ExecContext/QueryContext on a transaction and
// throws it away after. Profiling (docs/day-45.md) put sqlite3_prepare_v2 at about a fifth of all
// CPU in Rebuild and ApplyRemote, and a single write runs eight or ten different statements.
//
// Two layers remove that:
//
//   - stmtCache lives for one transaction: each distinct statement is prepared (or taken from the
//     pool) once and reused for every op in the transaction. Statements bound to a Tx are closed
//     by database/sql when it ends, so nothing leaks. Not safe for concurrent use, like the Tx.
//   - stmtPool lives for the store: statements prepared on the *sql.DB, which database/sql keeps
//     per connection and lets a transaction borrow (Tx.StmtContext) without preparing again. The
//     pool is filled by what transactions actually ran (they note a miss; the writer prepares the
//     noted statements once no transaction holds the single connection), so there is no list of
//     SQL text to keep in step with the code. Text that is built per call cannot flood it: the
//     pool is bounded.
//
// Nothing about what runs changes, only whether it is parsed again.

const (
	maxCachedStmts = 64  // per transaction
	maxPooledStmts = 128 // per store
)

type stmtPool struct {
	db    *sql.DB
	mu    sync.RWMutex
	ready map[string]*sql.Stmt
	want  map[string]struct{}
}

func newStmtPool(db *sql.DB) *stmtPool {
	return &stmtPool{db: db, ready: map[string]*sql.Stmt{}, want: map[string]struct{}{}}
}

func (p *stmtPool) lookup(q string) *sql.Stmt {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.ready[q]
}

// note records that a transaction had to prepare q itself, so fill should prepare it for the pool.
func (p *stmtPool) note(q string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if _, ok := p.ready[q]; !ok && len(p.ready)+len(p.want) < maxPooledStmts {
		p.want[q] = struct{}{}
	}
	p.mu.Unlock()
}

// fill prepares the noted statements on the *sql.DB. It must run when no transaction holds the
// connection (the store has one, so a Prepare would wait for it): the writer calls it between
// batches. A statement that fails to prepare is dropped; the transaction that ran it already
// reported any real error.
func (p *stmtPool) fill(ctx context.Context) {
	if p == nil {
		return
	}
	p.mu.Lock()
	todo := make([]string, 0, len(p.want))
	for q := range p.want {
		todo = append(todo, q)
	}
	p.want = map[string]struct{}{}
	p.mu.Unlock()
	for _, q := range todo {
		st, err := p.db.PrepareContext(ctx, q)
		if err != nil {
			continue
		}
		p.mu.Lock()
		if _, dup := p.ready[q]; dup {
			st.Close()
		} else {
			p.ready[q] = st
		}
		p.mu.Unlock()
	}
}

func (p *stmtPool) close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for q, st := range p.ready {
		st.Close()
		delete(p.ready, q)
	}
}

type stmtCache struct {
	tx   *sql.Tx
	pool *stmtPool // may be nil
	m    map[string]*sql.Stmt
}

func newStmtCache(tx *sql.Tx, pool *stmtPool) *stmtCache {
	return &stmtCache{tx: tx, pool: pool, m: make(map[string]*sql.Stmt, 16)}
}

// cached wraps x in a per-transaction statement cache when it is a transaction, and returns it
// unchanged otherwise (a *sql.DB runs each statement on its own connection, where a cache would
// be wrong). The commit writer builds its own cache with the store pool (newStmtCache).
func cached(x execer) execer {
	if tx, ok := x.(*sql.Tx); ok {
		return newStmtCache(tx, nil)
	}
	return x
}

// stmt returns the statement for q bound to this transaction, preparing it on first use. It
// returns nil when q cannot be cached (the cache is full, or preparing failed); the caller then
// runs q the plain way, which reports the same error a plain call would.
func (c *stmtCache) stmt(ctx context.Context, q string) *sql.Stmt {
	if s, ok := c.m[q]; ok {
		return s
	}
	if len(c.m) >= maxCachedStmts || !cacheable(q) {
		return nil
	}
	var s *sql.Stmt
	if ps := c.pool.lookup(q); ps != nil {
		s = c.tx.StmtContext(ctx, ps)
	} else {
		var err error
		if s, err = c.tx.PrepareContext(ctx, q); err != nil {
			return nil
		}
		c.pool.note(q)
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

// cacheable says whether q is a single data statement. A prepared statement runs only the first
// statement of its text, while Exec on a transaction runs them all (the projection's DROP and CREATE
// script relies on that), and schema changes should not be pinned in a cache anyway. Anything
// else, and any text containing a ';' before its end, takes the plain path. A ';' inside a string
// literal only loses the speed-up, never correctness.
func cacheable(q string) bool {
	q = strings.TrimSpace(q)
	q = strings.TrimRight(q, "; \t\r\n")
	if strings.Contains(q, ";") {
		return false
	}
	end := strings.IndexAny(q, " \t\r\n(")
	if end < 0 {
		end = len(q)
	}
	switch strings.ToUpper(q[:end]) {
	case "SELECT", "INSERT", "UPDATE", "DELETE", "REPLACE", "WITH":
		return true
	}
	return false
}
