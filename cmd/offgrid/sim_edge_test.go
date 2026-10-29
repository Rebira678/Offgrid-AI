package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSimCLIFlagValidationEdgeCases tests invalid parameter bounds for offgrid sim.
func TestSimCLIFlagValidationEdgeCases(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"zero seeds", []string{"sim", "-seeds", "0"}, "error: -seeds and -steps must be at least 1"},
		{"negative seeds", []string{"sim", "-seeds", "-10"}, "error: -seeds and -steps must be at least 1"},
		{"zero steps", []string{"sim", "-steps", "0"}, "error: -seeds and -steps must be at least 1"},
		{"negative steps", []string{"sim", "-steps", "-5"}, "error: -seeds and -steps must be at least 1"},
		{"single replica", []string{"sim", "-replicas", "1"}, "error: -seeds and -steps must be at least 1"},
		{"negative replicas", []string{"sim", "-replicas", "-2"}, "error: -seeds and -steps must be at least 1"},
		{"zero workers", []string{"sim", "-j", "0"}, "error: -seeds and -steps must be at least 1"},
		{"negative workers", []string{"sim", "-j", "-4"}, "error: -seeds and -steps must be at least 1"},
		{"unexpected positional argument", []string{"sim", "extra-arg"}, "error: unexpected argument \"extra-arg\""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, errOut := runCLI(t, "", tc.args...)
			if code != 2 {
				t.Fatalf("runCLI(%v) exited %d, want 2. Stderr: %s", tc.args, code, errOut)
			}
			if !strings.Contains(errOut, tc.want) {
				t.Errorf("stderr %q does not contain expected substring %q", errOut, tc.want)
			}
		})
	}
}

// TestSimReplayFileEdgeCases tests file reading edge cases for -replay.
func TestSimReplayFileEdgeCases(t *testing.T) {
	tempDir := t.TempDir()

	// 1. Non-existent file
	nonExistent := filepath.Join(tempDir, "does-not-exist.txt")
	code, _, errOut := runCLI(t, "", "sim", "-replay", nonExistent)
	if code != 2 || !strings.Contains(errOut, "no such file or directory") {
		t.Fatalf("expected code 2 for non-existent file, got %d: %s", code, errOut)
	}

	// 2. Empty file
	emptyFile := filepath.Join(tempDir, "empty.txt")
	if err := os.WriteFile(emptyFile, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runCLI(t, "", "sim", "-replay", emptyFile)
	if code != 0 || !strings.Contains(out, "PASS") {
		t.Fatalf("empty schedule should pass with empty state, got %d: %s", code, out)
	}

	// 3. Comments and blank lines only
	commentsFile := filepath.Join(tempDir, "comments.txt")
	if err := os.WriteFile(commentsFile, []byte("# only comments\n   \n# another comment\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ = runCLI(t, "", "sim", "-replay", commentsFile)
	if code != 0 || !strings.Contains(out, "PASS") {
		t.Fatalf("comments-only file should pass, got %d: %s", code, out)
	}

	// 4. Corrupt action format in file
	corruptFile := filepath.Join(tempDir, "corrupt.txt")
	if err := os.WriteFile(corruptFile, []byte("create r0 n=1\ncorrupted line here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errOut = runCLI(t, "", "sim", "-replay", corruptFile)
	if code != 2 || !strings.Contains(errOut, "cannot read action") {
		t.Fatalf("expected error on corrupt action line, got %d: %s", code, errOut)
	}
}

// TestSimReplayWithCustomDir tests replaying into a custom directory.
func TestSimReplayWithCustomDir(t *testing.T) {
	tempDir := t.TempDir()
	f := filepath.Join(tempDir, "custom_actions.txt")
	if err := os.WriteFile(f, []byte("create r0 n=100\ncreate r1 n=200\nsync r0-r1 n=5\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	simDbDir := filepath.Join(tempDir, "sim_db_dir")
	if err := os.MkdirAll(simDbDir, 0o755); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := runCLI(t, "", "sim", "-replay", f, "-dir", simDbDir)
	if code != 0 {
		t.Fatalf("sim -replay with -dir failed: code %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "PASS") {
		t.Fatalf("missing PASS in output:\n%s", out)
	}
}

// TestSimSingleSeedTraceStepNumbering tests trace numbering format.
func TestSimSingleSeedTraceStepNumbering(t *testing.T) {
	code, out, errOut := runCLI(t, "", "sim", "-seed", "7", "-steps", "10")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "001  ") || !strings.Contains(out, "010  ") {
		t.Fatalf("trace output does not contain formatted step numbers (001.., 010..):\n%s", out)
	}
	if !strings.Contains(out, "PASS (seed 7)") {
		t.Fatalf("missing PASS (seed 7) in:\n%s", out)
	}
}
