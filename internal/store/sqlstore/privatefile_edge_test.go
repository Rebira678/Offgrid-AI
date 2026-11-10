//go:build unix

package sqlstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"offgrid/internal/task"
)

func TestPrivateFileCancelledContextCreatesNoFile(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)

	t.Run("already canceled", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "canceled.db")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		s, err := Open(ctx, path, Options{NodeID: "n1"})
		if !errors.Is(err, context.Canceled) {
			if s != nil {
				s.Close()
			}
			t.Fatalf("expected context.Canceled, got store=%v err=%v", s, err)
		}
		if ents, rerr := os.ReadDir(dir); rerr != nil || len(ents) != 0 {
			t.Fatalf("canceled Open left files on disk: ents=%v err=%v", ents, rerr)
		}
	})

	t.Run("already expired deadline", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "expired.db")
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()

		s, err := Open(ctx, path, Options{NodeID: "n1"})
		if !errors.Is(err, context.DeadlineExceeded) {
			if s != nil {
				s.Close()
			}
			t.Fatalf("expected context.DeadlineExceeded, got store=%v err=%v", s, err)
		}
		if ents, rerr := os.ReadDir(dir); rerr != nil || len(ents) != 0 {
			t.Fatalf("expired Open left files on disk: ents=%v err=%v", ents, rerr)
		}
	})
}

func TestPrivateFileMemoryDoesNotCreateDiskFile(t *testing.T) {
	dir := t.TempDir()
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWD) }()

	s, err := Open(context.Background(), ":memory:", Options{NodeID: "mem"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if ents, err := os.ReadDir(dir); err != nil || len(ents) != 0 {
		t.Fatalf(":memory: created files in wd: ents=%v err=%v", ents, err)
	}
}

func TestPrivateFileLiteralFilePrefixAndSpecialChars(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)

	cases := []struct {
		name string
		rel  string
	}{
		{"literal_file_prefix", "file:tasks.db"},
		{"uri_metachars", "tasks?v=1#hash%20.db"},
		{"spaces_and_unicode", "field site αβ/tasks.db"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, tc.rel)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			s, err := Open(ctx, path, Options{NodeID: "n-special"})
			if err != nil {
				t.Fatalf("Open(%q): %v", tc.rel, err)
			}
			if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS probe(x)`); err != nil {
				s.Close()
				t.Fatal(err)
			}
			s.Close()

			for _, p := range []string{path, path + "-wal", path + "-shm"} {
				fi, err := os.Stat(p)
				if err != nil {
					continue
				}
				if fi.Mode().Perm()&0o077 != 0 {
					t.Errorf("%s has mode %04o; want owner-only", p, fi.Mode().Perm())
				}
			}
		})
	}
}

func TestPrivateFileMustExistNeverCreatesUnderUmask(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)

	dir := t.TempDir()
	path := filepath.Join(dir, "missing.db")
	s, err := Open(context.Background(), path, Options{MustExist: true})
	if !errors.Is(err, ErrNoDatabase) {
		if s != nil {
			s.Close()
		}
		t.Fatalf("want ErrNoDatabase, got %v", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("MustExist=true left files on disk: %v", ents)
	}
}

func TestPrivateFileConcurrentOpenCreatesOwnerOnly(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)

	dir := t.TempDir()
	path := filepath.Join(dir, "concurrent.db")

	const workers = 6
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			s, err := Open(context.Background(), path, Options{NodeID: "shared-node"})
			if err == nil {
				_ = s.Close()
			}
		}()
	}
	wg.Wait()

	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s mode %04o exposed group/other bits", filepath.Base(p), fi.Mode().Perm())
		}
	}
}

func TestPrivateFileDirectoryAndUnwritableParentErrors(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, "subdir")
	if err := os.Mkdir(subDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(context.Background(), subDir, Options{NodeID: "n1"}); err == nil {
		s.Close()
		t.Fatal("expected error opening a directory as database")
	}

	missingParent := filepath.Join(dir, "no-such-dir", "tasks.db")
	if s, err := Open(context.Background(), missingParent, Options{NodeID: "n1"}); err == nil {
		s.Close()
		t.Fatal("expected error opening db inside missing parent directory")
	}

	if os.Geteuid() != 0 {
		roDir := filepath.Join(dir, "readonly")
		if err := os.Mkdir(roDir, 0o500); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(roDir, 0o700)
		roDB := filepath.Join(roDir, "tasks.db")
		if s, err := Open(context.Background(), roDB, Options{NodeID: "n1"}); err == nil {
			s.Close()
			t.Fatal("expected error opening db inside read-only directory")
		}
		if ents, rerr := os.ReadDir(roDir); rerr != nil || len(ents) != 0 {
			t.Fatalf("read-only dir should have no created files, got %v (err=%v)", ents, rerr)
		}
	}
}

func TestPrivateFileDanglingAndValidSymlinks(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)

	dir := t.TempDir()
	missingTarget := filepath.Join(dir, "never_created.db")
	dangling := filepath.Join(dir, "dangling.db")
	if err := os.Symlink(missingTarget, dangling); err != nil {
		t.Skip("symlinks not supported:", err)
	}

	s, err := Open(context.Background(), dangling, Options{NodeID: "n1"})
	if err == nil || !strings.Contains(err.Error(), "dangling symlink") {
		if s != nil {
			s.Close()
		}
		t.Fatalf("expected dangling symlink refusal, got store=%v err=%v", s, err)
	}
	if _, statErr := os.Stat(missingTarget); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("dangling symlink Open created target file! statErr=%v", statErr)
	}

	// Symlink to an already-existing 0600 database continues to work cleanly.
	realDB := filepath.Join(dir, "real.db")
	sReal, err := Open(context.Background(), realDB, Options{NodeID: "real"})
	if err != nil {
		t.Fatal(err)
	}
	sReal.Close()
	validLink := filepath.Join(dir, "valid.db")
	if err := os.Symlink(realDB, validLink); err != nil {
		t.Fatal(err)
	}
	sLink, err := Open(context.Background(), validLink, Options{})
	if err != nil {
		t.Fatalf("opening valid symlink failed: %v", err)
	}
	sLink.Close()
}

func TestPrivateFileRefusesNamedPipeFIFOImmediately(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe.db")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip("mkfifo not supported:", err)
	}

	done := make(chan error, 1)
	go func() {
		s, err := Open(context.Background(), fifo, Options{MustExist: true})
		if s != nil {
			s.Close()
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("expected non-regular file refusal, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Open blocked on FIFO instead of failing cleanly")
	}
}

func TestPrivateFileCorruptNonSQLiteFilePreservesModeAndBytes(t *testing.T) {
	dir := t.TempDir()
	badDB := filepath.Join(dir, "corrupt.db")
	garbage := []byte("NOT A SQLITE DATABASE AT ALL")
	if err := os.WriteFile(badDB, garbage, 0o640); err != nil {
		t.Fatal(err)
	}

	for _, mustExist := range []bool{false, true} {
		s, err := Open(context.Background(), badDB, Options{MustExist: mustExist})
		if err == nil {
			s.Close()
			t.Fatalf("MustExist=%v: expected error opening non-sqlite file", mustExist)
		}
		fi, serr := os.Stat(badDB)
		if serr != nil {
			t.Fatal(serr)
		}
		if fi.Mode().Perm() != 0o640 {
			t.Errorf("MustExist=%v changed mode to %04o", mustExist, fi.Mode().Perm())
		}
		if got, _ := os.ReadFile(badDB); string(got) != string(garbage) {
			t.Errorf("MustExist=%v modified corrupt file bytes: %q", mustExist, got)
		}
	}
}

func TestRebuildSinglePassStateHashAndCountParity(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:", Options{NodeID: "rebuild-parity"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	t1, _, err := s.Create(ctx, task.NewTask{Title: "Inspect pump", Site: "A", Priority: task.PriorityHigh})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddNote(ctx, t1.ID, "note one"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddNote(ctx, t1.ID, "note two"); err != nil {
		t.Fatal(err)
	}
	t2, _, err := s.Create(ctx, task.NewTask{Title: "Temp task", Priority: task.PriorityLow})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTask(ctx, t2.ID); err != nil {
		t.Fatal(err)
	}

	hashBefore, err := s.StateHash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := s.Rebuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Tasks != 1 {
		t.Errorf("Rebuild Tasks = %d, want 1", rep.Tasks)
	}
	if rep.Hash != hashBefore {
		t.Errorf("Rebuild Hash mismatch: got %s, want %s", rep.Hash, hashBefore)
	}
}
