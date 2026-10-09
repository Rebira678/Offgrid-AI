// Package tools is where a model's proposal becomes an action, or does not.
//
// The rule of the whole project, from Day 4 on: THE MODEL PROPOSES, THE CODE
// DISPOSES. A model reply is only ever a proposal (an intent.Command). This
// package decides what happens to it, and it decides without trusting
// anything:
//
//   - Every command is validated again here, even if the caller says it
//     already was. The registry does not know who is calling it.
//   - Each tool turns the loose strings of a Command into its own typed
//     arguments (task.Priority, task.Status...) and rejects what does not fit.
//   - The only way to change a task is through a tool. There is no "run this
//     SQL" and no "call this function by name": the model can select among five
//     fixed actions, nothing more.
//   - A tool never guesses. An unclear reference is an error, not a coin toss.
//
// This package imports no model code. A test enforces it, so it can be tested
// without a model, and a model can never be a dependency of the rules.
package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"offgrid/internal/intent"
	"offgrid/internal/task"
)

// Result is what a tool did, in a form both a person and a program can use.
type Result struct {
	Tool    intent.Intent
	Message string // one human-readable answer
	Changed bool   // true if the store was modified

	Task    *task.Task  // the task acted on, for create/update/add_note
	Tasks   []task.Task // matching tasks, for list_tasks
	Summary *Summary    // for daily_summary
}

// Summary is the numbers behind a daily_summary.
type Summary struct {
	Range            string
	Created          int // tasks created in the range
	Completed        int // tasks finished in the range
	NotesAdded       int // notes written in the range
	StillOpen        int // tasks not done right now, whatever their age
	HighPriorityOpen int // ...of which high priority
}

var (
	// ErrNoTool means the command is valid but is not something a tool does:
	// the "unknown" intent. It is a normal, expected outcome, not a failure.
	ErrNoTool = errors.New("no tool for this intent")
)

// ArgumentError means the command was refused before anything ran. Problems
// are written as plain sentences, because they will be shown to a person, and
// later (Day 5) fed back to the model.
type ArgumentError struct {
	Tool     intent.Intent
	Problems []string
}

func (e *ArgumentError) Error() string {
	return strings.Join(e.Problems, "; ")
}

// ArgSpec documents one argument of a tool.
type ArgSpec struct {
	Field    string // the Command field it reads, e.g. "task_ref"
	Required bool
	Doc      string
}

// Spec documents a tool.
type Spec struct {
	Intent intent.Intent
	Does   string
	Args   []ArgSpec
}

// env is what a running tool is given: the store and one fixed moment in time,
// so a single command sees a single "now".
type env struct {
	store task.Store
	now   time.Time
}

type runner func(ctx context.Context, e env, cmd intent.Command) (Result, error)

type entry struct {
	spec Spec
	run  runner
}

// Registry maps each intent to the tool that handles it.
type Registry struct {
	store   task.Store
	clock   func() time.Time
	entries map[intent.Intent]entry
	order   []intent.Intent
}

// NewRegistry builds the registry with the five tools. clock supplies "now"
// (nil means time.Now); tests pass a fixed one.
func NewRegistry(store task.Store, clock func() time.Time) *Registry {
	if clock == nil {
		clock = time.Now
	}
	r := &Registry{store: store, clock: clock, entries: map[intent.Intent]entry{}}
	register(r, createTaskSpec, parseCreate, doCreate)
	register(r, updateTaskSpec, parseUpdate, doUpdate)
	register(r, listTasksSpec, parseList, doList)
	register(r, addNoteSpec, parseNote, doNote)
	register(r, dailySummarySpec, parseSummary, doSummary)
	return r
}

// register adds one tool. A tool has two halves with different jobs:
//
//	parse: Command -> typed arguments A. Pure, touches no store, and is where
//	       every argument is checked.
//	do:    typed arguments -> effect. Cannot even see the loose strings; it can
//	       only use what parse approved.
//
// The type parameter A is what keeps the halves honest: do for create_task
// takes CreateTaskArgs and nothing else.
func register[A any](r *Registry, spec Spec, parse func(intent.Command) (A, error), do func(context.Context, env, A) (Result, error)) {
	r.entries[spec.Intent] = entry{spec: spec, run: func(ctx context.Context, e env, cmd intent.Command) (Result, error) {
		args, err := parse(cmd)
		if err != nil {
			return Result{}, err
		}
		return do(ctx, e, args)
	}}
	r.order = append(r.order, spec.Intent)
}

// Specs lists the tools in a stable order.
func (r *Registry) Specs() []Spec {
	out := make([]Spec, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.entries[id].spec)
	}
	return out
}

// Execute validates cmd and, only if it passes every check, runs its tool.
//
// Errors fall into three kinds a caller can tell apart:
//
//	*ArgumentError      the command was refused; nothing ran
//	task.ErrNotFound / task.ErrAmbiguous
//	                    the command was fine but the task it names is not
//	                    identifiable; nothing changed
//	ErrNoTool           the "unknown" intent; nothing ran
//
// Anything else (a cancelled context, a failing store) is infrastructure.
func (r *Registry) Execute(ctx context.Context, cmd intent.Command) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	// cmd is a copy, so normalizing it here never touches the caller's value.
	if err := intent.Validate(&cmd); err != nil {
		var ve *intent.ValidationError
		if errors.As(err, &ve) {
			return Result{}, &ArgumentError{Tool: cmd.Intent, Problems: ve.Problems}
		}
		return Result{}, err
	}
	e, ok := r.entries[cmd.Intent]
	if !ok {
		return Result{Tool: cmd.Intent, Message: "That is not something Offgrid can do. It can create, update and list tasks, add notes, and summarise the day."},
			fmt.Errorf("%w: %q", ErrNoTool, cmd.Intent)
	}
	res, err := e.run(ctx, env{store: r.store, now: r.clock()}, cmd)
	res.Tool = cmd.Intent
	return res, err
}

// problems collects everything wrong with a command, so a person (or the
// model) hears all of it at once.
type problems []string

func (p *problems) addf(format string, a ...any) { *p = append(*p, fmt.Sprintf(format, a...)) }

func (p problems) err(tool intent.Intent) error {
	if len(p) == 0 {
		return nil
	}
	return &ArgumentError{Tool: tool, Problems: p}
}
