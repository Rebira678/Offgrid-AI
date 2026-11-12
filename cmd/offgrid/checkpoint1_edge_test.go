package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckpoint1EdgeStatsCustomLimits verifies that the sync stats CLI
// correctly applies customized thresholds passed on the command line.
func TestCheckpoint1EdgeStatsCustomLimits(t *testing.T) {
	file := filepath.Join(t.TempDir(), "soak.stats")
	var b strings.Builder
	// 25 samples over 15 minutes
	// Goroutines grow from 10 to 18 (+8 growth)
	for i := 0; i < 25; i++ {
		g := 10
		if i >= 15 {
			g = 18
		}
		b.WriteString(`{"at":"2026-11-12T10:` + twoDigits(i) + `:00Z","role":"run","goroutines":` + itoa(g) + `,"rss_kb":30000}` + "\n")
	}
	if err := os.WriteFile(file, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	// Under default limits (+5 max growth), +8 should FAIL
	code, out, _ := runCLI(t, "", "sync", "stats", file)
	if code != 1 || !strings.Contains(out, "RESULT: FAIL") {
		t.Fatalf("expected default limits to FAIL: code %d, out:\n%s", code, out)
	}

	// With custom -max-goroutine-growth 10, +8 should PASS
	code, out, errOut := runCLI(t, "", "sync", "stats", "-max-goroutine-growth", "10", file)
	if code != 0 || !strings.Contains(out, "RESULT: PASS") {
		t.Fatalf("expected custom limit to PASS: code %d, errOut: %s, out:\n%s", code, errOut, out)
	}
}

// TestCheckpoint1EdgeStatsCorruptedLineHandling verifies that malformed JSON
// in a stats file exits cleanly with code 1 and descriptive error.
func TestCheckpoint1EdgeStatsCorruptedLineHandling(t *testing.T) {
	file := filepath.Join(t.TempDir(), "corrupted.stats")
	content := `{"at":"2026-11-12T10:00:00Z","goroutines":10,"rss_kb":30000}` + "\n" +
		`{"at":"2026-11-12T10:01:00Z","goroutines":10` // partial trailing write
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, errOut := runCLI(t, "", "sync", "stats", file)
	if code != 1 || !strings.Contains(errOut, "soak: line 2 is not a sample") {
		t.Fatalf("expected error on line 2, got code %d, errOut: %s", code, errOut)
	}
}

// TestCheckpoint1EdgeDeprecationWarningPreserved verifies that all sync commands
// output the shared sync key deprecation warning to stderr.
func TestCheckpoint1EdgeDeprecationWarningPreserved(t *testing.T) {
	t.Setenv("OFFGRID_SYNC_KEY", strings.Repeat("k", 32))
	db := filepath.Join(t.TempDir(), "tasks.db")
	runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"Test"}`)

	// Test sync once deprecation warning
	{
		_, _, errOut := runCLI(t, "", "sync", "once", "-db", db, "-peer", "http://127.0.0.1:1", "-timeout", "100ms")
		if !strings.Contains(errOut, "warning: the shared sync key is deprecated") {
			t.Errorf("sync once missing deprecation warning:\n%s", errOut)
		}
	}
}
