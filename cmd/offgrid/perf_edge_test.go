package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPerfCLIEdgeFlagParsingAndValidation verifies robust flag validation and
// error reporting across the offgrid perf CLI interface.
func TestPerfCLIEdgeFlagParsingAndValidation(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantErrSub string
	}{
		{
			name:       "repeats zero",
			args:       []string{"perf", "-repeats", "0"},
			wantCode:   2,
			wantErrSub: "-repeats must be at least 1",
		},
		{
			name:       "repeats negative",
			args:       []string{"perf", "-repeats", "-2"},
			wantCode:   2,
			wantErrSub: "-repeats must be at least 1",
		},
		{
			name:       "sizes empty",
			args:       []string{"perf", "-sizes", ""},
			wantCode:   2,
			wantErrSub: "an empty list of sizes",
		},
		{
			name:       "sizes non-numeric",
			args:       []string{"perf", "-sizes", "10,twenty,30"},
			wantCode:   2,
			wantErrSub: "not a positive whole number",
		},
		{
			name:       "sizes negative",
			args:       []string{"perf", "-sizes", "10,-5,30"},
			wantCode:   2,
			wantErrSub: "not a positive whole number",
		},
		{
			name:       "jobs zero",
			args:       []string{"perf", "-jobs", "0"},
			wantCode:   2,
			wantErrSub: "not a positive whole number",
		},
		{
			name:       "workers negative",
			args:       []string{"perf", "-workers", "-1"},
			wantCode:   2,
			wantErrSub: "not a positive whole number",
		},
		{
			name:       "unexpected positional arguments",
			args:       []string{"perf", "unexpected-argument"},
			wantCode:   2,
			wantErrSub: "unexpected argument",
		},
		{
			name:       "unknown flag",
			args:       []string{"perf", "-unknown-flag"},
			wantCode:   2,
			wantErrSub: "flag provided but not defined",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, out, errOut := runCLI(t, "", tt.args...)
			if code != tt.wantCode {
				t.Fatalf("runCLI(%v) returned code %d (want %d), out: %s, err: %s", tt.args, code, tt.wantCode, out, errOut)
			}
			if !strings.Contains(errOut, tt.wantErrSub) && !strings.Contains(out, tt.wantErrSub) {
				t.Fatalf("expected output to contain %q; got out: %s, err: %s", tt.wantErrSub, out, errOut)
			}
		})
	}
}

// TestPerfCLIEdgeNonExistentDirectoryCreation verifies that passing a non-existent
// -dir path automatically creates it recursively and places files there.
func TestPerfCLIEdgeNonExistentDirectoryCreation(t *testing.T) {
	tempParent := t.TempDir()
	nestedDir := filepath.Join(tempParent, "deeply", "nested", "perfdir")

	args := append(tinyPerf, "-dir", nestedDir)
	code, out, errOut := runCLI(t, "", args...)
	if code != 0 {
		t.Fatalf("runCLI with nested dir failed: code %d, out: %s, err: %s", code, out, errOut)
	}

	// Verify the nested directory was created
	st, err := os.Stat(nestedDir)
	if err != nil || !st.IsDir() {
		t.Fatalf("expected nestedDir to exist as directory: %v", err)
	}

	if !strings.Contains(out, "**Hardware:**") {
		t.Fatalf("expected report output, got:\n%s", out)
	}
}

// TestPerfCLIEdgeReadmeAtomicReplacementAndErrorPreservation verifies atomic
// replacement of the README block without leaving .tmp files behind.
func TestPerfCLIEdgeReadmeAtomicReplacementAndErrorPreservation(t *testing.T) {
	dir := t.TempDir()
	readmePath := filepath.Join(dir, "README.md")

	initialContent := "# Welcome\n\nSome text\n\n" + perfStart + "\n[OLD STATS]\n" + perfEnd + "\n\nTrailing text\n"
	if err := os.WriteFile(readmePath, []byte(initialContent), 0644); err != nil {
		t.Fatal(err)
	}

	// 1. Successful update
	code, _, errOut := runCLI(t, "", append(tinyPerf, "-readme", readmePath)...)
	if code != 0 {
		t.Fatalf("perf -readme failed: %d, err: %s", code, errOut)
	}

	updated, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(updated)
	if !strings.HasPrefix(s, "# Welcome\n\nSome text\n\n"+perfStart+"\n**Hardware:**") {
		t.Fatalf("README prefix corrupted:\n%s", s)
	}
	if !strings.HasSuffix(s, perfEnd+"\n\nTrailing text\n") {
		t.Fatalf("README suffix corrupted:\n%s", s)
	}
	if strings.Contains(s, "[OLD STATS]") {
		t.Fatalf("old stats still present in README:\n%s", s)
	}

	// Ensure no .tmp file was left
	if _, err := os.Stat(readmePath + ".tmp"); err == nil {
		t.Errorf("temporary file %s.tmp was left behind", readmePath)
	}

	// 2. Failure case: corrupted marker (missing end marker)
	corrupted := "# Welcome\n\n" + perfStart + "\n[NO END MARKER]\n"
	if err := os.WriteFile(readmePath, []byte(corrupted), 0644); err != nil {
		t.Fatal(err)
	}

	code, _, errOut = runCLI(t, "", append(tinyPerf, "-readme", readmePath)...)
	if code != 1 {
		t.Fatalf("expected code 1 on corrupted markers, got %d", code)
	}
	if !strings.Contains(errOut, perfEnd) {
		t.Fatalf("expected error mentioning missing end marker, got: %s", errOut)
	}

	// README must be unchanged
	afterFailed, _ := os.ReadFile(readmePath)
	if string(afterFailed) != corrupted {
		t.Fatalf("corrupted README was modified despite error:\n%s", string(afterFailed))
	}
}

// TestPerfCLIEdgeArtifactParityAndValidation verifies that -md, -json, and -svg
// artifacts produced by offgrid perf are internally consistent and well-formed.
func TestPerfCLIEdgeArtifactParityAndValidation(t *testing.T) {
	dir := t.TempDir()
	mdFile := filepath.Join(dir, "out.md")
	jsonFile := filepath.Join(dir, "out.json")
	svgFile := filepath.Join(dir, "out.svg")

	code, out, errOut := runCLI(t, "", append(tinyPerf, "-md", mdFile, "-json", jsonFile, "-svg", svgFile)...)
	if code != 0 {
		t.Fatalf("perf failed: code %d, err: %s", code, errOut)
	}

	// 1. Verify Markdown file
	mdBytes, err := os.ReadFile(mdFile)
	if err != nil {
		t.Fatalf("reading mdFile: %v", err)
	}
	if string(mdBytes) != out {
		t.Fatalf("mdFile content differs from stdout")
	}

	// 2. Verify JSON file
	jsonBytes, err := os.ReadFile(jsonFile)
	if err != nil {
		t.Fatalf("reading jsonFile: %v", err)
	}
	var rep struct {
		Meta struct {
			Seed     int64 `json:"seed"`
			Repeats  int   `json:"repeats"`
			Hardware struct {
