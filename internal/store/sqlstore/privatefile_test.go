//go:build unix

package sqlstore

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Day 35 review finding: a new database was created with the process's default
// mode (0644 under the usual umask), so on a shared or stolen machine every
// local user could read the tasks. A new database is now private.
func TestNewDatabaseAndItsSidecarsAreOwnerOnly(t *testing.T) {
	old := syscall.Umask(0o022) // the usual default, which is what made the bug
	defer syscall.Umask(old)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.db")
	s, err := Open(ctx, path, Options{NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS probe(x)`); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Logf("%s: %v (not created, fine)", filepath.Base(p), err)
			continue
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s has mode %04o: readable by other users", filepath.Base(p), fi.Mode().Perm())
		}
	}
	if fi, _ := os.Stat(path); fi == nil {
		t.Fatal("no database file")
	}
}

// An existing database keeps the mode its owner gave it: Offgrid does not
// change permissions on files it did not create.
func TestExistingDatabaseModeIsLeftAlone(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.db")
	s, err := Open(ctx, path, Options{NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path, Options{NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o644 {
		t.Errorf("an existing database's mode was changed to %04o", fi.Mode().Perm())
	}
}

func TestOpeningAnExistingEmptyFileKeepsItsMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.db")
	if err := os.WriteFile(path, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), path, Options{NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode changed to %04o", fi.Mode().Perm())
	}
}
