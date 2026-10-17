package drain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"offgrid/internal/agent"
	"offgrid/internal/intent"
	"offgrid/internal/jobqueue"
	"offgrid/internal/llm"
	"offgrid/internal/structured"
	"offgrid/internal/task"
	"offgrid/internal/tools"
)

// Kinds the router (Day 10) puts in the queue. They are repeated here as
// plain strings on purpose: this package does not import the router.
const (
	KindInterpret  = "interpret"
	KindDeepThink  = "deep_think"
	markExecuting  = "executing"
	maxResultBytes = 8 << 10
)

type textPayload struct {
	Text string `json:"text"`
}

func decodeText(raw json.RawMessage) (string, error) {
	var p textPayload
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return "", Permanent(fmt.Errorf("the job's payload is not {\"text\": ...}: %w", err))
	}
	if strings.TrimSpace(p.Text) == "" {
		return "", Permanent(errors.New("the job's text is empty"))
	}
	return p.Text, nil
}

// mutating says whether running a command can change the person's tasks.
func mutating(i intent.Intent) bool {
	switch i {
	case intent.CreateTask, intent.UpdateTask, intent.AddNote:
		return true
	}
	return false
}

// Interpret handles "interpret" jobs: a message the rules could not read and
// the model could not (or was not there to) understand when it arrived. Now
// the model is back, so read it and run it.
//
// This handler changes the person's tasks, and that is where "run again after
// a crash" becomes dangerous: a repeated "create a task" makes two tasks.
// Two kinds of safety exist, and this handler uses the second:
//
//	idempotent  repeating is harmless (a read-only question)
//	at-most-once  do not repeat if unsure; ask a person
//
// Before it runs a command that can change anything, it saves the checkpoint
// "executing". If the process dies between that line and the end, the next
// attempt finds the marker and does NOT run the command again: it sends the
// job to the dead-letter state with a message saying the task list should be
// checked by hand. That costs a human a glance in a very rare case, and it
// can never create a duplicate task. The long part of the job (the model
// reading the message) comes BEFORE the marker, so a crash there is simply
// retried.
func Interpret(a *agent.Agent) Spec {
	return Spec{Handle: func(ctx context.Context, job jobqueue.Job, p Progress) (string, error) {
		text, err := decodeText(job.Payload)
		if err != nil {
			return "", err
		}
		if job.Progress == markExecuting {
			return "", Permanent(errors.New("this job may already have changed your tasks before the program stopped, " +
				"so it was not run again. Check the task list; if the change is missing, requeue it with: offgrid queue retry " + fmt.Sprint(job.ID)))
		}
		if a.Model == nil {
			return "", agent.ErrNoModel
		}
		res, err := a.Model.Classify(ctx, text)
		if err != nil {
			if errors.Is(err, structured.ErrNoValidReply) {
				return "", Permanent(fmt.Errorf("the model could not turn this into a valid command: %w", err))
			}
			return "", err // unavailable, too slow, cancelled: retry later
		}
		cmd := res.Value
		if mutating(cmd.Intent) {
			if err := p.Save(ctx, markExecuting); err != nil {
				return "", fmt.Errorf("saving progress before changing anything: %w", err)
			}
		}
		out, err := a.Registry.Execute(ctx, cmd)
		var argErr *tools.ArgumentError
		switch {
		case err == nil:
			return out.Message, nil
		case errors.Is(err, tools.ErrNoTool):
			return out.Message, nil // "that is not something Offgrid can do" is an answer
		case errors.As(err, &argErr), errors.Is(err, task.ErrNotFound), errors.Is(err, task.ErrAmbiguous):
			return "Not done: " + err.Error(), nil // refused, nothing changed: also an answer
		default:
			return "", err
		}
	}}
}

// DeepThinkConfig configures the handler for "deep_think" jobs.
type DeepThinkConfig struct {
	Client llm.Client
	Model  string
	// NeedsNetwork is true when Client reaches another machine (a bigger model
	// on a server, a cloud model): the worker then runs these jobs only while
	// the network is up. False for a model on this laptop.
	NeedsNetwork bool
	// Context supplies the data the answer must be based on (today: the task
	// list). Without it a model answering "summarize this week's incidents"
	// can only make things up.
	Context func(ctx context.Context) (string, error)
}

const deepSystem = "You help a field crew supervisor. Answer the question using ONLY the task data given. " +
	"If the data does not contain what is needed, say what is missing instead of guessing. " +
	"Be concrete and brief: at most 8 lines."

// DeepThink handles "deep_think" jobs: requests for judgement that the small
// local rules cannot answer. It only reads, so repeating it after a crash is
// harmless, and the result is stored atomically with completion.
func DeepThink(cfg DeepThinkConfig) Spec {
	return Spec{NeedsNetwork: cfg.NeedsNetwork, Handle: func(ctx context.Context, job jobqueue.Job, _ Progress) (string, error) {
		text, err := decodeText(job.Payload)
		if err != nil {
			return "", err
		}
		data := ""
		if cfg.Context != nil {
			if data, err = cfg.Context(ctx); err != nil {
				return "", fmt.Errorf("reading the task data: %w", err)
			}
		}
		prompt := "Task data:\n" + strings.TrimSpace(data) + "\n\nQuestion: " + text
		resp, err := cfg.Client.Chat(ctx, llm.Request{
			Model: cfg.Model,
			Messages: []llm.Message{
				{Role: llm.RoleSystem, Content: deepSystem},
				{Role: llm.RoleUser, Content: prompt},
			},
			MaxTokens:   400,
			Temperature: 0.2,
		}, nil)
		var nf *llm.ModelNotFoundError
		switch {
		case errors.As(err, &nf):
			return "", Permanent(fmt.Errorf("the model %q is not installed there (ollama pull %s): %w", nf.Model, nf.Model, err))
		case err != nil:
			return "", err
		}
		out := strings.TrimSpace(resp.Text)
		if out == "" {
			return "", errors.New("the model returned an empty answer")
		}
		if len(out) > maxResultBytes {
			out = out[:maxResultBytes]
		}
		return out, nil
	}}
}
