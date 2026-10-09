package task

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// MemStore is an in-memory Store. It is the store for tests and for Day 4's
// no-model demos; Day 6 adds SQLite behind the same interface.
type MemStore struct {
	mu    sync.Mutex
	now   func() time.Time
	next  int
	tasks []*Task
}

var _ Store = (*MemStore)(nil)

// NewMemStore returns an empty store. now supplies the time; passing a fake
// clock makes every timestamp, and so every "today" and "yesterday", exact in
// tests. A nil now means time.Now.
func NewMemStore(now func() time.Time) *MemStore {
	if now == nil {
		now = time.Now
	}
	return &MemStore{now: now, next: 1}
}

func (s *MemStore) Create(ctx context.Context, n NewTask) (Task, bool, error) {
	if err := ctx.Err(); err != nil {
		return Task{}, false, err
	}
	title := strings.TrimSpace(n.Title)
	if title == "" {
		return Task{}, false, fmt.Errorf("%w: empty title", ErrInvalid)
	}
	if _, err := ParsePriority(string(n.Priority)); err != nil {
		return Task{}, false, fmt.Errorf("%w: %v", ErrInvalid, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tasks {
		if t.Status != StatusDone && fold(t.Title) == fold(title) && strings.EqualFold(t.Site, n.Site) {
			return clone(*t), false, nil
		}
	}
	now := s.now()
	t := &Task{
		ID:        fmt.Sprintf("T%d", s.next),
		Title:     title,
		Site:      n.Site,
		Priority:  n.Priority,
		Status:    StatusOpen,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.next++
	s.tasks = append(s.tasks, t)
	return clone(*t), true, nil
}

func (s *MemStore) Get(ctx context.Context, id string) (Task, error) {
	if err := ctx.Err(); err != nil {
		return Task{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.find(id)
	if t == nil {
		return Task{}, ErrNotFound
	}
	return clone(*t), nil
}

func (s *MemStore) Update(ctx context.Context, id string, c Changes) (Task, bool, error) {
	if err := ctx.Err(); err != nil {
		return Task{}, false, err
	}
	if c.Status == nil && c.Priority == nil {
		return Task{}, false, fmt.Errorf("%w: nothing to change", ErrInvalid)
	}
	if c.Status != nil {
		if _, err := ParseStatus(string(*c.Status)); err != nil {
			return Task{}, false, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
	}
	if c.Priority != nil {
		if _, err := ParsePriority(string(*c.Priority)); err != nil {
			return Task{}, false, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.find(id)
	if t == nil {
		return Task{}, false, ErrNotFound
	}
	now := s.now()
	changed := false
	if c.Status != nil && *c.Status != t.Status {
		t.Status = *c.Status
		if t.Status == StatusDone {
			t.DoneAt = now
		} else {
			t.DoneAt = time.Time{} // reopened
		}
		changed = true
	}
	if c.Priority != nil && *c.Priority != t.Priority {
		t.Priority = *c.Priority
		changed = true
	}
	if changed {
		t.UpdatedAt = now
	}
	return clone(*t), changed, nil
}

func (s *MemStore) AddNote(ctx context.Context, id, text string) (Task, error) {
	if err := ctx.Err(); err != nil {
		return Task{}, err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return Task{}, fmt.Errorf("%w: empty note", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.find(id)
	if t == nil {
		return Task{}, ErrNotFound
	}
	now := s.now()
	t.Notes = append(t.Notes, Note{Text: text, At: now})
	t.UpdatedAt = now
	return clone(*t), nil
}

func (s *MemStore) List(ctx context.Context, f Filter) ([]Task, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Task{}
	for _, t := range s.tasks {
		switch {
		case f.Status != "" && t.Status != f.Status:
		case f.Status == "" && f.ExcludeDone && t.Status == StatusDone:
		case f.Priority != "" && t.Priority != f.Priority:
		case f.Site != "" && !strings.EqualFold(t.Site, f.Site):
		case f.Within != nil && !f.Within.Contains(t.CreatedAt):
		default:
			out = append(out, clone(*t))
		}
	}
	return out, nil
}

func (s *MemStore) find(id string) *Task {
	for _, t := range s.tasks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// clone copies a task, including its notes slice, so callers cannot reach into
// the store's data.
func clone(t Task) Task {
	t.Notes = append([]Note(nil), t.Notes...)
	return t
}
