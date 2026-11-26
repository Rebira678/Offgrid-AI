package eval

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"offgrid/internal/intent"
	"offgrid/internal/sysinfo"
)

func TestBaselineCompareBoundaryExact(t *testing.T) {
	casesFp := "8fa84c0de958"
	b := Baseline{
		Model:             "test-model",
		PromptVersion:     intent.PromptVersion,
		PromptFingerprint: intent.PromptFingerprint(),
		CasesFingerprint:  casesFp,
		Setups: []BaselineSetup{
			{
				UseSchema:     true,
				MaxAttempts:   3,
				Cases:         100,
				ValidFirstTry: 90, // 90%
				Succeeded:     95, // 95%
				IntentCorrect: 80, // 80%
				ArgsChecked:   50,
				ArgsCorrect:   40, // 80%
			},
		},
	}

	// 1. Exact margin boundary (e.g. margin = 0.05).
	// If rate drops by exactly 0.05 (e.g. ValidFirstTry goes from 90 to 85, 90/100 -> 85/100):
	// difference is -0.05. Is it allowed? Yes, d < -margin - 1e-9 is false, so it PASSES!
	runExact := Summary{
		UseSchema:         true,
		MaxAttempts:       3,
		Cases:             100,
		ValidFirstTry:     85, // -5 points: exactly on margin
		Succeeded:         95,
		IntentCorrect:     80,
		ArgsChecked:       50,
		ArgsCorrect:       40,
		CasesFingerprint:  casesFp,
		PromptVersion:     intent.PromptVersion,
		PromptFingerprint: intent.PromptFingerprint(),
	}

	regs, _, err := b.Compare([]Summary{runExact}, 0.05)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(regs) != 0 {
		t.Errorf("drop equal to margin must be allowed, got regressions: %+v", regs)
	}

	// 2. Drop by 0.05 + 1 case (from 90 to 84, -6 points):
	// difference is -0.06 < -0.05 -> REGRESSION!
	runFail := runExact
	runFail.ValidFirstTry = 84
	regs, _, err = b.Compare([]Summary{runFail}, 0.05)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(regs) != 1 || regs[0].Metric != "valid command, first try" {
		t.Errorf("expected 1 regression on valid command first try, got: %+v", regs)
	}

	// 3. Significant improvement (from 90 to 96, +6 points > +0.05):
	// difference is +0.06 > +0.05 -> Recorded in notes!
	runImprove := runExact
	runImprove.ValidFirstTry = 96
	regs, notes, err := b.Compare([]Summary{runImprove}, 0.05)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(regs) != 0 {
		t.Errorf("improvements must not be regressions: %+v", regs)
	}
	if len(notes) == 0 || !strings.Contains(notes[0], "improved from 90.0% to 96.0%") {
		t.Errorf("expected improvement note, got: %v", notes)
	}
}

func TestBaselineNegativeMarginRefused(t *testing.T) {
	b := Baseline{Setups: []BaselineSetup{{Cases: 10}}}
	_, _, err := b.Compare([]Summary{{Cases: 10}}, -0.01)
	if err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Fatalf("expected negative margin error, got: %v", err)
	}
}

func TestBaselineEmptySetupsRefused(t *testing.T) {
	b := Baseline{Setups: nil}
	_, _, err := b.Compare([]Summary{{Cases: 10}}, 0.02)
	if err == nil || !strings.Contains(err.Error(), "no setups") {
		t.Fatalf("expected empty setups error, got: %v", err)
	}
}

func TestBaselineStaleCasesRefused(t *testing.T) {
	b := Baseline{
		CasesFingerprint: "expected_fp",
		Setups: []BaselineSetup{
			{UseSchema: true, MaxAttempts: 3, Cases: 105},
		},
	}

	// Fingerprint mismatch
	runMismatchFp := Summary{
		UseSchema:        true,
		MaxAttempts:      3,
		Cases:            105,
		CasesFingerprint: "different_fp",
	}
	_, _, err := b.Compare([]Summary{runMismatchFp}, 0.02)
	if !errors.Is(err, ErrStaleBaseline) {
		t.Fatalf("expected ErrStaleBaseline for fp mismatch, got: %v", err)
	}

	// Case count mismatch
	runMismatchCount := Summary{
		UseSchema:        true,
		MaxAttempts:      3,
		Cases:            104, // 104 instead of 105
		CasesFingerprint: "expected_fp",
	}
	_, _, err = b.Compare([]Summary{runMismatchCount}, 0.02)
	if !errors.Is(err, ErrStaleBaseline) {
		t.Fatalf("expected ErrStaleBaseline for count mismatch, got: %v", err)
	}
}

func TestAppendHistoryFormattingAndConcurrency(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "eval_history.csv")

	meta := Meta{
		Date: "2026-11-26",
		Hardware: sysinfo.Info{
			CPU:     "AMD, Ryzen\nSpecial", // contains commas and newlines
			Threads: 8,
			MemGiB:  16.0,
		},
	}

	sums := []Summary{
		{
			Model:             "model,comma\nnewline",
			UseSchema:         true,
			MaxAttempts:       3,
			Cases:             105,
			ValidFirstTry:     105,
			Succeeded:         105,
			IntentCorrect:     101,
			ArgsCorrect:       65,
			ArgsChecked:       68,
			PromptVersion:     "2",
			PromptFingerprint: "9d865728ff30",
			CasesFingerprint:  "8fa84c0de958",
		},
	}

	// 1. Initial write creates file with header
	if err := AppendHistory(p, meta, sums); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines (header + 1 row), got %d:\n%s", len(lines), string(data))
	}
	if !strings.HasPrefix(lines[0], HistoryHeader) {
		t.Errorf("expected header %q, got: %s", HistoryHeader, lines[0])
	}
	// Check sanitized fields
	if strings.Contains(lines[1], "model,comma") || strings.Contains(lines[1], "\nSpecial") {
		t.Errorf("commas and newlines in model/hardware must be replaced with spaces: %s", lines[1])
	}

	// 2. Second append does NOT duplicate header
	if err := AppendHistory(p, meta, sums); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(p)
	lines = strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines after second append, got %d", len(lines))
	}

	// 3. Concurrent appends
	const routines = 10
	var wg sync.WaitGroup
	for i := 0; i < routines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			_ = AppendHistory(p, meta, sums)
		}(i)
	}
	wg.Wait()

	data, _ = os.ReadFile(p)
	lines = strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3+routines {
		t.Errorf("expected %d lines after concurrent appends, got %d", 3+routines, len(lines))
	}
}

func TestCasesFingerprintSensitivity(t *testing.T) {
	cases := []Case{
		{ID: "c01", Text: "fix the pump", Intent: "create_task"},
		{ID: "c02", Text: "mark done", Intent: "update_task", Status: "done"},
	}
	fp1 := CasesFingerprint(cases)

	// Change ID
	casesMod := []Case{
		{ID: "c01_mod", Text: "fix the pump", Intent: "create_task"},
		{ID: "c02", Text: "mark done", Intent: "update_task", Status: "done"},
	}
	fp2 := CasesFingerprint(casesMod)
	if fp1 == fp2 {
		t.Errorf("fingerprint should change when ID changes: %s == %s", fp1, fp2)
	}

	// Change intent
	casesModIntent := []Case{
		{ID: "c01", Text: "fix the pump", Intent: "update_task"},
		{ID: "c02", Text: "mark done", Intent: "update_task", Status: "done"},
	}
	fp3 := CasesFingerprint(casesModIntent)
	if fp1 == fp3 {
		t.Errorf("fingerprint should change when intent changes: %s == %s", fp1, fp3)
	}
}

func TestProportionsEdgeCalculations(t *testing.T) {
	// Zero denominator
	pZero := Proportion{K: 0, N: 0}
	if r := pZero.Rate(); r != 0 {
		t.Errorf("expected 0 rate for 0/0, got %f", r)
	}
	lo, hi := pZero.Wilson()
	if lo != 0 || hi != 0 {
		t.Errorf("expected [0, 0] for Wilson(0/0), got [%f, %f]", lo, hi)
	}
	if s := pZero.String(); s != "n/a" {
		t.Errorf("expected 'n/a', got %q", s)
	}

	// Empty Summary rates
	sEmpty := Summary{Cases: 0, TotalAttempts: 0}
	if r := sEmpty.CleanRate().Rate(); r != 0 {
		t.Errorf("expected 0 for CleanRate: %f", r)
	}
	if r := sEmpty.ValidFirstRate().Rate(); r != 0 {
		t.Errorf("expected 0 for ValidFirstRate: %f", r)
	}
	if r := sEmpty.SucceededRate().Rate(); r != 0 {
		t.Errorf("expected 0 for SucceededRate: %f", r)
	}
	if r := sEmpty.IntentRate().Rate(); r != 0 {
		t.Errorf("expected 0 for IntentRate: %f", r)
	}
	if r := sEmpty.ArgsRate().Rate(); r != 0 {
		t.Errorf("expected 0 for ArgsRate: %f", r)
	}
	if avg := sEmpty.AvgAttempts(); avg != 0 {
		t.Errorf("expected 0 for AvgAttempts with 0 cases: %f", avg)
	}
}

func TestCaseArgumentMatchingEdgeCases(t *testing.T) {
	cNoArgs := Case{
		ID:     "c1",
		Text:   "list all tasks",
		Intent: intent.ListTasks,
	}
	if cNoArgs.HasArgs() {
		t.Errorf("case with no args should return HasArgs() = false")
	}
	// Matches with any arbitrary args as long as intent matches
	cmd := intent.Command{
		Intent:   intent.ListTasks,
		Site:     "north",
		Status:   "todo",
		Priority: "low",
	}
	if !cNoArgs.Correct(cmd) {
		t.Errorf("expected cNoArgs to match valid command")
	}

	// Case with case-insensitive arguments
	cWithArgs := Case{
		ID:       "c2",
		Text:     "mark urgent pump as done today",
		Intent:   intent.UpdateTask,
		Status:   "done",
		Priority: "high",
		When:     "today",
	}
	if !cWithArgs.HasArgs() {
		t.Errorf("expected HasArgs() = true")
	}

	// Same values with different casing should match
	cmdCased := intent.Command{
		Intent:   intent.UpdateTask,
		Status:   "DONE",
		Priority: "HIGH",
		When:     "TODAY",
	}
	if !cWithArgs.Correct(cmdCased) {
		t.Errorf("casing should be ignored when comparing arguments")
	}

	// Mismatched argument fails
	cmdMismatch := cmdCased
	cmdMismatch.Status = "in_progress"
	if cWithArgs.Correct(cmdMismatch) {
		t.Errorf("mismatched argument should not be correct")
	}

	// Mismatched intent fails even if args match
	cmdWrongIntent := cmdCased
	cmdWrongIntent.Intent = intent.CreateTask
	if cWithArgs.Correct(cmdWrongIntent) {
		t.Errorf("wrong intent should not be correct")
	}
}

func TestBaselineCorruptedReadAndUnknownFields(t *testing.T) {
	dir := t.TempDir()

	// 1. Non-existent file
	_, err := ReadBaseline(filepath.Join(dir, "missing.json"))
	if err == nil {
		t.Fatal("expected error for missing baseline file")
	}

	// 2. Corrupted JSON syntax
	corruptPath := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corruptPath, []byte(`{not-json}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = ReadBaseline(corruptPath)
	if err == nil {
		t.Fatal("expected error for corrupted JSON syntax")
	}

	// 3. Unknown field rejected
	unknownFieldPath := filepath.Join(dir, "unknown_field.json")
	if err := os.WriteFile(unknownFieldPath, []byte(`{"model":"m","unknown_field":123}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = ReadBaseline(unknownFieldPath)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected unknown field rejection, got: %v", err)
	}
}
