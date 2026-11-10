package main

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"offgrid/internal/store/sqlstore"
	"offgrid/internal/syncer"
)

// Day 35 review finding: `sync summary` (and once/run) ignored the error from
// computing the state hash and printed an empty one with exit 0. The hash is what
// people compare between two machines to see that they converged: two failures
// would print two empty strings that look equal. A hash that cannot be computed
// must be an error.
func TestSyncSummaryFailsWhenTheStateHashCannotBeComputed(t *testing.T) {
	t.Setenv("OFFGRID_DB", "")
	t.Setenv("OFFGRID_NODE", "")
	db := filepath.Join(t.TempDir(), "tasks.db")
	if code, _, e := runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"a"}`); code != 0 {
		t.Fatalf("exec: %d %s", code, e)
	}
	raw, err := sql.Open("sqlite3", db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`ALTER TABLE tasks RENAME COLUMN title TO title_x`); err != nil {
		t.Fatalf("could not break the view: %v", err)
	}
	raw.Close()
	code, out, errOut := runCLI(t, "", "sync", "summary", "-db", db)
	if code == 0 {
		t.Fatalf("exit 0 with a broken view; stdout:\n%s", out)
	}
	if !strings.Contains(errOut, "state hash") {
		t.Errorf("stderr should say the state hash failed: %q", errOut)
	}
	if strings.Contains(out, "state hash: \n") || strings.Contains(out, "hash:        \n") {
		t.Errorf("printed an empty hash:\n%s", out)
	}
}

func TestSyncOnceFailsWhenTheStateHashCannotBeComputed(t *testing.T) {
	t.Setenv("OFFGRID_DB", "")
	t.Setenv("OFFGRID_NODE", "")
	key := []byte("0123456789abcdef-test-key")
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, key, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	peer, err := sqlstore.Open(ctx, filepath.Join(t.TempDir(), "peer.db"), sqlstore.Options{NodeID: "peer"})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	srv := httptest.NewServer(syncer.Handler(peer, syncer.ServerOptions{Key: key}))
	defer srv.Close()

	db := filepath.Join(t.TempDir(), "mine.db")
	if code, _, e := runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"a"}`); code != 0 {
		t.Fatalf("exec: %d %s", code, e)
	}
	raw, _ := sql.Open("sqlite3", db)
	if _, err := raw.Exec(`ALTER TABLE tasks RENAME COLUMN title TO title_x`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	// The peer is empty, so there is nothing to apply: the sync itself succeeds and only the hash fails.
	code, out, errOut := runCLI(t, "", "sync", "once", "-db", db, "-peer", srv.URL, "-key-file", keyFile, "-timeout", "10s")
	if code == 0 {
		t.Fatalf("exit 0 although the state hash could not be computed:\n%s", out)
	}
	if !strings.Contains(errOut, "state hash") {
		t.Errorf("stderr should name the state hash: %q", errOut)
	}
	if strings.Contains(out, "state hash:") {
		t.Errorf("printed a state hash line:\n%s", out)
	}
}
