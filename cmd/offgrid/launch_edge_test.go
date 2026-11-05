package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestLaunchReadiness_FileManifest verifies that all critical launch artifacts,
// documentation files, CI/CD specifications, and media assets exist, are readable,
// and are non-empty.
func TestLaunchReadiness_FileManifest(t *testing.T) {
	requiredFiles := []string{
		"LICENSE",
		"CONTRIBUTING.md",
		"README.md",
		"ARCHITECTURE.md",
		".github/workflows/ci.yml",
		".github/workflows/release.yml",
		"docs/day-29-demo.gif",
		"docs/architecture.png",
		"docs/architecture.svg",
		"docs/day-30.md",
		"docs/good-first-issues.md",
		"scripts/launch-check.sh",
		"scripts/outreach-drafts.py",
		"scripts/fresh-clone-test.sh",
		"scripts/render-demo-gif.py",
		"scripts/converge-demo.sh",
	}

	repoRoot := filepath.Join("..", "..")
	for _, relPath := range requiredFiles {
		absPath := filepath.Join(repoRoot, relPath)
		info, err := os.Stat(absPath)
		if err != nil {
			t.Errorf("critical launch file missing: %s (%v)", relPath, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("critical launch file is empty: %s", relPath)
		}
	}
}
