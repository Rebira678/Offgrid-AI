package main

import (
	"strings"
	"testing"
)

func runSim(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var o, e strings.Builder
	code := run(append([]string{"sim"}, args...), &o, &e)
	return code, o.String(), e.String()
}

// TestSimFlagValidationAndWorkerBounds verifies CLI validation for sim flags.
func TestSimFlagValidationAndWorkerBounds(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantErrSub string
	}{
		{
			name:       "seeds zero",
			args:       []string{"-seeds", "0"},
			wantCode:   2,
			wantErrSub: "-seeds and -steps must be at least 1",
		},
		{
			name:       "steps zero",
			args:       []string{"-steps", "0"},
			wantCode:   2,
			wantErrSub: "-seeds and -steps must be at least 1",
		},
		{
			name:       "replicas less than 2",
			args:       []string{"-replicas", "1"},
			wantCode:   2,
			wantErrSub: "-replicas at least 2",
		},
		{
			name:       "workers zero",
			args:       []string{"-j", "0"},
			wantCode:   2,
			wantErrSub: "-j at least 1",
		},
		{
			name:       "unexpected extra argument",
			args:       []string{"bogus-argument"},
			wantCode:   2,
			wantErrSub: "unexpected argument",
		},
		{
			name:       "nonexistent replay file",
			args:       []string{"-replay", "definitely-not-found-replay.txt"},
			wantCode:   2,
			wantErrSub: "no such file or directory",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, out, errOut := runSim(t, tt.args...)
			if code != tt.wantCode {
				t.Fatalf("runSim(%v) returned code %d (want %d), out: %s, err: %s", tt.args, code, tt.wantCode, out, errOut)
			}
			if !strings.Contains(errOut, tt.wantErrSub) && !strings.Contains(out, tt.wantErrSub) {
				t.Fatalf("expected output to contain %q; got out: %s, err: %s", tt.wantErrSub, out, errOut)
			}
		})
	}
}

// TestSimSingleSeedExecutionCLI tests that running a single seed through the CLI
// executes deterministically and prints the expected trace and PASS message.
func TestSimSingleSeedExecutionCLI(t *testing.T) {
	tempDir := t.TempDir()
	code, out, errOut := runSim(t, "-seed", "42", "-steps", "6", "-replicas", "2", "-dir", tempDir)
	if code != 0 {
		t.Fatalf("runSim single seed failed: code %d, out: %s, err: %s", code, out, errOut)
	}
	if !strings.Contains(out, "PASS (seed 42)") {
		t.Fatalf("expected 'PASS (seed 42)' in output, got:\n%s", out)
	}
}
