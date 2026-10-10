// Package eval measures how reliably a model turns worker messages into valid,
// correct commands.
//
// "It seems to work" is not a measurement. This package runs a fixed set of
// labelled messages through the real classifier and counts what happened, so
// two models, or two techniques, can be compared with numbers.
package eval

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"offgrid/internal/intent"
	"offgrid/internal/llm"
	"offgrid/internal/structured"
)

//go:embed cases.json
var casesJSON []byte

// Case is one labelled worker message. Intent is always checked. Site,
// Priority, Status and When are checked only when the case lists them, so a
// case does not penalise a model for filling in a field the test never cared
// about.
type Case struct {
	ID       string        `json:"id"`
	Text     string        `json:"text"`
	Intent   intent.Intent `json:"intent"`
	Site     string        `json:"site,omitempty"`
	Priority string        `json:"priority,omitempty"`
	Status   string        `json:"status,omitempty"`
	When     string        `json:"when,omitempty"`
}

// HasArgs reports whether the case lists any expected argument.
func (c Case) HasArgs() bool {
	return c.Site != "" || c.Priority != "" || c.Status != "" || c.When != ""
}

// matches reports whether got agrees with every argument the case lists.
func (c Case) matches(got intent.Command) bool {
	same := func(want, have string) bool { return want == "" || strings.EqualFold(want, have) }
	return same(c.Site, got.Site) && same(c.Priority, got.Priority) &&
		same(c.Status, got.Status) && same(c.When, got.When)
}

// Correct reports whether got is what the case expects: the right intent, and
// every argument the case lists. It is the single definition of "correct" shared
// by the model evaluation and the tiered-agent evaluation.
func (c Case) Correct(got intent.Command) bool {
	return got.Intent == c.Intent && c.matches(got)
}

// LoadCases returns the built-in evaluation set.
func LoadCases() ([]Case, error) {
	var cases []Case
	dec := json.NewDecoder(strings.NewReader(string(casesJSON)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cases); err != nil {
		return nil, fmt.Errorf("eval: read cases.json: %w", err)
	}
	return cases, nil
}

// Config describes one evaluation run: one model with one setup.
type Config struct {
	Client      llm.Client
	Model       string
	UseSchema   bool
	MaxAttempts int // 0 means structured.DefaultMaxAttempts
	Limit       int // run only the first N cases; 0 means all
	CaseTimeout time.Duration
	Warmup      bool // send one throwaway request first so loading the model is not timed
	Progress    func(format string, args ...any)
}

// attempts returns the effective attempt limit.
func (c Config) attempts() int {
	if c.MaxAttempts == 0 {
		return structured.DefaultMaxAttempts
	}
	return c.MaxAttempts
}

// CaseResult is what happened for one case.
type CaseResult struct {
	ID       string        `json:"id"`
	Text     string        `json:"text"`
	Expected intent.Intent `json:"expected"`
	Got      intent.Intent `json:"got,omitempty"` // empty when no valid reply was ever produced

	Attempts      int  `json:"attempts"`
	CleanFirstTry bool `json:"clean_first_try"` // first reply was exactly a JSON object of the right shape, nothing around it
	ValidFirstTry bool `json:"valid_first_try"` // ...and also passed validation
	Succeeded     bool `json:"succeeded"`       // some attempt was accepted
	IntentCorrect bool `json:"intent_correct"`
	ArgsChecked   bool `json:"args_checked"`
	ArgsCorrect   bool `json:"args_correct"`
	Repaired      bool `json:"repaired"` // surrounding text had to be cut off a reply

	Seconds float64 `json:"seconds"`
	Error   string  `json:"error,omitempty"`
}

// Summary is the outcome of one run.
type Summary struct {
	Model       string `json:"model"`
	UseSchema   bool   `json:"use_schema"`
	MaxAttempts int    `json:"max_attempts"`
	Cases       int    `json:"cases"`

	CleanFirstTry int `json:"clean_first_try"`
	ValidFirstTry int `json:"valid_first_try"`
	Succeeded     int `json:"succeeded"`
	IntentCorrect int `json:"intent_correct"`
	ArgsChecked   int `json:"args_checked"`
	ArgsCorrect   int `json:"args_correct"`
	Rescued       int `json:"rescued"`  // failed the first try, accepted on a retry
	Repaired      int `json:"repaired"` // cases where at least one reply needed surrounding text removed
	Errors        int `json:"errors"`   // cases that failed for reasons other than a bad reply, e.g. a timeout

	TotalAttempts int     `json:"total_attempts"`
	MedianSeconds float64 `json:"median_seconds"`

	Results []CaseResult `json:"results"`
}

func (s Summary) CleanRate() Proportion      { return Proportion{s.CleanFirstTry, s.Cases} }
func (s Summary) ValidFirstRate() Proportion { return Proportion{s.ValidFirstTry, s.Cases} }
func (s Summary) SucceededRate() Proportion  { return Proportion{s.Succeeded, s.Cases} }
func (s Summary) IntentRate() Proportion     { return Proportion{s.IntentCorrect, s.Cases} }
func (s Summary) ArgsRate() Proportion       { return Proportion{s.ArgsCorrect, s.ArgsChecked} }

// AvgAttempts is the mean number of model calls per case.
func (s Summary) AvgAttempts() float64 {
	if s.Cases == 0 {
		return 0
	}
	return float64(s.TotalAttempts) / float64(s.Cases)
}

// Run evaluates cfg.Model on cases.
//
// A model that produces bad replies is a RESULT and is counted. A backend that
// is down, or a model that is not installed, makes every case meaningless, so
// Run stops and returns that error together with what it has so far. A
// cancelled context does the same.
func Run(ctx context.Context, cfg Config, cases []Case) (Summary, error) {
	progress := cfg.Progress
	if progress == nil {
		progress = func(string, ...any) {}
	}
	if cfg.Limit > 0 && cfg.Limit < len(cases) {
		cases = cases[:cfg.Limit]
	}
	sum := Summary{Model: cfg.Model, UseSchema: cfg.UseSchema, MaxAttempts: cfg.attempts()}
	classifier := intent.Classifier{
		Client: cfg.Client, Model: cfg.Model, UseSchema: cfg.UseSchema, MaxAttempts: cfg.MaxAttempts,
	}

	if cfg.Warmup {
		progress("[%s] warming up...", cfg.Model)
		if _, err := classify(ctx, classifier, cfg.CaseTimeout, "hello"); isFatal(ctx, err) {
			return sum, err
		}
	}

	var times []float64
	for i, c := range cases {
		progress("[%s] case %d/%d: %s", cfg.Model, i+1, len(cases), c.Text)
		res, err := classify(ctx, classifier, cfg.CaseTimeout, c.Text)
		if isFatal(ctx, err) {
			return sum, err
		}

		cr := CaseResult{ID: c.ID, Text: c.Text, Expected: c.Intent, Attempts: len(res.Attempts)}
		var seconds float64
		for _, at := range res.Attempts {
			seconds += at.Usage.Total.Seconds()
			cr.Repaired = cr.Repaired || at.Repaired
		}
		cr.Seconds = seconds
		if len(res.Attempts) > 0 {
			// "Clean" is strict: well-formed AND nothing had to be cut off
			// around it. Counting repaired replies as clean would flatter a
			// model (or a prompt) that rambles around its JSON.
			cr.CleanFirstTry = res.Attempts[0].Parsed && !res.Attempts[0].Repaired
			cr.ValidFirstTry = res.Attempts[0].Err == nil
		}

		switch {
		case err == nil:
			cr.Succeeded = true
			cr.Got = res.Value.Intent
			cr.IntentCorrect = cr.Got == c.Intent
			if c.HasArgs() {
				cr.ArgsChecked = true
				cr.ArgsCorrect = cr.IntentCorrect && c.matches(res.Value)
			}
		case errors.Is(err, structured.ErrNoValidReply):
			// A bad-reply failure: this is what we are measuring. Keep only the
			// reason the last reply was refused; the wrapper text adds nothing.
			cr.Error = err.Error()
			if n := len(res.Attempts); n > 0 && res.Attempts[n-1].Err != nil {
				cr.Error = res.Attempts[n-1].Err.Error()
			}
		default:
			cr.Error = err.Error() // e.g. a timeout on this one case
			sum.Errors++
		}
		if c.HasArgs() {
			sum.ArgsChecked++
		}

		sum.Cases++
		sum.TotalAttempts += cr.Attempts
		count(&sum.CleanFirstTry, cr.CleanFirstTry)
		count(&sum.ValidFirstTry, cr.ValidFirstTry)
		count(&sum.Succeeded, cr.Succeeded)
		count(&sum.IntentCorrect, cr.IntentCorrect)
		count(&sum.ArgsCorrect, cr.ArgsCorrect)
		count(&sum.Rescued, cr.Succeeded && !cr.ValidFirstTry)
		count(&sum.Repaired, cr.Repaired)
		times = append(times, seconds)
		sum.Results = append(sum.Results, cr)
	}
	sum.MedianSeconds = median(times)
	return sum, nil
}

func classify(ctx context.Context, c intent.Classifier, timeout time.Duration, text string) (structured.Result[intent.Command], error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	return c.Classify(ctx, text)
}

// isFatal reports whether err means the whole run should stop. A single case
// timing out is not fatal, but the caller cancelling is, and so is a backend
// that is missing or has no such model.
func isFatal(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	var notFound *llm.ModelNotFoundError
	return ctx.Err() != nil || errors.Is(err, llm.ErrUnavailable) || errors.As(err, &notFound) ||
		errors.Is(err, llm.ErrInvalidRequest)
}

func count(n *int, cond bool) {
	if cond {
		*n++
	}
}
