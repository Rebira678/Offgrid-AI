package eval

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"offgrid/internal/intent"
	"offgrid/internal/llm"
	"offgrid/internal/structured"
)

// ciRuns are the three runs the CI gate makes: the prompt alone (no schema, no retries: exposes the
// JSON repair), the prompt with retries (exposes the retry loop and its feedback), and the production
// setup (schema and retries).
func ciRuns(t *testing.T, c llm.Client, cases []Case) []Summary {
	t.Helper()
	var sums []Summary
	for _, s := range []struct {
		schema   bool
		attempts int
	}{{false, 1}, {false, structured.DefaultMaxAttempts}, {true, structured.DefaultMaxAttempts}} {
		sum, err := Run(context.Background(), Config{Client: c, Model: "reference", UseSchema: s.schema, MaxAttempts: s.attempts}, cases)
		if err != nil {
			t.Fatal(err)
		}
		sums = append(sums, sum)
	}
	return sums
}

// TestTheGoldenSetHoldsOnTheReferenceModel is the gate CI runs on every push. It fails when the
// valid-command rate (or the intent or argument rate) of the pipeline falls more than DefaultMargin
// below the committed baseline. To accept a lower number on purpose, regenerate the baseline:
//
//	UPDATE_BASELINE=1 go test ./internal/eval -run TestTheGoldenSetHolds
//
// and say why in the commit.
func TestTheGoldenSetHoldsOnTheReferenceModel(t *testing.T) {
	cases, err := LoadCases()
	if err != nil {
		t.Fatal(err)
	}
	sums := ciRuns(t, NewReference(cases), cases)
	if os.Getenv("UPDATE_BASELINE") != "" {
		var b bytes.Buffer
		if err := BaselineOf("reference", "", sums).Write(&b); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("baseline.json", b.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Skip("baseline.json rewritten")
	}
	base, err := ReadBaseline("baseline.json")
	if err != nil {
		t.Fatal(err)
	}
	regs, notes, err := base.Compare(sums, DefaultMargin)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range notes {
		t.Log(n)
	}
	for _, r := range regs {
		t.Error(r)
	}
}

func TestTheReferenceModelHasTheFaultsItClaims(t *testing.T) {
	cases, _ := LoadCases()
	sums := ciRuns(t, NewReference(cases), cases)
	prompt, prod := sums[0], sums[2]
	if prompt.CleanFirstTry == prompt.Cases || prompt.Repaired == 0 {
		t.Errorf("without a schema some replies must need repair: clean %d repaired %d", prompt.CleanFirstTry, prompt.Repaired)
	}
	if prompt.Succeeded >= prompt.Cases-1 || prompt.IntentCorrect >= prompt.Succeeded+1 {
		t.Errorf("no retries: broken JSON must lose cases: %+v", prompt.SucceededRate())
	}
	if prod.CleanFirstTry != prod.Cases || prod.Succeeded != prod.Cases || prod.Rescued != 0 {
		t.Errorf("with the schema every reply is clean: clean %d/%d rescued %d", prod.CleanFirstTry, prod.Cases, prod.Rescued)
	}
	if prod.IntentCorrect == prod.Cases {
		t.Error("the wrong-intent fault must show up in the intent rate")
	}
	retried, _ := Run(context.Background(), Config{Client: NewReference(cases), Model: "r", UseSchema: false, MaxAttempts: 3}, cases)
	if retried.Rescued == 0 || retried.Succeeded != retried.Cases {
		t.Errorf("retries must rescue the broken replies: rescued %d succeeded %d/%d", retried.Rescued, retried.Succeeded, retried.Cases)
	}
}

// ---- the gate's own arithmetic ----------------------------------------------

func sumOf(schema bool, attempts, cases, valid1, ok, intentOK, argsChecked, argsOK int) Summary {
	return Summary{UseSchema: schema, MaxAttempts: attempts, Cases: cases, CasesFingerprint: "f1",
		ValidFirstTry: valid1, Succeeded: ok, IntentCorrect: intentOK, ArgsChecked: argsChecked, ArgsCorrect: argsOK}
}

func baseOf(s Summary) Baseline {
	b := BaselineOf("m", "d", []Summary{s})
	b.PromptFingerprint = intent.PromptFingerprint()
	return b
}

func TestAMarginIsExactlyAllowedAndOneMoreFails(t *testing.T) {
	base := baseOf(sumOf(true, 3, 100, 90, 100, 95, 50, 40))
	// valid-after-retries falls 100 -> 98: exactly the 2-point margin, allowed
	regs, _, err := base.Compare([]Summary{sumOf(true, 3, 100, 90, 98, 95, 50, 40)}, 0.02)
	if err != nil || len(regs) != 0 {
		t.Fatalf("at the margin: %v %v", regs, err)
	}
	regs, _, _ = base.Compare([]Summary{sumOf(true, 3, 100, 90, 97, 95, 50, 40)}, 0.02)
	if len(regs) != 1 || regs[0].Metric != "valid command, after retries" || regs[0].Now.K != 97 {
		t.Fatalf("one over: %v", regs)
	}
	if !strings.Contains(regs[0].String(), "fell from 100/100 (100.0%) to 97/100 (97.0%)") {
		t.Fatal(regs[0].String())
	}
}

func TestEachGatedRateIsWatched(t *testing.T) {
	base := baseOf(sumOf(true, 3, 100, 90, 100, 95, 50, 40))
	for name, s := range map[string]Summary{
		"first":  sumOf(true, 3, 100, 80, 100, 95, 50, 40),
		"after":  sumOf(true, 3, 100, 90, 90, 95, 50, 40),
		"intent": sumOf(true, 3, 100, 90, 100, 80, 50, 40),
		"args":   sumOf(true, 3, 100, 90, 100, 95, 50, 30),
	} {
		regs, _, err := base.Compare([]Summary{s}, 0.02)
		if err != nil || len(regs) != 1 {
			t.Errorf("%s: %v %v", name, regs, err)
		}
	}
	// Argument rate is measured against arguments checked, not cases: 40/50 = 80%, 38/50 = 76%.
	if regs, _, _ := base.Compare([]Summary{sumOf(true, 3, 100, 90, 100, 95, 50, 38)}, 0.02); len(regs) != 1 {
		t.Fatalf("%v", regs)
	}
}

func TestImprovementsAreNotesAndNeverFailures(t *testing.T) {
	base := baseOf(sumOf(true, 3, 100, 80, 90, 85, 50, 30))
	regs, notes, err := base.Compare([]Summary{sumOf(true, 3, 100, 95, 100, 99, 50, 45)}, 0.02)
	if err != nil || len(regs) != 0 || len(notes) == 0 || !strings.Contains(notes[0], "improved") {
		t.Fatalf("%v %v %v", regs, notes, err)
	}
	// and a change within the margin says nothing at all
	if _, notes, _ := base.Compare([]Summary{sumOf(true, 3, 100, 81, 91, 86, 50, 31)}, 0.02); len(notes) != 0 {
		t.Fatalf("%v", notes)
	}
}

func TestTheGateRefusesToCompareWhatIsNotComparable(t *testing.T) {
	base := baseOf(sumOf(true, 3, 100, 90, 100, 95, 50, 40))
	other := sumOf(true, 3, 100, 90, 100, 95, 50, 40)
	other.CasesFingerprint = "f2"
	if _, _, err := base.Compare([]Summary{other}, 0.02); !errors.Is(err, ErrStaleBaseline) {
		t.Fatalf("different cases: %v", err)
	}
	if _, _, err := base.Compare([]Summary{sumOf(false, 1, 100, 90, 100, 95, 50, 40)}, 0.02); err == nil || !strings.Contains(err.Error(), "did not measure") {
		t.Fatalf("a missing setup must not pass: %v", err)
	}
	if _, _, err := base.Compare(nil, 0.02); err == nil {
		t.Fatal("no runs must not pass")
	}
	if _, _, err := (Baseline{}).Compare([]Summary{other}, 0.02); err == nil {
		t.Fatal("an empty baseline must not pass")
	}
	if _, _, err := base.Compare([]Summary{sumOf(true, 3, 100, 90, 100, 95, 50, 40)}, -1); err == nil {
		t.Fatal("negative margin")
	}
}

func TestAChangedPromptIsNotedNotFailed(t *testing.T) {
	base := baseOf(sumOf(true, 3, 100, 90, 100, 95, 50, 40))
	base.PromptFingerprint, base.PromptVersion = "old", "1"
	_, notes, err := base.Compare([]Summary{sumOf(true, 3, 100, 90, 100, 95, 50, 40)}, 0.02)
	if err != nil || len(notes) != 1 || !strings.Contains(notes[0], "the prompt changed") {
		t.Fatalf("%v %v", notes, err)
	}
}

func TestBaselineRoundTripsAndRefusesUnknownFields(t *testing.T) {
	dir := t.TempDir()
	b := baseOf(sumOf(true, 3, 100, 90, 100, 95, 50, 40))
	var buf bytes.Buffer
	if err := b.Write(&buf); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "b.json")
	os.WriteFile(p, buf.Bytes(), 0o644)
	got, err := ReadBaseline(p)
	if err != nil || got.Setups[0] != b.Setups[0] || got.CasesFingerprint != "f1" {
		t.Fatalf("%+v %v", got, err)
	}
	os.WriteFile(p, []byte(`{"model":"m","surprise":1}`), 0o644)
	if _, err := ReadBaseline(p); err == nil {
		t.Fatal("a misspelt baseline must not load as an empty one")
	}
	if _, err := ReadBaseline(filepath.Join(dir, "none.json")); err == nil {
		t.Fatal("missing file")
	}
}

func TestCasesFingerprintChangesWithAnyLabel(t *testing.T) {
	cases, _ := LoadCases()
	a := CasesFingerprint(cases)
	if a != CasesFingerprint(cases) {
		t.Fatal("not stable")
	}
	cases[10].Priority += "x"
	if CasesFingerprint(cases) == a {
		t.Fatal("a changed label must change the fingerprint")
	}
}

func TestHistoryIsAppendedWithOneHeader(t *testing.T) {
	p := filepath.Join(t.TempDir(), "history.csv")
	cases, _ := LoadCases()
	sums := ciRuns(t, NewReference(cases), cases)
	meta := Meta{Date: "2026-11-24"}
	meta.Hardware.CPU = "Test, CPU"
	for i := 0; i < 2; i++ {
		if err := AppendHistory(p, meta, sums); err != nil {
			t.Fatal(err)
		}
	}
	f, _ := os.Open(p)
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil || len(rows) != 7 || strings.Join(rows[0], ",") != HistoryHeader {
		t.Fatalf("%v %d rows", err, len(rows))
	}
	for _, r := range rows[1:] {
		if len(r) != len(rows[0]) || r[0] != "2026-11-24" || r[2] != intent.PromptVersion || r[14] == "" {
			t.Fatalf("%v", r)
		}
	}
	if rows[3][5] != "+ schema + retries (up to 3)" || rows[3][6] != "105" {
		t.Fatalf("%v", rows[2])
	}
}
