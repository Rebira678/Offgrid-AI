package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSimReplayWithCRLFAndComments tests that readActions and offgrid sim -replay
// cleanly parse action trace files with CRLF line endings, comments, blank lines,
// and execute them to completion.
func TestSimReplayWithCRLFAndComments(t *testing.T) {
	tempDir := t.TempDir()
	replayFile := filepath.Join(tempDir, "trace.replay")

	content := strings.Join([]string{
		"# Simulated failure trace reproduction",
		"",
		"   # Indented comment",
		"create r0 n=101",
		"create r1 n=202",
		"sync r0-r1 n=303",
		"note r0 n=404",
		"sync r1-r0 n=505",
		"",
	}, "\r\n") // Intentionally Windows CRLF line endings

	if err := os.WriteFile(replayFile, []byte(content), 0644); err != nil {
		t.Fatalf("writing replay file: %v", err)
	}

	// 1. Verify readActions parses 5 valid actions
	acts, err := readActions(replayFile)
	if err != nil {
		t.Fatalf("readActions: %v", err)
	}
	if len(acts) != 5 {
		t.Fatalf("expected 5 actions, got %d", len(acts))
	}

	// 2. Execute via CLI
	code, out, errOut := runSim(t, "-replay", replayFile, "-replicas", "2", "-dir", tempDir)
	if code != 0 {
		t.Fatalf("sim -replay failed: code %d, out: %s, err: %s", code, out, errOut)
	}
	if !strings.Contains(out, "PASS (seed 1)") {
		t.Fatalf("expected 'PASS (seed 1)' in replay output, got:\n%s", out)
	}

	// 3. Test malformed action line in replay file
	badFile := filepath.Join(tempDir, "bad.replay")
	if err := os.WriteFile(badFile, []byte("totally_unknown_action_xyz\n"), 0644); err != nil {
		t.Fatalf("writing bad file: %v", err)
	}
	code, _, errOut = runSim(t, "-replay", badFile, "-dir", tempDir)
	if code != 2 {
		t.Fatalf("expected code 2 for malformed replay file, got %d", code)
	}
	if !strings.Contains(errOut, "error:") {
		t.Fatalf("expected error message on bad replay line, got: %s", errOut)
	}
}
