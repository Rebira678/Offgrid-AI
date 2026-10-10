package agent

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"offgrid/internal/eval"
)

// CaseOutcome is one labelled message run through the loop.
type CaseOutcome struct {
	Case    eval.Case
	Outcome Outcome
	Correct bool // the command the loop proposed matches the label
}

// RunCases pushes every case through the agent, one after another, so the
// effects of earlier messages are visible to later ones (as in real use). each,
// if not nil, is called with every outcome as it happens (for logging).
func RunCases(ctx context.Context, a *Agent, cases []eval.Case, each func(Outcome)) []CaseOutcome {
	out := make([]CaseOutcome, 0, len(cases))
	for _, c := range cases {
		if ctx.Err() != nil {
			break
		}
		o := a.Handle(ctx, c.Text)
		if each != nil {
			each(o)
		}
		out = append(out, CaseOutcome{Case: c, Outcome: o, Correct: o.Tier != TierNone && c.Correct(o.Command)})
	}
	return out
}

// CaseStats counts what each tier did with the labelled cases.
type CaseStats struct {
	Total        int
	Rules        int // handled by Tier 0
	RulesCorrect int
	Model        int // handled by Tier 1
	ModelCorrect int
	Unhandled    int // no tier produced a command
	Completed    int // reached a final outcome other than "failed" or "not understood"
}

// CountCases tallies results.
func CountCases(rs []CaseOutcome) CaseStats {
	var s CaseStats
	for _, r := range rs {
		s.Total++
		switch r.Outcome.Tier {
		case TierRules:
			s.Rules++
			if r.Correct {
				s.RulesCorrect++
			}
		case TierModel:
			s.Model++
			if r.Correct {
				s.ModelCorrect++
			}
		default:
			s.Unhandled++
		}
		if r.Outcome.Status != StatusFailed && r.Outcome.Status != StatusNotUnderstood {
			s.Completed++
		}
	}
	return s
}

// WriteCases prints one line per case: tier, latency, and whether the proposed
// command matched the label, followed by the totals.
func WriteCases(w io.Writer, rs []CaseOutcome) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTIER\tLATENCY\tMATCHES LABEL\tOUTCOME\tMESSAGE")
	for _, r := range rs {
		tier, match := "-", "-"
		switch r.Outcome.Tier {
		case TierRules:
			tier = "0 rules"
		case TierModel:
			tier = "1 model"
		}
		if r.Outcome.Tier != TierNone {
			match = "yes"
			if !r.Correct {
				match = "NO"
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Case.ID, tier, FormatDuration(r.Outcome.Total), match, r.Outcome.Status, truncate(r.Case.Text, 60))
	}
	tw.Flush()

	s := CountCases(rs)
	fmt.Fprintf(w, "\nTier 0 (rules) handled %d of %d, and %d of those matched the label.\n", s.Rules, s.Total, s.RulesCorrect)
	if s.Model > 0 {
		fmt.Fprintf(w, "Tier 1 (model) handled %d of %d, and %d of those matched the label.\n", s.Model, s.Total, s.ModelCorrect)
	}
	if s.Unhandled > 0 {
		fmt.Fprintf(w, "%d of %d reached no command (the model was missing, down, or gave no valid reply).\n", s.Unhandled, s.Total)
	}
	fmt.Fprintf(w, "%d of %d ran to a final outcome.\n", s.Completed, s.Total)
}

func truncate(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "…"
}
