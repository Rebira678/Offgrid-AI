package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMustExistRefusesAMissingDatabaseAndCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.db")
	st, err := Open(context.Background(), path, Options{MustExist: true, NodeID: "node-x"})
	if st != nil {
		st.Close()
		t.Fatal("opened a database that does not exist")
	}
	if !errors.Is(err, ErrNoDatabase) {
		t.Fatalf("error = %v, want ErrNoDatabase", err)
	}
	if got := err.Error(); got != "no such database: "+path {
		t.Errorf("message %q does not name the path", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("created %d files", len(entries))
	}
}

func TestMustExistOpensAnExistingDatabaseAndKeepsItsNode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.db")
	st, err := Open(context.Background(), path, Options{NodeID: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	st, err = Open(context.Background(), path, Options{MustExist: true})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if st.NodeID() != "node-a" {
		t.Errorf("node = %q", st.NodeID())
	}
	// A wrong node name is still refused: MustExist does not weaken identity.
	if _, err := Open(context.Background(), path, Options{MustExist: true, NodeID: "node-b"}); !errors.Is(err, ErrNodeMismatch) {
		t.Errorf("error = %v, want ErrNodeMismatch", err)
	}
}

func TestMustExistOnAFolderIsNotReportedAsMissing(t *testing.T) {
	_, err := Open(context.Background(), t.TempDir(), Options{MustExist: true})
	if err == nil || errors.Is(err, ErrNoDatabase) {
		t.Errorf("error = %v, want a folder error that is not ErrNoDatabase", err)
	}
}

// The pre-check is not the only guard: the driver is told mode=rw, so SQLite
// itself refuses to create the file if it vanishes after the check. This pins
// the assumption that the driver honours that flag.
func TestDriverModeRWNeverCreates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "raced.db")
	db, err := sql.Open(driverName, dsn(path)+"&mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err == nil {
		t.Error("ping succeeded on a file that does not exist")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("the driver created the file")
	}
}
