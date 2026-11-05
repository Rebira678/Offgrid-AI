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

// TestOutreachScript_CSVGenerationAndVerification verifies that outreach-drafts.py
// correctly parses CSV rows, embeds the video link, generates structured drafts,
// and enforces placeholder detection in check mode.
func TestOutreachScript_CSVGenerationAndVerification(t *testing.T) {
	repoRoot := filepath.Join("..", "..")
	scriptPath := filepath.Join(repoRoot, "scripts", "outreach-drafts.py")

	tmpDir := t.TempDir()
	csvPath := filepath.Join(tmpDir, "targets.csv")
	draftsPath := filepath.Join(tmpDir, "drafts.md")

	// Create test CSV dataset
	csvContent := `#,Name,Website,Category,What they do (specific),Why Offgrid is relevant / angle for a comment,How sure I am
1,Acme Mine Systems,https://acme.test,Mining,Underground fleet automation,Reliable local queue during shaft communication drop,High confidence
2,Field Clinic Cloud,https://clinic.test,Healthcare,Remote triage tablets,Offline patient sync over local WiFi mesh,Verify first
3,Harbor Logistics,https://harbor.test,Maritime,Cargo crane telemetry,Vessel disconnected syncing upon dock reconnect,High confidence
`
	if err := os.WriteFile(csvPath, []byte(csvContent), 0o644); err != nil {
		t.Fatalf("writing test csv: %v", err)
	}

	testVideoURL := "https://example.com/demo90s"

	// 1. Generate drafts for all 3 targets
	genCmd := exec.Command("python3", scriptPath, csvPath, "--video", testVideoURL)
	var genOut bytes.Buffer
	genCmd.Stdout = &genOut
	genCmd.Stderr = os.Stderr
	if err := genCmd.Run(); err != nil {
		t.Fatalf("generating drafts failed: %v", err)
	}

	draftsOutput := genOut.String()
	if !strings.Contains(draftsOutput, "Acme Mine Systems") ||
		!strings.Contains(draftsOutput, "Field Clinic Cloud") ||
		!strings.Contains(draftsOutput, "Harbor Logistics") {
		t.Errorf("generated drafts missing targets: %s", draftsOutput)
	}
	if !strings.Contains(draftsOutput, testVideoURL) {
		t.Errorf("generated drafts missing custom video URL: %s", draftsOutput)
	}
	if !strings.Contains(draftsOutput, "**VERIFY FIRST**") {
		t.Errorf("expected VERIFY FIRST warning for unverified target")
	}

	// Save generated drafts
	if err := os.WriteFile(draftsPath, genOut.Bytes(), 0o644); err != nil {
		t.Fatalf("saving test drafts: %v", err)
	}

	// 2. Run --check on unedited drafts (must FAIL because placeholders remain)
	checkFailCmd := exec.Command("python3", scriptPath, "--check", draftsPath)
	failOut, err := checkFailCmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected --check to fail with placeholders remaining, but it succeeded: %s", string(failOut))
	}
	if !strings.Contains(string(failOut), "placeholder(s) left in 3 drafts") {
		t.Errorf("unexpected check failure output: %s", string(failOut))
	}

	// 3. Replace placeholders with personalized notes
	editedDrafts := strings.ReplaceAll(draftsOutput, "⟦FIRST LINE: one specific thing you saw⟧", "Saw your recent dispatch from Site B.")
	editedDrafts = strings.ReplaceAll(editedDrafts, "⟦NAME⟧", "Alex")
	if err := os.WriteFile(draftsPath, []byte(editedDrafts), 0o644); err != nil {
		t.Fatalf("saving edited drafts: %v", err)
	}

	// 4. Run --check on fully personalized drafts (must PASS)
	checkPassCmd := exec.Command("python3", scriptPath, "--check", draftsPath)
	passOut, err := checkPassCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("expected --check to pass with all placeholders resolved: %v, output: %s", err, string(passOut))
	}
	if !strings.Contains(string(passOut), "ok: 3 drafts, no placeholders left") {
		t.Errorf("unexpected check pass output: %s", string(passOut))
	}
}

// TestOutreachScript_IDFiltering verifies that the --ids option filters specific rows.
func TestOutreachScript_IDFiltering(t *testing.T) {
	repoRoot := filepath.Join("..", "..")
	scriptPath := filepath.Join(repoRoot, "scripts", "outreach-drafts.py")

	tmpDir := t.TempDir()
	csvPath := filepath.Join(tmpDir, "targets.csv")

	csvContent := `#,Name,Website,Category,What they do (specific),Why Offgrid is relevant / angle for a comment,How sure I am
10,Company Ten,https://ten.test,Robotics,Warehouse agvs,Edge sync,High
20,Company Twenty,https://twenty.test,Agriculture,Tractor sensors,Field sync,High
30,Company Thirty,https://thirty.test,Energy,Substation monitoring,Grid resilience,High
`
	if err := os.WriteFile(csvPath, []byte(csvContent), 0o644); err != nil {
		t.Fatalf("writing test csv: %v", err)
	}

	// Select only ID 20
	cmd := exec.Command("python3", scriptPath, csvPath, "--ids", "20")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("outreach-drafts.py --ids failed: %v", err)
	}

	outStr := string(out)
	if !strings.Contains(outStr, "Company Twenty") {
		t.Errorf("expected selected company 20 in output: %s", outStr)
	}
	if strings.Contains(outStr, "Company Ten") || strings.Contains(outStr, "Company Thirty") {
		t.Errorf("unselected companies appeared in output: %s", outStr)
	}
}
