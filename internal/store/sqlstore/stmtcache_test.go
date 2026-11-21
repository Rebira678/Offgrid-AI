package sqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestStatementCacheGivesTheSameAnswersAsPlainCalls(t *testing.T) {
	ctx := context.Background()
	db := open(t, newDB(t), Options{NodeID: "n"}).db
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	c := cached(tx).(*stmtCache)

	if _, err := c.ExecContext(ctx, `CREATE TABLE tmp_cache (k TEXT, v INTEGER)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if _, err := c.ExecContext(ctx, `INSERT INTO tmp_cache (k, v) VALUES (?, ?)`, fmt.Sprint("k", i), i); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.m) != 1 {
		t.Fatalf("50 inserts of one statement must prepare it once (and the CREATE not at all), have %d", len(c.m))
	}
	var viaCache, plain int
	if err := c.QueryRowContext(ctx, `SELECT SUM(v) FROM tmp_cache WHERE v >= ?`, 10).Scan(&viaCache); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT SUM(v) FROM tmp_cache WHERE v >= ?`, 10).Scan(&plain); err != nil {
		t.Fatal(err)
	}
	if viaCache != plain || plain != 1225-45 {
		t.Fatalf("cache %d plain %d", viaCache, plain)
	}
	rows, err := c.QueryContext(ctx, `SELECT k FROM tmp_cache ORDER BY v LIMIT 3`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		got = append(got, k)
	}
	rows.Close()
	if fmt.Sprint(got) != "[k0 k1 k2]" {
		t.Fatalf("%v", got)
	}
	// A rows value left open, then the same statement used again: the way applyOp reads a task
	// and then writes a mark while other readers are open must keep working.
	r1, err := c.QueryContext(ctx, `SELECT v FROM tmp_cache ORDER BY v`)
	if err != nil {
		t.Fatal(err)
	}
	r1.Close()
	// Errors are the plain errors.
	if _, err := c.ExecContext(ctx, `INSERT INTO nowhere VALUES (1)`); err == nil {
		t.Fatal("a bad statement must fail")
	}
	if err := c.QueryRowContext(ctx, `SELECT nope FROM tmp_cache`).Scan(new(int)); err == nil {
		t.Fatal("a bad query must fail")
	}
}

func TestStatementCacheIsBoundedAndKeepsWorkingWhenFull(t *testing.T) {
	ctx := context.Background()
	db := open(t, newDB(t), Options{NodeID: "n"}).db
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	c := newStmtCache(tx, nil)
	for i := 0; i < maxCachedStmts+20; i++ {
		var got int
		if err := c.QueryRowContext(ctx, fmt.Sprintf(`SELECT %d`, i)).Scan(&got); err != nil || got != i {
			t.Fatalf("query %d: got %d, %v", i, got, err)
		}
	}
	if len(c.m) != maxCachedStmts {
		t.Fatalf("the cache grew to %d, bound is %d", len(c.m), maxCachedStmts)
	}
}

func TestCachedLeavesADatabaseHandleAlone(t *testing.T) {
	db := open(t, newDB(t), Options{NodeID: "n"}).db
	var x execer = db
	if cached(x) != x {
		t.Fatal("a *sql.DB must not be wrapped: each statement there runs on its own connection")
	}
	var _ execer = (*sql.Tx)(nil)
}

func TestOnlySingleDataStatementsAreCached(t *testing.T) {
	for q, want := range map[string]bool{
		"SELECT 1":                                true,
		"  select x FROM t WHERE a = ?  ":         true,
		"INSERT INTO t VALUES (1);":               true,
		"UPDATE t SET a = 1":                      true,
		"DELETE FROM t":                           true,
		"WITH x AS (SELECT 1) SELECT * FROM x":    true,
		"INSERT INTO t(a) VALUES (1)\n":           true,
		"DROP TABLE a; DROP TABLE b":              false,
		"CREATE TABLE t (a)":                      false,
		"PRAGMA user_version":                     false,
		"SAVEPOINT w":                             false,
		"INSERT INTO t VALUES (1); DELETE FROM t": false,
		"": false,
	} {
		if got := cacheable(q); got != want {
			t.Errorf("cacheable(%q) = %v, want %v", q, got, want)
		}
	}
}

// A prepared statement runs only the first statement of its text; the projection reset is two
// scripts of many. Through the cache they must still all run.
func TestAMultiStatementScriptStillRunsEveryStatementThroughTheCache(t *testing.T) {
	ctx := context.Background()
	db := open(t, newDB(t), Options{NodeID: "n"}).db
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	c := newStmtCache(tx, nil)
	if _, err := c.ExecContext(ctx, `CREATE TABLE s1 (a); CREATE TABLE s2 (a); INSERT INTO s2 VALUES (7)`); err != nil {
		t.Fatal(err)
	}
	var v int
	if err := c.QueryRowContext(ctx, `SELECT a FROM s2`).Scan(&v); err != nil || v != 7 {
		t.Fatalf("the later statements of the script did not run: %d %v", v, err)
	}
}
