package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGoodFirstIssues_MarkdownStructure verifies that docs/good-first-issues.md
// is well-formed, contains at least 6 issues, and every issue has clear starter instructions.
func TestGoodFirstIssues_MarkdownStructure(t *testing.T) {
	docPath := filepath.Join("..", "..", "docs", "good-first-issues.md")
	content, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("reading %s: %v", docPath, err)
	}

	lines := strings.Split(string(content), "\n")
	var issues []string
	for _, l := range lines {
		if strings.HasPrefix(l, "## ") {
			issues = append(issues, strings.TrimPrefix(l, "## "))
		}
	}

	if len(issues) < 6 {
		t.Fatalf("expected at least 6 starter issues in %s, found %d: %v", docPath, len(issues), issues)
	}

	text := string(content)
	for _, issue := range issues {
		if !strings.Contains(text, issue) {
			t.Errorf("issue %q not found in text", issue)
		}
	}

	// Each issue must have start and done criteria
	startCount := strings.Count(text, "**Where to start:**")
	doneCount := strings.Count(text, "**Done when:**")
	if startCount < len(issues) {
		t.Errorf("found %d '**Where to start:**' sections, want at least %d", startCount, len(issues))
	}
	if doneCount < len(issues) {
		t.Errorf("found %d '**Done when:**' sections, want at least %d", doneCount, len(issues))
	}
}

// TestCreateGoodFirstIssuesScript_DryRunExecution tests that scripts/create-good-first-issues.sh
// executes cleanly in DRY=1 mode and emits all 6 issues without shell syntax errors.
func TestCreateGoodFirstIssuesScript_DryRunExecution(t *testing.T) {
	scriptRel := filepath.Join("scripts", "create-good-first-issues.sh")
	repoRoot := filepath.Join("..", "..")
	scriptPath := filepath.Join(repoRoot, scriptRel)
	if _, err := os.Stat(scriptPath); err != nil {
		t.Fatalf("script %s not found: %v", scriptPath, err)
	}

	cmd := exec.Command("bash", scriptRel)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "DRY=1")

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running %s DRY=1 failed: %v\nOutput:\n%s", scriptPath, err, string(out))
	}

	outStr := string(out)
	for _, want := range []string{
		"Run the tests on macOS and tell us what breaks",
		"Add `offgrid completion bash`",
		"Check the dashboard with a keyboard and a screen reader",
		"Stop ignoring the errors from closing and removing files in `internal/perf`",
		"Make `offgrid version` say more",
		"Document the sync protocol in one page",
	} {
		if !strings.Contains(outStr, want) {
			t.Errorf("dry-run output missing issue %q", want)
		}
	}
}
