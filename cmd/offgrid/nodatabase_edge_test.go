package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"offgrid/internal/store/sqlstore"
)

// TestNoDatabaseEdge_MemoryDBAllowsMustExist verifies that :memory: databases
// behave properly with MustExist: true and do not trigger requireFile.
func TestNoDatabaseEdge_MemoryDBAllowsMustExist(t *testing.T) {
	ctx := context.Background()
	st, err := sqlstore.Open(ctx, ":memory:", sqlstore.Options{
		MustExist: true,
		NodeID:    "node-mem",
	})
	if err != nil {
		t.Fatalf("expected :memory: with MustExist:true to succeed, got %v", err)
	}
	defer st.Close()

	if st.NodeID() != "node-mem" {
		t.Errorf("expected nodeID node-mem, got %s", st.NodeID())
	}
}

// TestNoDatabaseEdge_SymlinkHandling verifies that a broken symlink or symlink to a
// directory is properly handled by requireFile.
func TestNoDatabaseEdge_SymlinkHandling(t *testing.T) {
	tmp := t.TempDir()
	missingTarget := filepath.Join(tmp, "nonexistent.db")
	symlinkPath := filepath.Join(tmp, "broken_link.db")
	if err := os.Symlink(missingTarget, symlinkPath); err != nil {
		t.Skip("symlinks not supported on this filesystem")
	}

	ctx := context.Background()
	_, err := sqlstore.Open(ctx, symlinkPath, sqlstore.Options{MustExist: true})
	if err == nil {
		t.Fatal("expected error opening broken symlink with MustExist: true, got nil")
	}
	if !errors.Is(err, sqlstore.ErrNoDatabase) {
		t.Errorf("expected ErrNoDatabase, got %v", err)
	}
}

// TestNoDatabaseEdge_ConcurrentMissingOpens verifies that 20 concurrent goroutines
// attempting to open the same missing database all fail cleanly with ErrNoDatabase
// and leave zero files, sidecars, or locks on disk.
func TestNoDatabaseEdge_ConcurrentMissingOpens(t *testing.T) {
	tmp := t.TempDir()
	missingDB := filepath.Join(tmp, "concurrent_missing.db")

	var wg sync.WaitGroup
	ctx := context.Background()
	concurrency := 20
	errs := make([]error, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := sqlstore.Open(ctx, missingDB, sqlstore.Options{MustExist: true})
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err == nil {
			t.Errorf("goroutine %d: expected error, got nil", i)
		}
		if !errors.Is(err, sqlstore.ErrNoDatabase) {
			t.Errorf("goroutine %d: expected ErrNoDatabase, got %v", i, err)
		}
	}

	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("expected temp directory to remain completely empty, found %d entries", len(entries))
	}
}

// TestNoDatabaseEdge_ExitCode3StderrFormat verifies that every read command's
// stderr matches the standardized two-line message contract.
func TestNoDatabaseEdge_ExitCode3StderrFormat(t *testing.T) {
	missingDB := filepath.Join(t.TempDir(), "target_missing.db")

	commands := [][]string{
		{"task", "-db", missingDB, "list"},
		{"task", "-db", missingDB, "delete", "T1"},
		{"task", "-db", missingDB, "remove-note", "T1", "1"},
		{"sync", "summary", "-db", missingDB},
		{"store", "-db", missingDB, "info"},
		{"store", "-db", missingDB, "verify"},
		{"suggestions", "-db", missingDB, "list"},
		{"answers", "-db", missingDB},
		{"web", "-db", missingDB},
	}

	for _, cmd := range commands {
		t.Run(strings.Join(cmd[:2], "_"), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(cmd, &stdout, &stderr)
			if code != 3 {
				t.Errorf("run(%v) exit code = %d, want 3", cmd, code)
			}
			errStr := stderr.String()
			if !strings.Contains(errStr, "error: no such database: "+missingDB) {
				t.Errorf("stderr missing 'error: no such database': %s", errStr)
			}
			if !strings.Contains(errStr, "Nothing was created. Check the path, or start a database with: offgrid exec -db FILE") {
				t.Errorf("stderr missing remediation instruction: %s", errStr)
			}
			if stdout.Len() != 0 {
				t.Errorf("expected empty stdout, got %q", stdout.String())
			}
		})
	}
}
