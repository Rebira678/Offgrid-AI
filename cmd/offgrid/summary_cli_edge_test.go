package main

import (
	"bytes"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIStoreVerifyDetectsSummaryMismatchAndRebuildFixes(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "cli_summary.db")

	// 1. Initialize store and create a task via CLI
	var out, errOut bytes.Buffer
	code := cmdExec([]string{"-db", dbPath, `{"intent":"create_task","title":"first task"}`}, strings.NewReader(""), &out, &errOut)
	if code != 0 {
		t.Fatalf("cmdExec failed: %d, stderr: %s", code, errOut.String())
	}

	// 2. Corrupt op_summary table directly in SQLite
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// Artificially change max_seq in op_summary to 999
	if _, err := db.Exec(`UPDATE op_summary SET max_seq = 999`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	// 3. Run "store verify" via CLI - must fail with exit code 1 and mention mismatch
	out.Reset()
	errOut.Reset()
	code = cmdStore([]string{"-db", dbPath, "verify"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("expected exit code 1 from verify on drifted summary, got %d (stdout: %s, stderr: %s)", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "the summary table is wrong") || !strings.Contains(errOut.String(), "Rebuild repairs it") {
		t.Errorf("expected stderr to explain summary drift and mention Rebuild repairs it, got: %s", errOut.String())
	}

	// 4. Run "store rebuild" via CLI - must succeed with code 0
	out.Reset()
	errOut.Reset()
	code = cmdStore([]string{"-db", dbPath, "rebuild"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("expected exit code 0 from rebuild, got %d (stderr: %s)", code, errOut.String())
	}

	// 5. Run "store verify" again via CLI - must now succeed with code 0
	out.Reset()
	errOut.Reset()
	code = cmdStore([]string{"-db", dbPath, "verify"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("expected exit code 0 from verify after rebuild, got %d (stderr: %s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "ok: the hash chain of all 1 ops matches") {
		t.Errorf("expected clean verification output, got:\n%s", out.String())
	}
}
