package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPerfgateFlagAndInputValidationErrors(t *testing.T) {
	dir := t.TempDir()
	validBench := write(t, dir, "valid.txt", benchText("Test CPU", 1000))

	// 1. Missing -new flag
	code, _, errOut := gate(t, nil)
	if code != 2 || !strings.Contains(errOut, "-new is required") {
		t.Fatalf("expected code 2 on missing -new, got code=%d, err=%s", code, errOut)
	}

	// 2. Unknown flag
	code, _, _ = gate(t, nil, "-invalid-flag-123")
	if code != 2 {
		t.Fatalf("expected code 2 on unknown flag, got %d", code)
	}

	// 3. Non-existent -old file
	code, _, errOut = gate(t, nil, "-new", validBench, "-old", filepath.Join(dir, "no-such-old.txt"))
	if code != 2 || !strings.Contains(errOut, "no such file") {
		t.Fatalf("expected code 2 on missing -old file, got code=%d, err=%s", code, errOut)
	}

	// 4. Non-existent -budgets file
	code, _, errOut = gate(t, nil, "-new", validBench, "-budgets", filepath.Join(dir, "no-such-budget.json"))
	if code != 2 || !strings.Contains(errOut, "no such file") {
		t.Fatalf("expected code 2 on missing -budgets file, got code=%d, err=%s", code, errOut)
	}

	// 5. Corrupt -budgets JSON
	corruptBudget := write(t, dir, "corrupt_budget.json", `{not valid json}`)
	code, _, errOut = gate(t, nil, "-new", validBench, "-budgets", corruptBudget)
	if code != 2 || !strings.Contains(errOut, "budgets:") {
		t.Fatalf("expected code 2 on corrupt -budgets, got code=%d, err=%s", code, errOut)
	}

	// 6. Invalid override (too short)
	code, _, errOut = gate(t, nil, "-new", validBench, "-override", "too short")
	if code != 2 || !strings.Contains(errOut, "reason") {
		t.Fatalf("expected code 2 on invalid override, got code=%d, err=%s", code, errOut)
	}

	// 7. Unwritable -md path
	unwritableMD := filepath.Join(dir, "non-existent-dir", "sub", "report.md")
	code, _, errOut = gate(t, nil, "-new", validBench, "-md", unwritableMD)
	if code != 2 || !strings.Contains(errOut, "no such file") {
		t.Fatalf("expected code 2 on unwritable -md, got code=%d, err=%s", code, errOut)
	}
}

func TestPerfgateCustomThresholds(t *testing.T) {
	dir := t.TempDir()
	old := write(t, dir, "old.txt", benchText("Test CPU", 1000))
	// 40% slower
	slow := write(t, dir, "slow.txt", benchText("Test CPU", 1400))

	// Default threshold is 10%: 40% slower should FAIL (code 1)
	code, out, _ := gate(t, nil, "-old", old, "-new", slow)
	if code != 1 || !strings.Contains(out, "FAILED") {
		t.Fatalf("expected failure with default thresholds, got code=%d, out=%s", code, out)
	}

	// With -max-time 0.50 (50% tolerance): 40% slower should PASS (code 0)
	code, out, _ = gate(t, nil, "-old", old, "-new", slow, "-max-time", "0.50")
	if code != 0 || !strings.Contains(out, "PASSED") {
		t.Fatalf("expected pass with -max-time 0.50, got code=%d, out=%s", code, out)
	}
}

func TestPerfgateMarkdownFileContent(t *testing.T) {
	dir := t.TempDir()
	old := write(t, dir, "old.txt", benchText("Test CPU", 1000))
	newFast := write(t, dir, "new.txt", benchText("Test CPU", 600)) // 40% faster
	mdPath := filepath.Join(dir, "summary.md")

	code, stdout, _ := gate(t, nil, "-old", old, "-new", newFast, "-md", mdPath)
	if code != 0 {
		t.Fatalf("expected pass, got %d", code)
	}

	// Verify markdown file was written and matches stdout
	data, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatalf("failed to read generated markdown: %v", err)
	}
	if string(data) != stdout {
		t.Fatalf("markdown file content differs from stdout:\n--- file ---\n%s\n--- stdout ---\n%s", string(data), stdout)
	}
	if !strings.Contains(string(data), "Improvements") || !strings.Contains(string(data), "PASSED") {
		t.Fatalf("markdown does not contain expected improvement sections: %s", string(data))
	}
}

func TestPerfgateDifferentHardwareSkipsGating(t *testing.T) {
	dir := t.TempDir()
	old := write(t, dir, "old.txt", benchText("Intel Core i7", 1000))
	newDiff := write(t, dir, "new.txt", benchText("Apple M3 Max", 500))

	code, out, _ := gate(t, nil, "-old", old, "-new", newDiff)
	if code != 0 {
		t.Fatalf("different hardware must exit 0, got %d", code)
	}
	if !strings.Contains(out, "Different hardware") {
		t.Fatalf("expected 'Different hardware' notice, got: %s", out)
	}
	if !strings.Contains(out, "PASSED") {
		t.Fatalf("expected PASSED on different hardware, got: %s", out)
	}
}
