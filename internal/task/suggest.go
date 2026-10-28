package task

import (
	"context"
	"errors"
	"time"
)

// A Suggestion is a change an AI proposes. It is NOT applied: it sits next to
// the task, in its own place, until a person accepts it. Nothing a model does
// can write a task field directly, so a human edit can never be overwritten by
// a model that finished thinking later.
//
// The suggestion remembers what the task looked like when it was made (the
// Base fields). If a person has changed that field since, the suggestion is
// Stale: it was written against a task that no longer exists in that form.

// SuggestionState is where a suggestion stands.
type SuggestionState string

const (
	SuggestionPending   SuggestionState = "pending"
	SuggestionAccepted  SuggestionState = "accepted"
	SuggestionDismissed SuggestionState = "dismissed"
)

// Draft is what an AI hands over: which task, which change, who proposes it.
// Key, when set, makes proposing idempotent: a retried job proposes once.
type Draft struct {
	Task     string // task ID
	Status   *Status
	Priority *Priority
	Note     string
	By       string // who proposed it, e.g. the model name
	Key      string
}

// Suggestion is a stored proposal.
type Suggestion struct {
	ID       string // stable on every device: "<node>.<seq>" of the op that made it
	Task     string
	Status   *Status
	Priority *Priority
	Note     string
	// What the task showed when the suggestion was made.
	BaseStatus   Status
	BasePriority Priority
	By           string
	Key          string
	At           time.Time
	State        SuggestionState
	// Stale: still pending, but the task's status or priority (whichever the
	// suggestion would change) is no longer what it was when proposed.
	Stale bool
}

var (
	// ErrStale: the task changed after the suggestion was made, so accepting it
	// could undo a person's edit. Dismiss it, or edit the task by hand.
	ErrStale = errors.New("the task changed after this suggestion was made")
	// ErrSuggestionNotFound: no suggestion has that ID.
	ErrSuggestionNotFound = errors.New("no such suggestion")
	// ErrSuggestionResolved: it was already accepted or dismissed.
	ErrSuggestionResolved = errors.New("this suggestion was already accepted or dismissed")
	// ErrNothingToSuggest: the draft changes nothing.
	ErrNothingToSuggest = errors.New("a suggestion must change a status, a priority, or add a note")
)

// Suggester is the optional part of a store that can keep suggestions. Like
// Remover it is separate from Store: only code that is allowed to handle
// AI output needs it.
type Suggester interface {
	// Suggest records a proposal. created is false when Key was already used
	// (a retried job): the existing suggestion comes back and nothing is added.
	Suggest(ctx context.Context, d Draft) (s Suggestion, created bool, err error)
	// Suggestions lists proposals, oldest first. Only pending ones unless all.
	Suggestions(ctx context.Context, all bool) ([]Suggestion, error)
	// Resolve accepts or dismisses a pending suggestion. It is the ONLY way an
	// AI proposal reaches a task, and only a person (the command line) calls it.
	// Accepting a stale suggestion returns ErrStale and changes nothing.
	Resolve(ctx context.Context, id string, accept bool) (Task, error)
}
