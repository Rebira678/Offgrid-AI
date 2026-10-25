package task

import "context"

// NewTask is what is needed to create a task. The store assigns the ID, the
// status (always open) and the timestamps.
type NewTask struct {
	Title    string
	Site     string
	Priority Priority
}

// Changes lists what to change on an existing task. A nil field is left alone.
type Changes struct {
	Status   *Status
	Priority *Priority
}

// Filter selects tasks. Zero fields match everything.
type Filter struct {
	Status      Status   // only this status
	ExcludeDone bool     // everything except done (ignored when Status is set)
	Priority    Priority // only this priority
	Site        string   // only this site, ignoring case
	Within      *Range   // only tasks created inside this time range
}

// Store keeps tasks. It is described as domain operations, not as SQL, so Day 6
// can add a SQLite implementation behind the same interface and the tools do
// not change. Every implementation must pass storetest.Run.
//
// Returned tasks are copies: changing one never changes the store.
type Store interface {
	// Create adds a task. If an unfinished task with the same title and site
	// already exists, nothing is added and that task comes back with
	// created == false. This makes a repeated message (a retry, a duplicate in
	// the queue) harmless, and the check is atomic inside the store.
	Create(ctx context.Context, n NewTask) (t Task, created bool, err error)

	// Get returns one task by ID, or ErrNotFound.
	Get(ctx context.Context, id string) (Task, error)

	// Update applies changes. changed is false when they were already true.
	Update(ctx context.Context, id string, c Changes) (t Task, changed bool, err error)

	// AddNote attaches a note to a task.
	AddNote(ctx context.Context, id, text string) (Task, error)

	// List returns matching tasks in creation order.
	List(ctx context.Context, f Filter) ([]Task, error)
}

// Remover is the optional part of a store that can delete. It is separate from
// Store on purpose: deleting is not something the language model is allowed to
// ask for (the "unknown" intent exists so that "delete everything" goes
// nowhere), so only the command line and tests reach it.
type Remover interface {
	// DeleteTask removes a task from every list, or returns ErrNotFound.
	DeleteTask(ctx context.Context, id string) error
	// RemoveNote removes the number-th note currently shown on a task
	// (counting from 1) and returns the task, or ErrNotFound.
	RemoveNote(ctx context.Context, id string, number int) (Task, error)
}
