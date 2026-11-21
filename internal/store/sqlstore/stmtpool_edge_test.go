package sqlstore

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestStmtPoolNilReceiverSafety(t *testing.T) {
	var p *stmtPool

	// Calling lookup on nil pool returns nil without panicking
	if stmt := p.lookup("SELECT 1"); stmt != nil {
		t.Fatalf("expected nil statement from nil pool lookup, got %v", stmt)
	}

	// Calling note, fill, and close on nil pool should be safe no-ops
	p.note("SELECT 1")
	p.fill(context.Background())
	p.close()
}

func TestStmtPoolCapacityBoundaries(t *testing.T) {
	ctx := context.Background()
	db := open(t, newDB(t), Options{NodeID: "n"}).db
	pool := newStmtPool(db)
	defer pool.close()

	// Note up to maxPooledStmts (128)
	for i := 0; i < maxPooledStmts; i++ {
		q := fmt.Sprintf("SELECT %d", i)
		pool.note(q)
	}

	pool.mu.RLock()
	wantLen := len(pool.want)
	pool.mu.RUnlock()
	if wantLen != maxPooledStmts {
		t.Fatalf("expected %d wanted statements, got %d", maxPooledStmts, wantLen)
	}

	// Note beyond capacity: must be rejected / dropped
	for i := maxPooledStmts; i < maxPooledStmts+50; i++ {
		q := fmt.Sprintf("SELECT %d", i)
		pool.note(q)
	}

	pool.mu.RLock()
	wantLenAfter := len(pool.want)
	pool.mu.RUnlock()
	if wantLenAfter != maxPooledStmts {
		t.Fatalf("expected want set to remain capped at %d, got %d", maxPooledStmts, wantLenAfter)
	}

	// Fill the pool
	pool.fill(ctx)

	pool.mu.RLock()
	readyLen := len(pool.ready)
	pool.mu.RUnlock()
	if readyLen != maxPooledStmts {
		t.Fatalf("expected %d ready statements in pool, got %d", maxPooledStmts, readyLen)
	}

	// Try noting more statements when ready is full
	pool.note("SELECT 999999")
	pool.mu.RLock()
	newWantLen := len(pool.want)
	pool.mu.RUnlock()
	if newWantLen != 0 {
		t.Fatalf("pool is already full at %d ready stmts; should not accept new want, got %d", maxPooledStmts, newWantLen)
	}
}

func TestStmtPoolFillDropInvalidSQL(t *testing.T) {
	ctx := context.Background()
	db := open(t, newDB(t), Options{NodeID: "n"}).db
	pool := newStmtPool(db)
	defer pool.close()

	// Note valid and invalid queries
	pool.note("SELECT 1")
	pool.note("INVALID SQL SYNTAX BROKEN ???")
	pool.note("SELECT 2")

	pool.fill(ctx)

	if pool.lookup("SELECT 1") == nil {
		t.Fatal("SELECT 1 should be ready in pool")
	}
	if pool.lookup("SELECT 2") == nil {
		t.Fatal("SELECT 2 should be ready in pool")
	}
	if pool.lookup("INVALID SQL SYNTAX BROKEN ???") != nil {
		t.Fatal("broken query should not be ready in pool")
	}

	pool.mu.RLock()
	readyCount := len(pool.ready)
	pool.mu.RUnlock()
	if readyCount != 2 {
		t.Fatalf("expected 2 ready statements, got %d", readyCount)
	}
}

func TestStmtPoolDeduplicationAndIdempotence(t *testing.T) {
	ctx := context.Background()
	db := open(t, newDB(t), Options{NodeID: "n"}).db
	pool := newStmtPool(db)
	defer pool.close()

	// Note same statement multiple times
	for i := 0; i < 10; i++ {
		pool.note("SELECT 42")
	}
	pool.mu.RLock()
	if len(pool.want) != 1 {
		t.Fatalf("expected 1 item in want map, got %d", len(pool.want))
	}
	pool.mu.RUnlock()

	pool.fill(ctx)

	// Note statement that is already in ready map
	pool.note("SELECT 42")
	pool.mu.RLock()
	if len(pool.want) != 0 {
		t.Fatalf("already ready statement should not be re-added to want, got %d", len(pool.want))
	}
	pool.mu.RUnlock()

	// Calling fill again is idempotent
	pool.fill(ctx)
	if pool.lookup("SELECT 42") == nil {
		t.Fatal("SELECT 42 should still be ready")
	}

	// Close cleans up
	pool.close()
	if pool.lookup("SELECT 42") != nil {
		t.Fatal("lookup after close should return nil")
	}

	// Double close is safe
	pool.close()
}

func TestStmtPoolConcurrentAccess(t *testing.T) {
	ctx := context.Background()
	db := open(t, newDB(t), Options{NodeID: "n"}).db
	pool := newStmtPool(db)
	defer pool.close()

	var wg sync.WaitGroup
	// Concurrently note and lookup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				q := fmt.Sprintf("SELECT %d", (gid*50+i)%60)
				pool.note(q)
				_ = pool.lookup(q)
			}
		}(g)
	}
	wg.Wait()

	// Fill everything noted
	pool.fill(ctx)

	// Concurrently lookup after fill
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				q := fmt.Sprintf("SELECT %d", (gid*50+i)%60)
				_ = pool.lookup(q)
			}
		}(g)
	}
	wg.Wait()
}

func TestCacheableComprehensiveMatrix(t *testing.T) {
	tests := []struct {
		sql  string
		want bool
	}{
		// Valid data statements
		{"SELECT * FROM tasks", true},
		{"select 1", true},
		{"sElEcT 1", true},
		{"\n\t  SELECT 1  \r\n", true},
		{"SELECT 1;", true},
		{"SELECT 1; ; ;", true},
		{"INSERT INTO tasks (id) VALUES ('x')", true},
		{"insert into tasks values (1)", true},
		{"UPDATE tasks SET title = 'y'", true},
		{"DELETE FROM tasks WHERE id = 'z'", true},
		{"REPLACE INTO tasks (id) VALUES ('a')", true},
		{"WITH cte AS (SELECT 1) SELECT * FROM cte", true},
		{"WITH RECURSIVE cnt(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM cnt WHERE x<10) SELECT x FROM cnt", true},

		// Multi-statements (must NOT be cached)
		{"SELECT 1; SELECT 2", false},
		{"INSERT INTO a VALUES(1); DROP TABLE a", false},
		{"SELECT 1; \n SELECT 2;", false},

		// DDL & Administrative statements (must NOT be cached)
		{"CREATE TABLE foo (a INT)", false},
		{"DROP TABLE foo", false},
		{"ALTER TABLE foo ADD COLUMN b TEXT", false},
		{"CREATE INDEX idx_foo ON foo(a)", false},
		{"DROP INDEX idx_foo", false},
		{"PRAGMA foreign_keys = ON", false},
		{"PRAGMA user_version", false},
		{"VACUUM", false},
		{"ANALYZE", false},
		{"EXPLAIN QUERY PLAN SELECT 1", false},
		{"EXPLAIN SELECT 1", false},
		{"SAVEPOINT s1", false},
		{"RELEASE s1", false},
		{"ROLLBACK TO s1", false},
		{"BEGIN TRANSACTION", false},
		{"COMMIT", false},
		{"ROLLBACK", false},
		{"ATTACH DATABASE 'foo.db' AS aux", false},
		{"DETACH DATABASE aux", false},

		// Comments and edge inputs
		{"-- comment\nSELECT 1", false},
		{"/* block comment */ SELECT 1", false},
		{"", false},
		{"   ", false},
		{";", false},
		{";;;", false},
		{"   ;\n\t", false},
		{strings.Repeat(" ", 100), false},
	}

	for _, tt := range tests {
		got := cacheable(tt.sql)
		if got != tt.want {
			t.Errorf("cacheable(%q) = %v, want %v", tt.sql, got, tt.want)
		}
	}
}
