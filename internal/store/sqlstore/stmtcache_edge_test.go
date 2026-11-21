package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"testing"
)

func TestStmtCacheCapacityBoundary(t *testing.T) {
	ctx := context.Background()
	db := open(t, newDB(t), Options{NodeID: "n"}).db
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	c := newStmtCache(tx, nil)

	// Step 1: Fill cache exactly up to maxCachedStmts (64)
	for i := 0; i < maxCachedStmts; i++ {
		q := fmt.Sprintf("SELECT %d", i)
		var val int
		if err := c.QueryRowContext(ctx, q).Scan(&val); err != nil {
			t.Fatalf("query %d failed: %v", i, err)
		}
		if val != i {
			t.Fatalf("query %d got %d, want %d", i, val, i)
		}
	}
	if len(c.m) != maxCachedStmts {
		t.Fatalf("expected cache size %d, got %d", maxCachedStmts, len(c.m))
	}

	// Step 2: Run queries beyond capacity
	for i := maxCachedStmts; i < maxCachedStmts+25; i++ {
		q := fmt.Sprintf("SELECT %d", i)
		var val int
		if err := c.QueryRowContext(ctx, q).Scan(&val); err != nil {
			t.Fatalf("query beyond capacity %d failed: %v", i, err)
		}
		if val != i {
			t.Fatalf("query beyond capacity %d got %d, want %d", i, val, i)
		}
		if len(c.m) != maxCachedStmts {
			t.Fatalf("cache grew beyond limit to %d on query %d", len(c.m), i)
		}
	}

	// Step 3: Verify previously cached statements still work and stay cached
	for i := 0; i < 5; i++ {
		q := fmt.Sprintf("SELECT %d", i)
		var val int
		if err := c.QueryRowContext(ctx, q).Scan(&val); err != nil {
			t.Fatalf("re-querying cached %d failed: %v", i, err)
		}
		if val != i {
			t.Fatalf("re-querying cached %d got %d, want %d", i, val, i)
		}
	}
	if len(c.m) != maxCachedStmts {
		t.Fatalf("cache size changed after re-queries: got %d, want %d", len(c.m), maxCachedStmts)
	}
}

func TestStmtCacheContextCancellation(t *testing.T) {
	ctx := context.Background()
	db := open(t, newDB(t), Options{NodeID: "n"}).db
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	c := newStmtCache(tx, nil)

	// Pre-cancel context
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()

	// ExecContext with canceled context
	if _, err := c.ExecContext(canceledCtx, "SELECT 1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled on ExecContext, got %v", err)
	}

	// QueryContext with canceled context
	if _, err := c.QueryContext(canceledCtx, "SELECT 1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled on QueryContext, got %v", err)
	}

	// QueryRowContext with canceled context
	var v int
	if err := c.QueryRowContext(canceledCtx, "SELECT 1").Scan(&v); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled on QueryRowContext, got %v", err)
	}
}

func TestStmtCacheMalformedQueryFallback(t *testing.T) {
	ctx := context.Background()
	db := open(t, newDB(t), Options{NodeID: "n"}).db
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	c := newStmtCache(tx, nil)

	badQuery := "SYNTAX ERROR NOT A VALID SQL STATEMENT"

	// Executing a malformed query should fail cleanly and not cache
	if _, err := c.ExecContext(ctx, badQuery); err == nil {
		t.Fatal("expected error on malformed ExecContext, got nil")
	}
	if len(c.m) != 0 {
		t.Fatalf("malformed query should not be cached, cache len: %d", len(c.m))
	}

	// Querying a malformed query should fail cleanly and not cache
	if _, err := c.QueryContext(ctx, badQuery); err == nil {
		t.Fatal("expected error on malformed QueryContext, got nil")
	}
	if len(c.m) != 0 {
		t.Fatalf("malformed query should not be cached, cache len: %d", len(c.m))
	}

	// QueryRow on malformed query should fail during Scan
	var n int
	if err := c.QueryRowContext(ctx, badQuery).Scan(&n); err == nil {
		t.Fatal("expected error on malformed QueryRowContext, got nil")
	}
	if len(c.m) != 0 {
		t.Fatalf("malformed query should not be cached, cache len: %d", len(c.m))
	}

	// Valid data query afterwards should succeed and be cached
	if _, err := c.ExecContext(ctx, "CREATE TABLE t_ok (x INT)"); err != nil {
		t.Fatalf("valid create table failed: %v", err)
	}
	// DDL like CREATE TABLE is not cached (by design), but INSERT is cached
	if _, err := c.ExecContext(ctx, "INSERT INTO t_ok (x) VALUES (?)", 42); err != nil {
		t.Fatalf("valid insert failed after malformed queries: %v", err)
	}
	if len(c.m) != 1 {
		t.Fatalf("expected 1 cached query (the insert), got %d", len(c.m))
	}
}

func TestStmtCacheTransactionLifecycleRollbackAndCommit(t *testing.T) {
	ctx := context.Background()
	db := open(t, newDB(t), Options{NodeID: "n"}).db

	// Subtest 1: Rollback discards mutations made via cached statements
	{
		tx1, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		c1 := cached(tx1)
		if _, err := c1.ExecContext(ctx, "CREATE TABLE t_life (val TEXT)"); err != nil {
			t.Fatal(err)
		}
		if _, err := c1.ExecContext(ctx, "INSERT INTO t_life (val) VALUES (?)", "uncommitted"); err != nil {
			t.Fatal(err)
		}
		_ = tx1.Rollback()

		// New transaction: table should not exist
		txCheck, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		var count int
		if err := txCheck.QueryRowContext(ctx, "SELECT COUNT(*) FROM t_life").Scan(&count); err == nil {
			_ = txCheck.Rollback()
			t.Fatal("table t_life should not exist after rollback")
		}
		_ = txCheck.Rollback()
	}

	// Subtest 2: Calling cached statements on completed transaction fails with ErrTxDone
	{
		tx2, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		c2 := cached(tx2).(*stmtCache)
		if _, err := c2.ExecContext(ctx, "SELECT 1"); err != nil {
			t.Fatal(err)
		}
		if err := tx2.Commit(); err != nil {
			t.Fatal(err)
		}

		// Cached statement execution after commit fails because the Tx closed its statements
		if _, err := c2.ExecContext(ctx, "SELECT 1"); err == nil {
			t.Fatal("expected error on cached query after commit, got nil")
		}

		// Uncached statement execution after commit fails with sql.ErrTxDone
		if _, err := c2.ExecContext(ctx, "SELECT 2"); !errors.Is(err, sql.ErrTxDone) {
			t.Fatalf("expected sql.ErrTxDone on uncached query after commit, got %v", err)
		}
	}
}

func TestStmtCacheConcurrentRowsIteration(t *testing.T) {
	ctx := context.Background()
	db := open(t, newDB(t), Options{NodeID: "n"}).db
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	c := cached(tx)
	if _, err := c.ExecContext(ctx, "CREATE TABLE t_concur (id INT, label TEXT)"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if _, err := c.ExecContext(ctx, "INSERT INTO t_concur (id, label) VALUES (?, ?)", i, fmt.Sprintf("lbl-%d", i)); err != nil {
			t.Fatal(err)
		}
	}

	// Open first cursor
	rows1, err := c.QueryContext(ctx, "SELECT id, label FROM t_concur WHERE id < ? ORDER BY id", 10)
	if err != nil {
		t.Fatal(err)
	}
	defer rows1.Close()

	// Open second cursor with same statement while rows1 is open
	rows2, err := c.QueryContext(ctx, "SELECT id, label FROM t_concur WHERE id >= ? ORDER BY id", 10)
	if err != nil {
		t.Fatal(err)
	}
	defer rows2.Close()

	count1, count2 := 0, 0
	for rows1.Next() {
		var id int
		var lbl string
		if err := rows1.Scan(&id, &lbl); err != nil {
			t.Fatal(err)
		}
		count1++
	}
	for rows2.Next() {
		var id int
		var lbl string
		if err := rows2.Scan(&id, &lbl); err != nil {
			t.Fatal(err)
		}
		count2++
	}

	if count1 != 10 || count2 != 10 {
		t.Fatalf("expected 10 and 10 rows, got count1=%d count2=%d", count1, count2)
	}
}

func TestStmtCacheParameterTypesAndNulls(t *testing.T) {
	ctx := context.Background()
	db := open(t, newDB(t), Options{NodeID: "n"}).db
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	c := cached(tx)
	createSQL := `CREATE TABLE t_types (
		id INT PRIMARY KEY,
		str TEXT,
		null_str TEXT,
		big_int INTEGER,
		blob_val BLOB,
		f_val REAL
	)`
	if _, err := c.ExecContext(ctx, createSQL); err != nil {
		t.Fatal(err)
	}

	insertSQL := `INSERT INTO t_types (id, str, null_str, big_int, blob_val, f_val) VALUES (?, ?, ?, ?, ?, ?)`

	blobData := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0xFF}
	maxInt := int64(math.MaxInt64)
	floatVal := 3.1415926535

	if _, err := c.ExecContext(ctx, insertSQL, 1, "hello", nil, maxInt, blobData, floatVal); err != nil {
		t.Fatalf("insert failed: %v", err)
	}

	selectSQL := `SELECT str, null_str, big_int, blob_val, f_val FROM t_types WHERE id = ?`
	var (
		gotStr     string
		gotNullStr sql.NullString
		gotBigInt  int64
		gotBlob    []byte
		gotFloat   float64
	)
	if err := c.QueryRowContext(ctx, selectSQL, 1).Scan(&gotStr, &gotNullStr, &gotBigInt, &gotBlob, &gotFloat); err != nil {
		t.Fatalf("select failed: %v", err)
	}

	if gotStr != "hello" || gotNullStr.Valid || gotBigInt != maxInt || gotFloat != floatVal {
		t.Fatalf("unexpected values read: str=%q, nullStr=%v, bigInt=%d, float=%f", gotStr, gotNullStr, gotBigInt, gotFloat)
	}
	if string(gotBlob) != string(blobData) {
		t.Fatalf("blob mismatch: got %x, want %x", gotBlob, blobData)
	}
}

func TestCachedHelperIdempotenceAndTypes(t *testing.T) {
	ctx := context.Background()
	db := open(t, newDB(t), Options{NodeID: "n"}).db

	// 1. Passing *sql.DB returns the exact same *sql.DB
	if res := cached(db); res != db {
		t.Fatal("cached(*sql.DB) must return original db instance")
	}

	// 2. Passing *sql.Tx returns *stmtCache
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	c1 := cached(tx)
	if _, ok := c1.(*stmtCache); !ok {
		t.Fatalf("cached(*sql.Tx) must return *stmtCache, got %T", c1)
	}

	// 3. Passing *stmtCache to cached() returns unchanged *stmtCache (idempotent)
	c2 := cached(c1)
	if c2 != c1 {
		t.Fatal("cached(cached(tx)) must be idempotent")
	}
}
