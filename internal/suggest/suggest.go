// Package suggest is the wall between AI output and a person's tasks.
//
// A task store given to a model-driven component is wrapped in a Store from
// this package. Reading and creating pass through (a brand-new task cannot
// overwrite anything), but every change to an EXISTING task (a status, a
// priority, a note) is turned into a recorded suggestion and reported with
// *Suggested instead of being applied. The task is untouched.
//
// Because the wrapper implements task.Store, the tools and the rest of the
// agent do not know it is there, and no tool can be added later that forgets
// the rule: the way to write an existing task is not reachable from the model's
// side at all.
package suggest

import (
	"context"
	"fmt"
	"strings"

	"offgrid/internal/task"
)

// Store wraps a task.Store for use by AI code.
type Store struct {
	task.Store // reads and Create pass straight through

	Sink task.Suggester // where proposals go
	By   string         // who is proposing, e.g. the model name
	Key  string         // makes a retried job propose once (may be empty)
}

var _ task.Store = Store{}

// Suggested is returned INSTEAD of applying a change. It is an error so that
// no caller can mistake it for success, and so existing error handling stops.
type Suggested struct {
	Suggestion task.Suggestion
	Created    bool // false: the same job had already proposed this
}

func (s *Suggested) Error() string { return "suggested, not applied: " + Describe(s.Suggestion) }

// Describe says what a suggestion would do, in words.
func Describe(sg task.Suggestion) string {
	var parts []string
	if sg.Status != nil {
		parts = append(parts, fmt.Sprintf("set status to %s", *sg.Status))
	}
	if sg.Priority != nil {
		parts = append(parts, fmt.Sprintf("set priority to %s", *sg.Priority))
	}
	if sg.Note != "" {
		parts = append(parts, fmt.Sprintf("add the note %q", sg.Note))
	}
	return fmt.Sprintf("%s on %s", strings.Join(parts, " and "), sg.Task)
}

// Update proposes the change. Fields that already have the proposed value are
// dropped; if nothing is left it reports "no change" like a normal store.
func (s Store) Update(ctx context.Context, id string, c task.Changes) (task.Task, bool, error) {
	cur, err := s.Store.Get(ctx, id)
	if err != nil {
		return task.Task{}, false, err
	}
	var d task.Draft
	d.Task, d.By, d.Key = id, s.By, s.Key
	if c.Status != nil && *c.Status != cur.Status {
		d.Status = c.Status
	}
	if c.Priority != nil && *c.Priority != cur.Priority {
		d.Priority = c.Priority
	}
	if d.Status == nil && d.Priority == nil {
		return cur, false, nil
	}
	return s.propose(ctx, cur, d)
}

// AddNote proposes the note.
func (s Store) AddNote(ctx context.Context, id, text string) (task.Task, error) {
	cur, err := s.Store.Get(ctx, id)
	if err != nil {
		return task.Task{}, err
	}
	_, _, err = s.propose(ctx, cur, task.Draft{Task: id, Note: text, By: s.By, Key: s.Key})
	return cur, err
}

func (s Store) propose(ctx context.Context, cur task.Task, d task.Draft) (task.Task, bool, error) {
	sg, created, err := s.Sink.Suggest(ctx, d)
	if err != nil {
		return cur, false, err
	}
	return cur, false, &Suggested{Suggestion: sg, Created: created}
}
