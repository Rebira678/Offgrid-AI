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

// TestLaunchReadiness_ReadmeRelativeLinks parses every relative markdown link and
// image reference in README.md and ensures the referenced path exists in the repository.
func TestLaunchReadiness_ReadmeRelativeLinks(t *testing.T) {
	repoRoot := filepath.Join("..", "..")
	readmePath := filepath.Join(repoRoot, "README.md")
	content, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("reading README.md: %v", err)
	}

	// Match [text](target) and ![alt](target)
	linkRegex := regexp.MustCompile(`!?\[.*?\]\(([^)\s]+)\)`)
	matches := linkRegex.FindAllStringSubmatch(string(content), -1)
	if len(matches) == 0 {
		t.Fatalf("expected markdown links in README.md, found none")
	}

	for _, m := range matches {
		target := m[1]
		// Skip external URLs, intra-document anchors (#), and github action badge relative paths (../../)
		if strings.HasPrefix(target, "http://") ||
			strings.HasPrefix(target, "https://") ||
			strings.HasPrefix(target, "#") ||
			strings.HasPrefix(target, "../../") {
			continue
		}

		// Strip query parameters or anchor fragments
		cleanTarget := strings.Split(strings.Split(target, "#")[0], "?")[0]
		if cleanTarget == "" {
			continue
		}

		targetPath := filepath.Join(repoRoot, cleanTarget)
		if _, err := os.Stat(targetPath); err != nil {
			t.Errorf("README.md contains broken relative link: %s -> %s (error: %v)", target, targetPath, err)
		}
	}
}

// TestLaunchReadiness_NoPlaceholdersOrDraftLeaks scans user-facing launch documentation
// to verify that no template placeholders, draft markers, or sandbox references leak into release.
func TestLaunchReadiness_NoPlaceholdersOrDraftLeaks(t *testing.T) {
	repoRoot := filepath.Join("..", "..")
	readmePath := filepath.Join(repoRoot, "README.md")
	content, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("reading README.md: %v", err)
	}

	text := string(content)
	prohibitedPatterns := []string{
		"<your",
		"⟦",
		"TODO",
		"FIXME",
		"XXX",
		"xeon",
		"cloud sandbox",
	}

	for _, pattern := range prohibitedPatterns {
		if strings.Contains(strings.ToLower(text), strings.ToLower(pattern)) {
			t.Errorf("README.md contains forbidden draft pattern or sandbox reference: %q", pattern)
		}
	}
}

// TestLaunchReadiness_VersionStampingContract verifies that the compiled CLI binary
// version matches the Day 30 milestone format and contract.
func TestLaunchReadiness_VersionStampingContract(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"version"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("offgrid version exit code = %d, stderr: %s", code, stderr.String())
	}

	out := strings.TrimSpace(stdout.String())
	want := "offgrid 0.30.0-day30"
	if out != want {
		t.Errorf("version output = %q, want %q", out, want)
	}
}

// TestLaunchReadiness_Day30GuideContract verifies that docs/day-30.md contains
// the claims ledger, launch post drafts for all platforms, and outreach guidelines.
func TestLaunchReadiness_Day30GuideContract(t *testing.T) {
	guidePath := filepath.Join("..", "..", "docs", "day-30.md")
	content, err := os.ReadFile(guidePath)
	if err != nil {
		t.Fatalf("reading %s: %v", guidePath, err)
	}

	text := string(content)
	requiredSections := []string{
		"The claims ledger: say these, and only these",
		"The launch post (LinkedIn)",
		"X / short thread",
		"Show HN",
		"r/LocalLLaMA and r/golang",
		"The 30 messages",
		"Questions you will be asked",
	}

	for _, sec := range requiredSections {
		if !strings.Contains(text, sec) {
			t.Errorf("docs/day-30.md missing required section: %q", sec)
		}
	}
}
