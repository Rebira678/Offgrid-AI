package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Day 31 regression: a command that only reads must fail on a missing
// database, and must not create it. Before the fix, `task` and `sync summary`
// created an empty database (and so fixed its node name for good), and the
// commands that did refuse each used their own message and exit code.
func TestReadOnlyCommandsOnAMissingDatabase(t *testing.T) {
	t.Setenv("OFFGRID_NODE", "")
	cases := []struct {
		name string
		args []string // "%s" is replaced by the database path
		env  bool     // give the path through $OFFGRID_DB instead of -db
	}{
		{"task list", []string{"task", "-db", "%s", "list"}, false},
		{"task delete", []string{"task", "-db", "%s", "delete", "T1"}, false},
		{"task remove-note", []string{"task", "-db", "%s", "remove-note", "T1", "1"}, false},
		{"sync summary", []string{"sync", "summary", "-db", "%s"}, false},
		{"sync summary via env", []string{"sync", "summary"}, true},
		{"store info", []string{"store", "-db", "%s", "info"}, false},
		{"store verify", []string{"store", "-db", "%s", "verify"}, false},
		{"suggestions list", []string{"suggestions", "-db", "%s", "list"}, false},
		{"answers", []string{"answers", "-db", "%s"}, false},
		{"web", []string{"web", "-db", "%s"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			// The folder does not exist either: nothing may create it.
			db := filepath.Join(dir, "no-such-folder", "tasks.db")
			t.Setenv("OFFGRID_DB", "")
			args := make([]string, len(c.args))
			for i, a := range c.args {
				args[i] = strings.ReplaceAll(a, "%s", db)
			}
			if c.env {
				t.Setenv("OFFGRID_DB", db)
			}
			code, out, errOut := runCLI(t, "", args...)
			if code != exitNoDatabase {
				t.Errorf("exit %d, want %d (stderr %q)", code, exitNoDatabase, errOut)
			}
			if !strings.Contains(errOut, "no such database") || !strings.Contains(errOut, db) {
				t.Errorf("stderr %q should say \"no such database\" and name the file", errOut)
			}
			if out != "" {
				t.Errorf("wrote to stdout: %q", out)
			}
			if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 0 {
				t.Errorf("left files behind: %v", left)
			}
		})
	}
}

// A missing file in a folder that does exist: no .db, -wal or -shm may appear.
func TestReadOnlyCommandLeavesNoSidecarFiles(t *testing.T) {
	t.Setenv("OFFGRID_DB", "")
	dir := t.TempDir()
	db := filepath.Join(dir, "tasks.db")
	if code, _, _ := runCLI(t, "", "sync", "summary", "-db", db); code != exitNoDatabase {
		t.Fatalf("exit %d, want %d", code, exitNoDatabase)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("left %d files in the folder", len(entries))
	}
}

// The wrinkle: the node name must not be fixed by a command that only reads.
// Asking for a summary of a missing file under one name, then creating the real
// database under another, must give the real database the second name.
func TestReadOnlyCommandDoesNotFixTheNodeName(t *testing.T) {
	t.Setenv("OFFGRID_DB", "")
	db := filepath.Join(t.TempDir(), "tasks.db")
	t.Setenv("OFFGRID_NODE", "node-wrong")
	runCLI(t, "", "sync", "summary", "-db", db)
	t.Setenv("OFFGRID_NODE", "node-real")
	if code, _, errOut := runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"a"}`); code != 0 {
		t.Fatalf("exec: exit %d\n%s", code, errOut)
	}
	code, out, errOut := runCLI(t, "", "sync", "summary", "-db", db)
	if code != 0 || !strings.Contains(out, "node:        node-real") {
		t.Errorf("exit %d\nstdout %q\nstderr %q", code, out, errOut)
	}
}

// The commands that are allowed to create a database still do, folder included.
func TestWritingCommandsStillCreateTheDatabase(t *testing.T) {
	t.Setenv("OFFGRID_DB", "")
	db := filepath.Join(t.TempDir(), "sub", "tasks.db")
	if code, _, errOut := runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"a"}`); code != 0 {
		t.Fatalf("exec: exit %d\n%s", code, errOut)
	}
	if _, err := os.Stat(db); err != nil {
		t.Errorf("exec did not create the database: %v", err)
	}
}
