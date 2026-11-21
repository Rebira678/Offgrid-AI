package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
)

func TestPowerpullUsageAndUnknownCommands(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantExit int
		wantSub  string
	}{
		{"nil args", nil, 2, "usage:"},
		{"empty args", []string{}, 2, "usage:"},
		{"single arg write", []string{"write"}, 2, "usage:"},
		{"single arg check", []string{"check"}, 2, "usage:"},
		{"unknown command", []string{"frobnicate", "foo"}, 2, "unknown command \"frobnicate\""},
		{"write flag error unknown flag", []string{"write", "-invalidflag", "foo.db"}, 2, "flag provided but not defined"},
		{"write wrong arg count", []string{"write", "foo.db", "bar.db"}, 2, ""},
		{"check wrong arg count 1", []string{"check", "foo.db"}, 2, "usage: powerpull check DB ACKS"},
		{"check wrong arg count 3", []string{"check", "foo.db", "acks.txt", "extra"}, 2, "usage: powerpull check DB ACKS"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := run(tt.args, &out, &errb)
			if code != tt.wantExit {
				t.Fatalf("run(%v) exit code = %d, want %d (stderr: %s)", tt.args, code, tt.wantExit, errb.String())
			}
			if tt.wantSub != "" && !strings.Contains(errb.String(), tt.wantSub) && !strings.Contains(out.String(), tt.wantSub) {
				t.Fatalf("run(%v) expected output to contain %q; out=%q, err=%q", tt.args, tt.wantSub, out.String(), errb.String())
			}
		})
	}
}

func TestPowerpullWriteDatabaseOpenFailure(t *testing.T) {
	// A path that cannot be created (e.g. invalid directory)
	dir := t.TempDir()
	badPath := filepath.Join(dir, "nonexistent_dir", "sub", "db.db")

	var out, errb bytes.Buffer
	code := run([]string{"write", "-for", "50ms", badPath}, &out, &errb)
	if code != 1 {
		t.Fatalf("expected exit code 1 on DB open failure, got %d", code)
	}
	if !strings.Contains(errb.String(), "powerpull:") {
		t.Fatalf("expected error message on stderr, got %q", errb.String())
	}
}

func TestPowerpullCheckMissingFiles(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "exists.db")
	acksPath := filepath.Join(dir, "acks.txt")

	// Missing acks file
	var out, errb bytes.Buffer
	code := run([]string{"check", dbPath, filepath.Join(dir, "missing_acks.txt")}, &out, &errb)
	if code != 1 {
		t.Fatalf("expected exit code 1 on missing acks file, got %d", code)
	}

	// Create valid acks, but nonexistent DB path with invalid dir so Open fails
	os.WriteFile(acksPath, []byte("ack pp1-g0-0\n"), 0o600)
	out.Reset()
	errb.Reset()
	code = run([]string{"check", filepath.Join(dir, "nodir", "nonexistent.db"), acksPath}, &out, &errb)
	if code != 1 {
		t.Fatalf("expected exit code 1 on DB open error, got %d", code)
	}
	if !strings.Contains(out.String(), "FAIL: the database does not open after power loss") {
		t.Fatalf("unexpected output: %s", out.String())
	}
}

func TestPowerpullCheckEmptyAndMalformedAcks(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tasks.db")
	acksPath := filepath.Join(dir, "acks.txt")

	// Create real store with 1 task
	s, err := sqlstore.Open(context.Background(), dbPath, sqlstore.Options{NodeID: node})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.Create(context.Background(), task.NewTask{Title: "pp100-g0-0", Priority: task.PriorityMedium})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	// Case 1: Empty acks file should pass (0 acknowledged writes, 0 lost)
	os.WriteFile(acksPath, []byte(""), 0o600)
	var out, errb bytes.Buffer
	code := run([]string{"check", dbPath, acksPath}, &out, &errb)
	if code != 0 || !strings.Contains(out.String(), "PASS") {
		t.Fatalf("empty acks should pass: code=%d out=%s", code, out.String())
	}

	// Case 2: Acks file containing only malformed / corrupted lines
	// Lines without "ack ", lines with wrong hyphens, trailing incomplete lines
	malformedContent := strings.Join([]string{
		"# this is a comment",
		"random line without prefix",
		"ack invalid-format",
		"ack no-hyphens",
		"ack too-many-hyphens-here-now",
		"ack pp100-g0-0", // only this 1 valid line matching the DB!
		"ack truncated-line",
	}, "\n")
	os.WriteFile(acksPath, []byte(malformedContent), 0o600)

	out.Reset()
	errb.Reset()
	code = run([]string{"check", dbPath, acksPath}, &out, &errb)
	if code != 0 || !strings.Contains(out.String(), "PASS") || !strings.Contains(out.String(), "all 1 acknowledged writes present") {
		t.Fatalf("should parse 1 valid line and pass: code=%d, out=%s", code, out.String())
	}
}

func TestPowerpullCheckLostWritesReporting(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tasks.db")
	acksPath := filepath.Join(dir, "acks.txt")

	// Create empty db
	s, err := sqlstore.Open(context.Background(), dbPath, sqlstore.Options{NodeID: node})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	// Create acks file with 15 missing tasks (exercises the lost <= 10 logging cap)
	var acksLines []string
	for i := 0; i < 15; i++ {
		acksLines = append(acksLines, fmt.Sprintf("ack pp999-g0-%d", i))
	}
	os.WriteFile(acksPath, []byte(strings.Join(acksLines, "\n")), 0o600)

	var out, errb bytes.Buffer
	code := run([]string{"check", dbPath, acksPath}, &out, &errb)
	if code != 1 {
		t.Fatalf("expected code 1 on lost writes, got %d", code)
	}
	outStr := out.String()
	if !strings.Contains(outStr, "FAIL: 15 of 15 acknowledged writes are missing") {
		t.Fatalf("expected failure summary, got: %s", outStr)
	}
	// Check that first 10 items were logged
	for i := 0; i < 10; i++ {
		if !strings.Contains(outStr, fmt.Sprintf("LOST: pp999-g0-%d", i)) {
			t.Fatalf("expected lost item %d to be listed, got: %s", i, outStr)
		}
	}
	// 11th item should not be listed with LOST: prefix
	if strings.Contains(outStr, "LOST: pp999-g0-10") {
		t.Fatalf("expected lost logging to cap at 10, got: %s", outStr)
	}
}

func TestPowerpullWriteWithFlushWindowAndMultipleWriters(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "windowed.db")
	acksPath := filepath.Join(dir, "windowed_acks.txt")

	var out, errb bytes.Buffer
	// Test writing with -window 2ms, -writers 3, -for 100ms
	code := run([]string{
		"write",
		"-window", "2ms",
		"-writers", "3",
		"-for", "150ms",
		dbPath,
	}, &out, &errb)
	if code != 0 {
		t.Fatalf("write failed with exit code %d: %s", code, errb.String())
	}

	if !strings.Contains(out.String(), "ack pp") {
		t.Fatal("expected at least one ack in stdout")
	}

	// Verify check passes completely
	os.WriteFile(acksPath, out.Bytes(), 0o600)
	out.Reset()
	errb.Reset()
	code = run([]string{"check", dbPath, acksPath}, &out, &errb)
	if code != 0 || !strings.Contains(out.String(), "PASS") {
		t.Fatalf("check failed after windowed write: code=%d out=%s", code, out.String())
	}
}
