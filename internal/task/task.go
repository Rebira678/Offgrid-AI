// Package task is Offgrid's domain model: what a task is, what can be done to
// one, and the Store interface everything else depends on.
//
// Nothing in this package knows a language model exists. That is deliberate:
// the rules about tasks (what a status is, how "yesterday" is computed, which
// task "the pump" refers to) must be ordinary, testable code that behaves the
// same whatever the model says.
package task

import (
	"errors"
	"fmt"
	"time"
)

// Status is where a task is in its life.
type Status string

const (
	StatusOpen       Status = "open"
	StatusInProgress Status = "in_progress"
	StatusDone       Status = "done"
)

// Priority is how urgent a task is.
type Priority string

const (
	PriorityLow    Priority = "low"
	PriorityMedium Priority = "medium"
	PriorityHigh   Priority = "high"
)

// ParseStatus converts text to a Status, refusing anything that is not one.
func ParseStatus(s string) (Status, error) {
	switch v := Status(s); v {
	case StatusOpen, StatusInProgress, StatusDone:
		return v, nil
	}
	return "", fmt.Errorf("status %q is not one of open, in_progress, done", s)
}

// ParsePriority converts text to a Priority, refusing anything that is not one.
func ParsePriority(s string) (Priority, error) {
	switch v := Priority(s); v {
	case PriorityLow, PriorityMedium, PriorityHigh:
		return v, nil
	}
	return "", fmt.Errorf("priority %q is not one of low, medium, high", s)
}

// Rank orders priorities: high 3, medium 2, low 1, anything else 0.
func (p Priority) Rank() int {
	switch p {
	case PriorityHigh:
		return 3
	case PriorityMedium:
		return 2
	case PriorityLow:
		return 1
	}
	return 0
}

// Task is one piece of work.
type Task struct {
	ID        string
	Title     string
	Site      string
	Priority  Priority
	Status    Status
	Notes     []Note
	CreatedAt time.Time
	UpdatedAt time.Time
	DoneAt    time.Time // zero unless Status is done
}

// Note is a line of free text attached to a task.
type Note struct {
	Text string
	At   time.Time
}

// Sentinel errors. Callers test for them with errors.Is.
var (
	// ErrNotFound: no task matches.
	ErrNotFound = errors.New("task not found")
	// ErrAmbiguous: more than one task matches, and guessing is not allowed.
	ErrAmbiguous = errors.New("task reference is ambiguous")
	// ErrInvalid: the request itself is malformed (empty title, bad enum...).
	ErrInvalid = errors.New("invalid request")
)

// AmbiguousError says which tasks a reference could mean, so a person can pick.
type AmbiguousError struct {
	Ref        string
	Candidates []Task
}

func (e *AmbiguousError) Error() string {
	s := fmt.Sprintf("%q matches %d tasks:", e.Ref, len(e.Candidates))
	for i, t := range e.Candidates {
		sep := " "
		if i > 0 {
			sep = ", "
		}
		s += fmt.Sprintf("%s%s %q", sep, t.ID, t.Title)
	}
	return s + "; say which one, using its ID"
}

// Is makes errors.Is(err, ErrAmbiguous) true.
func (e *AmbiguousError) Is(target error) bool { return target == ErrAmbiguous }
