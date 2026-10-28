package drain

import (
	"context"
	"errors"
	"fmt"
	"time"

	"offgrid/internal/agent"
	"offgrid/internal/intent"
	"offgrid/internal/jobqueue"
	"offgrid/internal/structured"
	"offgrid/internal/suggest"
	"offgrid/internal/task"
	"offgrid/internal/tools"
)

// InterpretSafelyConfig configures InterpretSafely.
type InterpretSafelyConfig struct {
	Agent *agent.Agent // its Model reads the message; its Registry is NOT used
	Store task.Store   // the real store (reads, and creating new tasks)
	Sink  task.Suggester
	Clock func() time.Time
	By    string // recorded on every suggestion, e.g. the model name
}

// InterpretSafely is Interpret with one rule added: the model may create new
// tasks, but it may not change an existing one. A queued message is read hours
// after it was typed, by a model, while people may have edited the task in the
// meantime; writing it straight in could overwrite them. So an update or a note
// becomes a suggestion that a person accepts or dismisses, and the task is not
// touched.
//
// Safety of retries: creating a task is not repeatable, so it keeps Interpret's
// "executing" checkpoint (a crash then goes to a person). A suggestion IS
// repeatable (the job's key makes the store propose once), so it needs none.
func InterpretSafely(cfg InterpretSafelyConfig) Spec {
	return Spec{Handle: func(ctx context.Context, job jobqueue.Job, p Progress) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		text, err := decodeText(job.Payload)
		if err != nil {
			return "", err
		}
		if job.Progress == markExecuting {
			return "", Permanent(errors.New("this job may already have created a task before the program stopped, " +
				"so it was not run again. Check the task list; if the task is missing, requeue it with: offgrid queue retry " + fmt.Sprint(job.ID)))
		}
		if cfg.Agent == nil || cfg.Agent.Model == nil {
			return "", agent.ErrNoModel
		}
		res, err := cfg.Agent.Model.Classify(ctx, text)
		if err != nil {
			if errors.Is(err, structured.ErrNoValidReply) {
				return "", Permanent(fmt.Errorf("the model could not turn this into a valid command: %w", err))
			}
			return "", err
		}
		cmd := res.Value
		if cmd.Intent == intent.CreateTask {
			if err := p.Save(ctx, markExecuting); err != nil {
				return "", fmt.Errorf("saving progress before changing anything: %w", err)
			}
		}
		reg := tools.NewRegistry(suggest.Store{Store: cfg.Store, Sink: cfg.Sink, By: cfg.By, Key: job.Key}, cfg.Clock)
		out, err := reg.Execute(ctx, cmd)
		var argErr *tools.ArgumentError
		var sug *suggest.Suggested
		switch {
		case err == nil:
			return out.Message, nil
		case errors.As(err, &sug):
			return "Suggested, not applied: " + suggest.Describe(sug.Suggestion) +
				". A person decides: offgrid suggestions accept " + sug.Suggestion.ID + " (or dismiss).", nil
		case errors.Is(err, tools.ErrNoTool):
			return out.Message, nil
		case errors.As(err, &argErr), errors.Is(err, task.ErrNotFound), errors.Is(err, task.ErrAmbiguous):
			return "Not done: " + err.Error(), nil
		default:
			return "", err
		}
	}}
}
