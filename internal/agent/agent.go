// Package agent is the loop that turns a worker's message into an action:
//
//	message -> Tier 0 rules (microseconds, no model)
//	        -> Tier 1 small model (seconds), only for what the rules cannot read
//	        -> tools.Registry (validate, then act)
//
// Every request is timed and labelled with the tier that handled it. The
// principle is "don't call an LLM when a regex will do": the model is the
// expensive, slow, fallible part, so it is the fallback, not the default.
//
// Both tiers end at the same place. A rule that misfires is exactly as
// contained as a model that does, because the registry (Day 4) validates and
// bounds every command no matter who proposed it.
package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"offgrid/internal/intent"
	"offgrid/internal/llm"
	"offgrid/internal/structured"
	"offgrid/internal/task"
	"offgrid/internal/tools"
)

// Tier says who understood the message.
type Tier int

const (
	TierNone  Tier = -1 // nobody: the message was not understood
	TierRules Tier = 0
	TierModel Tier = 1
)

func (t Tier) String() string {
	switch t {
	case TierRules:
		return "tier 0 (rules)"
	case TierModel:
		return "tier 1 (model)"
	}
	return "none"
}

// Status is the outcome in one word.
type Status string

const (
	StatusOK            Status = "ok"             // understood and carried out
	StatusRefused       Status = "refused"        // understood, but the code said no (missing task, ambiguous, invalid)
	StatusDeclined      Status = "declined"       // understood: this is not something Offgrid does
	StatusNotUnderstood Status = "not_understood" // the model could not produce a valid command
	StatusFailed        Status = "failed"         // infrastructure: no model, model down, timeout, cancelled
)

// ErrNoModel means a message needed the model and none is configured.
var ErrNoModel = errors.New("this message needs the language model, and none is available")

// Model is what Tier 1 needs. intent.Classifier satisfies it; tests pass fakes.
type Model interface {
	Classify(ctx context.Context, text string) (structured.Result[intent.Command], error)
}

// Agent runs the loop.
type Agent struct {
	Rules    Rules
	Model    Model // may be nil: then only the rules work
	Registry *tools.Registry
	Now      func() time.Time // for measuring latency; nil means time.Now
}

// Outcome is everything that happened to one message.
type Outcome struct {
	Text   string
	Tier   Tier
	Rule   string // which rule matched, for Tier 0
	Status Status
	Err    error

	Command intent.Command // what was proposed; zero if nothing was understood
	Result  tools.Result

	Attempts int    // model calls made (Tier 1)
	Fallback string // why a Tier 0 match was not used, if it was not

	Understand time.Duration // reading the message: the rule, or the model call(s)
	Execute    time.Duration // running the command
	Total      time.Duration
}

// Message is one line a person can read.
func (o Outcome) Message() string {
	switch o.Status {
	case StatusOK, StatusDeclined:
		return o.Result.Message
	case StatusRefused:
		if errors.Is(o.Err, task.ErrNotFound) {
			return "No task matches that. Nothing was changed."
		}
		return o.Err.Error()
	case StatusNotUnderstood:
		return "I could not turn that into a command. Try saying it another way."
	default:
		if errors.Is(o.Err, ErrNoModel) {
			return "I could not read that with rules alone, and the language model is not available. Try a plainer phrase, such as \"list open tasks\"."
		}
		if errors.Is(o.Err, llm.ErrUnavailable) {
			return "I could not read that with rules alone, and the language model is not reachable (is Ollama running?)."
		}
		return fmt.Sprintf("Something went wrong: %v", o.Err)
	}
}

// Handle processes one message. It never panics on any input and never returns
// an error: whatever happened is described by the Outcome.
func (a *Agent) Handle(ctx context.Context, text string) Outcome {
	now := a.Now
	if now == nil {
		now = time.Now
	}
	start := now()
	out := Outcome{Text: text, Tier: TierNone}
	finish := func() Outcome {
		out.Total = now().Sub(start)
		return out
	}

	// An empty message is not worth a model call, which costs seconds.
	if strings.TrimSpace(text) == "" {
		out.Status, out.Err = StatusNotUnderstood, errors.New("empty message")
		return finish()
	}

	// ---- Tier 0 ----
	if cmd, rule, ok := a.Rules.Match(text); ok {
		out.Understand = now().Sub(start)
		out.Tier, out.Rule, out.Command = TierRules, rule, cmd
		execStart := now()
		res, err := a.Registry.Execute(ctx, cmd)
		out.Execute = now().Sub(execStart)

		var argErr *tools.ArgumentError
		if errors.As(err, &argErr) {
			// A rule produced something the registry refuses. That is a bug in
			// the rule, not the worker's fault, so do not blame them: note it
			// and let the model have a go.
			out.Fallback = fmt.Sprintf("rule %q proposed a command the registry refused (%v)", rule, err)
		} else {
			out.Result = res
			out.Status, out.Err = classify(err)
			return finish()
		}
	}

	// ---- Tier 1 ----
	if a.Model == nil {
		out.Tier = TierNone
		out.Status, out.Err = StatusFailed, ErrNoModel
		return finish()
	}
	res, err := a.Model.Classify(ctx, text)
	out.Understand = now().Sub(start)
	out.Attempts = len(res.Attempts)
	if err != nil {
		out.Tier = TierNone
		out.Err = err
		if errors.Is(err, structured.ErrNoValidReply) {
			out.Status = StatusNotUnderstood
		} else {
			out.Status = StatusFailed
		}
		return finish()
	}
	out.Tier, out.Rule, out.Command = TierModel, "", res.Value

	execStart := now()
	result, err := a.Registry.Execute(ctx, res.Value)
	out.Execute = now().Sub(execStart)
	out.Result = result
	out.Status, out.Err = classify(err)
	return finish()
}

// classify sorts a registry result into a Status.
func classify(err error) (Status, error) {
	var argErr *tools.ArgumentError
	switch {
	case err == nil:
		return StatusOK, nil
	case errors.Is(err, tools.ErrNoTool):
		return StatusDeclined, err
	case errors.As(err, &argErr), errors.Is(err, task.ErrNotFound), errors.Is(err, task.ErrAmbiguous):
		return StatusRefused, err
	default:
		return StatusFailed, err
	}
}
