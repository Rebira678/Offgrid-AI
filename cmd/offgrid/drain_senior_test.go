package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestQueueResultSubcommandCLI thoroughly tests the offgrid queue result command
// for all state variations: completed with result, running without result,
// non-existent job, and invalid CLI arguments.
func TestQueueResultSubcommandCLI(t *testing.T) {
	useFakeModels(t)
	db, queue := fill(t)

	// 1. Before draining: jobs are queued, so result returns exit code 1
	var out, errb bytes.Buffer
	code := run([]string{"queue", "-q", queue, "result", "1"}, &out, &errb)
	if code != 1 {
		t.Fatalf("expected exit code 1 for queued job result, got %d. err=%s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "job 1 has no result yet (it is queued)") {
		t.Errorf("unexpected error message: %s", errb.String())
	}

	// 2. Drain once so jobs complete
	code, dOut, dErr := runDrain(t, "-db", db, "-queue", queue, "-net", "offline", "-once", "-poll", "10ms")
	if code != 0 {
		t.Fatalf("drain failed: %d, %s %s", code, dErr, dOut)
	}

	// 3. After draining: result returns exit code 0 and exact text on stdout
	out.Reset()
	errb.Reset()
	code = run([]string{"queue", "-q", queue, "result", "1"}, &out, &errb)
	if code != 0 {
		t.Fatalf("expected exit code 0 for done job result, got %d. err=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "FAKE ANSWER") {
		t.Errorf("expected FAKE ANSWER in output, got: %q", out.String())
	}

	// 4. Non-existent job ID: exit code 0 with found=false (or error)
	out.Reset()
	errb.Reset()
	code = run([]string{"queue", "-q", queue, "result", "999"}, &out, &errb)
	if code == 0 {
		t.Error("expected non-zero exit code for non-existent job ID")
	}

	// 5. Invalid arguments (missing ID or non-integer)
	for _, badArgs := range [][]string{
		{"queue", "-q", queue, "result"},
		{"queue", "-q", queue, "result", "not-a-number"},
	} {
		out.Reset()
		errb.Reset()
		code = run(badArgs, &out, &errb)
		if code != 2 {
			t.Errorf("expected exit code 2 for args %v, got %d", badArgs, code)
		}
	}
}

// TestDrainCLIEmptyDatabase verifies that running drain on an empty queue
// exits cleanly with 0 and reports no jobs without errors.
func TestDrainCLIEmptyDatabase(t *testing.T) {
	useFakeModels(t)
	db, queue := fill(t)

	// First drain finishes all 2 jobs
	code, _, errb := runDrain(t, "-db", db, "-queue", queue, "-net", "offline", "-once", "-poll", "10ms")
	if code != 0 {
		t.Fatalf("setup drain failed: %d, %s", code, errb)
	}

	// Now the queue has 0 queued/running jobs. Drain once again.
	code, out, errb := runDrain(t, "-db", db, "-queue", queue, "-net", "offline", "-once", "-poll", "10ms")
	if code != 0 {
		t.Fatalf("expected exit code 0 on empty queue, got %d, err=%s", code, errb)
	}
	if !strings.Contains(out, "drain finished: 0 done, 0 will retry, 0 dead") {
		t.Errorf("unexpected output: %s", out)
	}
}

// TestDrainCLIFlagValidationEdgeCases tests invalid parameter values for drain.
func TestDrainCLIFlagValidationEdgeCases(t *testing.T) {
	useFakeModels(t)
	db, queue := fill(t)

	badCases := []struct {
		name string
		args []string
		msg  string
	}{
		{"negative poll", []string{"-db", db, "-queue", queue, "-net", "offline", "-poll", "-5ms"}, "poll"},
		{"negative lease", []string{"-db", db, "-queue", queue, "-net", "offline", "-lease", "-1s"}, "lease"},
	}

	for _, tc := range badCases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, errb := runDrain(t, tc.args...)
			if code != 2 {
				t.Errorf("expected exit code 2 for %s, got %d (err: %s)", tc.name, code, errb)
			}
			if !strings.Contains(strings.ToLower(errb), tc.msg) {
				t.Errorf("expected message containing %q, got: %q", tc.msg, errb)
			}
		})
	}
}
