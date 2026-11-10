//go:build unix

package sqlstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
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
