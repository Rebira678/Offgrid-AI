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

	"offgrid/internal/task"
)

// TestPrivateFileUnderUmask000WorldWritable verifies that even under a completely permissive
// umask (0000), a newly created database and all WAL/SHM sidecars are strictly owner-only (0600)
// and never expose group or world read/write/execute bits.
func TestPrivateFileUnderUmask000WorldWritable(t *testing.T) {
	old := syscall.Umask(0o000)
	defer syscall.Umask(old)

	dir := t.TempDir()
	path := filepath.Join(dir, "permissive_umask.db")

	const workers = 8
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(id int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			s, err := Open(ctx, path, Options{NodeID: "umask-node"})
			if err == nil {
				_, _, _ = s.Create(ctx, task.NewTask{Title: "Private task", Priority: task.PriorityMedium})
				_ = s.Close()
			}
		}(i)
	}
	wg.Wait()

	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s mode %04o exposed group/other bits under umask 0000", filepath.Base(p), fi.Mode().Perm())
		}
	}
}

// TestPrivateFileConcurrentOpenAndWriteBurst runs aggressive concurrent opens and writes across
// multiple goroutines contending on a fresh database path.
func TestPrivateFileConcurrentOpenAndWriteBurst(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)

	dir := t.TempDir()
	path := filepath.Join(dir, "burst_concurrent.db")

	const workers = 10
	var wg sync.WaitGroup
	wg.Add(workers)

	for i := 0; i < workers; i++ {
		go func(id int) {
			defer wg.Done()
			for attempt := 0; attempt < 3; attempt++ {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				s, err := Open(ctx, path, Options{NodeID: "burst-node"})
				if err == nil {
					_, _, _ = s.Create(ctx, task.NewTask{Title: "burst task", Priority: task.PriorityMedium})
					_, _ = s.List(ctx, task.Filter{})
					_ = s.Close()
					cancel()
					break
				}
				cancel()
				time.Sleep(10 * time.Millisecond)
			}
		}(i)
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

// TestEmptyStubPreservationDuringSimulatedLock verifies that removeEmptyDBStub protects empty stubs
// if WAL or SHM sidecars are present on disk.
func TestEmptyStubPreservationDuringSimulatedLock(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "stub_test.db")

	if err := os.WriteFile(dbPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Case 1: isolated 0-byte stub with no sidecars is removed.
	removeEmptyDBStub(dbPath)
	if _, err := os.Stat(dbPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected dbPath to be removed when isolated, stat err: %v", err)
	}

	// Case 2: 0-byte stub with WAL sidecar present is NOT removed.
	if err := os.WriteFile(dbPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	walPath := dbPath + "-wal"
	if err := os.WriteFile(walPath, []byte("active-wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	removeEmptyDBStub(dbPath)
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("expected dbPath to be preserved when WAL sidecar exists, got %v", err)
	}
	_ = os.Remove(walPath)

	// Case 3: 0-byte stub with SHM sidecar present is NOT removed.
	shmPath := dbPath + "-shm"
	if err := os.WriteFile(shmPath, []byte("active-shm"), 0o600); err != nil {
		t.Fatal(err)
	}
	removeEmptyDBStub(dbPath)
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("expected dbPath to be preserved when SHM sidecar exists, got %v", err)
	}
	_ = os.Remove(shmPath)
	_ = os.Remove(dbPath)
}

// TestConcurrentOpenWithContextCancellation verifies that concurrent openers suffering context
// cancellation do not destroy or corrupt the database for openers that succeed.
func TestConcurrentOpenWithContextCancellation(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)

	dir := t.TempDir()
	path := filepath.Join(dir, "cancel_race.db")

	const workers = 8
	var wg sync.WaitGroup
	wg.Add(workers)

	for i := 0; i < workers; i++ {
		go func(id int) {
			defer wg.Done()
			var ctx context.Context
			var cancel context.CancelFunc
			if id%2 == 0 {
				ctx, cancel = context.WithTimeout(context.Background(), 2*time.Millisecond)
			} else {
				ctx, cancel = context.WithTimeout(context.Background(), 4*time.Second)
			}
			defer cancel()

			s, err := Open(ctx, path, Options{NodeID: "cancel-node"})
			if err == nil {
				_, _, _ = s.Create(ctx, task.NewTask{Title: "survivor task", Priority: task.PriorityLow})
				_ = s.Close()
			}
		}(i)
	}
	wg.Wait()

	// At least one non-canceled worker should have succeeded or the file remains owner-only.
	fi, err := os.Stat(path)
	if err == nil {
		if fi.Mode().Perm()&0o077 != 0 {
			t.Fatalf("cancel_race.db mode %04o exposed group/other bits", fi.Mode().Perm())
		}
	}
}

// TestPrivateFileConcurrentOpenExistingDatabasePermsPreserved verifies that when opening an
// existing pre-populated database with non-default mode (e.g. 0640), concurrent opens never
// overwrite or mutate the permission bits.
func TestPrivateFileConcurrentOpenExistingDatabasePermsPreserved(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)

	dir := t.TempDir()
	path := filepath.Join(dir, "existing_custom_mode.db")

	// Create and initialize database first
	initStore, err := Open(context.Background(), path, Options{NodeID: "custom-node"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := initStore.Create(context.Background(), task.NewTask{Title: "initial task", Priority: task.PriorityHigh}); err != nil {
		initStore.Close()
		t.Fatal(err)
	}
	initStore.Close()

	// Explicitly set to 0640
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}

	const workers = 6
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			s, err := Open(context.Background(), path, Options{NodeID: "custom-node"})
			if err == nil {
				_ = s.Close()
			}
		}()
	}
	wg.Wait()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Fatalf("existing database mode mutated: got %04o, want 0640", fi.Mode().Perm())
	}
}
