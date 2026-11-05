package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestOutreachScript_HelpFlag verifies that scripts/outreach-drafts.py executes
// cleanly when invoked with help flags.
func TestOutreachScript_HelpFlag(t *testing.T) {
	repoRoot := filepath.Join("..", "..")
	scriptPath := filepath.Join(repoRoot, "scripts", "outreach-drafts.py")

	cmd := exec.Command("python3", scriptPath, "--help")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("exec outreach-drafts.py --help failed: %v, output: %s", err, string(out))
	}

	outStr := string(out)
	if !strings.Contains(outStr, "--video") || !strings.Contains(outStr, "--check") {
		t.Errorf("help text missing required flags: %s", outStr)
	}
}
