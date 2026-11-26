package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"offgrid/internal/intent"
)

// DefaultMargin is how far a rate may fall below its baseline before the gate fails: 2 percentage
// points, which on the 105-case set is 2 cases. A deterministic stand-in (the CI reference) can use a
// small margin; a real model has run-to-run noise and its caller picks a larger one.
const DefaultMargin = 0.02

// CasesFingerprint is a short hash of the labelled cases. Rates measured on different case sets are not
// comparable, so a baseline remembers which set it was measured on.
func CasesFingerprint(cases []Case) string {
	h := sha256.New()
	for _, c := range cases {
		b, _ := json.Marshal(c)
		h.Write(b)
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// BaselineSetup is the counts of one setup (schema on or off, how many tries) in a baseline.
type BaselineSetup struct {
	UseSchema     bool `json:"use_schema"`
	MaxAttempts   int  `json:"max_attempts"`
	Cases         int  `json:"cases"`
	ValidFirstTry int  `json:"valid_first_try"`
	Succeeded     int  `json:"succeeded"`
	IntentCorrect int  `json:"intent_correct"`
	ArgsChecked   int  `json:"args_checked"`
	ArgsCorrect   int  `json:"args_correct"`
}

// Baseline is what a model scored when its numbers were last accepted. It is a committed file: a drop
// below it is a regression, and raising it is a decision someone makes in a commit.
type Baseline struct {
	Model             string          `json:"model"`
	Date              string          `json:"date,omitempty"`
	PromptVersion     string          `json:"prompt_version"`
	PromptFingerprint string          `json:"prompt_fingerprint"`
	CasesFingerprint  string          `json:"cases_fingerprint"`
	Setups            []BaselineSetup `json:"setups"`
}

// BaselineOf records sums as a baseline.
func BaselineOf(model, date string, sums []Summary) Baseline {
	b := Baseline{Model: model, Date: date, PromptVersion: intent.PromptVersion, PromptFingerprint: intent.PromptFingerprint()}
	for _, s := range sums {
		b.CasesFingerprint = s.CasesFingerprint
		b.Setups = append(b.Setups, BaselineSetup{s.UseSchema, s.MaxAttempts, s.Cases, s.ValidFirstTry, s.Succeeded, s.IntentCorrect, s.ArgsChecked, s.ArgsCorrect})
	}
	return b
}

// Write saves b as JSON.
func (b Baseline) Write(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(b)
}

// ReadBaseline loads a baseline file.
func ReadBaseline(path string) (Baseline, error) {
	var b Baseline
	f, err := os.Open(path)
	if err != nil {
		return b, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return b, fmt.Errorf("eval: read baseline %s: %w", path, err)
	}
	return b, nil
}

// Regression is one rate that fell more than the margin.
type Regression struct {
	Setup  string
	Metric string
	Was    Proportion
	Now    Proportion
}

func (r Regression) String() string {
	return fmt.Sprintf("%s: %s fell from %d/%d (%.1f%%) to %d/%d (%.1f%%)", r.Setup, r.Metric,
		r.Was.K, r.Was.N, 100*r.Was.Rate(), r.Now.K, r.Now.N, 100*r.Now.Rate())
}

// ErrStaleBaseline means the baseline was measured on a different case set, so no comparison is fair.
var ErrStaleBaseline = errors.New("eval: the baseline was measured on a different set of cases; measure it again and commit the new baseline")

// Compare checks sums against b. A rate regresses when it is lower than the baseline's by MORE than
// margin (so exactly margin is allowed). The rates gated are the valid-command rate on the first try,
// the valid-command rate after retries, the intent rate and the argument rate. Every setup in the
// baseline must have been run; a missing one is an error, not a pass.
// notes are things worth saying that are not failures (a rate that improved by more than the margin,
// a changed prompt).
func (b Baseline) Compare(sums []Summary, margin float64) (regs []Regression, notes []string, err error) {
	if margin < 0 {
		return nil, nil, errors.New("eval: margin must not be negative")
	}
	if len(b.Setups) == 0 {
		return nil, nil, errors.New("eval: the baseline has no setups")
	}
	if b.PromptFingerprint != intent.PromptFingerprint() {
		notes = append(notes, fmt.Sprintf("the prompt changed since the baseline (v%s %s, now v%s %s): this run measures the change",
			b.PromptVersion, b.PromptFingerprint, intent.PromptVersion, intent.PromptFingerprint()))
	}
	for _, want := range b.Setups {
		var got *Summary
		for i := range sums {
			if sums[i].UseSchema == want.UseSchema && sums[i].MaxAttempts == want.MaxAttempts {
				got = &sums[i]
			}
		}
		name := fmt.Sprintf("schema=%v attempts=%d", want.UseSchema, want.MaxAttempts)
		if got == nil {
			return regs, notes, fmt.Errorf("eval: the baseline has the setup %s but this run did not measure it", name)
		}
		if got.CasesFingerprint != b.CasesFingerprint || got.Cases != want.Cases {
			return regs, notes, ErrStaleBaseline
		}
		for _, m := range []struct {
			name     string
			was, now Proportion
		}{
			{"valid command, first try", Proportion{want.ValidFirstTry, want.Cases}, got.ValidFirstRate()},
			{"valid command, after retries", Proportion{want.Succeeded, want.Cases}, got.SucceededRate()},
			{"intent correct", Proportion{want.IntentCorrect, want.Cases}, got.IntentRate()},
			{"arguments correct", Proportion{want.ArgsCorrect, want.ArgsChecked}, got.ArgsRate()},
		} {
			switch d := m.now.Rate() - m.was.Rate(); {
			case d < -margin-1e-9:
				regs = append(regs, Regression{name, m.name, m.was, m.now})
			case d > margin+1e-9:
				notes = append(notes, fmt.Sprintf("%s: %s improved from %.1f%% to %.1f%%; commit a new baseline to lock it in", name, m.name, 100*m.was.Rate(), 100*m.now.Rate()))
			}
		}
	}
	sort.SliceStable(regs, func(i, j int) bool { return regs[i].Setup < regs[j].Setup })
	return regs, notes, nil
}

// HistoryHeader is the first line of the history CSV.
const HistoryHeader = "date,model,prompt_version,prompt_fingerprint,cases_fingerprint,setup,cases,valid_first_try,succeeded,intent_correct,args_correct,args_checked,avg_attempts,median_seconds,hardware"

// AppendHistory adds one row per summary to a CSV, writing the header if the file is new or empty. This
// is how the nightly run is recorded: one file, in git, a row per model per setup per night.
func AppendHistory(path string, meta Meta, sums []Summary) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	var b strings.Builder
	if st.Size() == 0 {
		b.WriteString(HistoryHeader + "\n")
	}
	hw := strings.NewReplacer(",", " ", "\n", " ").Replace(fmt.Sprintf("%s %dthreads %.0fGiB", meta.Hardware.CPU, meta.Hardware.Threads, meta.Hardware.MemGiB))
	for _, s := range sums {
		fmt.Fprintf(&b, "%s,%s,%s,%s,%s,%s,%d,%d,%d,%d,%d,%d,%.2f,%.2f,%s\n",
			meta.Date, csvField(s.Model), s.PromptVersion, s.PromptFingerprint, s.CasesFingerprint,
			csvField(s.Setup()), s.Cases, s.ValidFirstTry, s.Succeeded, s.IntentCorrect, s.ArgsCorrect, s.ArgsChecked,
			s.AvgAttempts(), s.MedianSeconds, hw)
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func csvField(s string) string { return strings.NewReplacer(",", " ", "\n", " ").Replace(s) }
