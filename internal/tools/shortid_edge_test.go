package tools_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"offgrid/internal/intent"
	"offgrid/internal/task"
	"offgrid/internal/tools"
)

// mockMultiReplicaStore simulates a task.Store containing tasks with Day 24 qualified IDs.
type mockMultiReplicaStore struct {
	tasks []task.Task
}

func (m *mockMultiReplicaStore) Create(ctx context.Context, n task.NewTask) (task.Task, bool, error) {
	t := task.Task{
		ID:        n.Title, // arbitrary placeholder
		Title:     n.Title,
		Priority:  n.Priority,
		Site:      n.Site,
		Status:    task.StatusOpen,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	m.tasks = append(m.tasks, t)
	return t, true, nil
}

func (m *mockMultiReplicaStore) Update(ctx context.Context, id string, c task.Changes) (task.Task, bool, error) {
	for i, t := range m.tasks {
		if t.ID == id {
			if c.Priority != nil {
				m.tasks[i].Priority = *c.Priority
			}
			if c.Status != nil {
				m.tasks[i].Status = *c.Status
			}
			return m.tasks[i], true, nil
		}
	}
	return task.Task{}, false, task.ErrNotFound
}

func (m *mockMultiReplicaStore) AddNote(ctx context.Context, id, text string) (task.Task, error) {
	for i, t := range m.tasks {
		if t.ID == id {
			n := task.Note{Text: text, At: time.Now()}
			m.tasks[i].Notes = append(m.tasks[i].Notes, n)
			return m.tasks[i], nil
		}
	}
	return task.Task{}, task.ErrNotFound
}

func (m *mockMultiReplicaStore) Get(ctx context.Context, id string) (task.Task, error) {
	for _, t := range m.tasks {
		if t.ID == id {
			return t, nil
		}
	}
	return task.Task{}, task.ErrNotFound
}

func (m *mockMultiReplicaStore) List(ctx context.Context, f task.Filter) ([]task.Task, error) {
	return append([]task.Task(nil), m.tasks...), nil
}

// TestAgentToolsFormattingAndResolutionWithShortIDs tests that list_tasks and add_note
// handle ShortIDs and qualified IDs correctly during agent tool invocations.
func TestAgentToolsFormattingAndResolutionWithShortIDs(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	s := &mockMultiReplicaStore{
		tasks: []task.Task{
			{ID: "T1.laptop", Title: "Fix laptop hinge", Priority: task.PriorityHigh, Status: task.StatusOpen, CreatedAt: now},
			{ID: "T1.phone", Title: "Order phone battery", Priority: task.PriorityMedium, Status: task.StatusOpen, CreatedAt: now},
			{ID: "T2.laptop", Title: "Clean keyboard", Priority: task.PriorityLow, Status: task.StatusOpen, CreatedAt: now},
		},
	}

	reg := tools.NewRegistry(s, func() time.Time { return now })

	// 1. list_tasks should display qualified IDs for ambiguous T1, but short ID T2 for unique T2
	res, err := reg.Execute(ctx, intent.Command{Intent: intent.ListTasks})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if !strings.Contains(res.Message, "T1.laptop") || !strings.Contains(res.Message, "T1.phone") {
		t.Fatalf("expected qualified IDs for colliding T1 in list_tasks output:\n%s", res.Message)
	}
	if !strings.Contains(res.Message, "T2 [low, open]") {
		t.Fatalf("expected short ID T2 for unique task in list_tasks output:\n%s", res.Message)
	}

	// 2. add_note with ambiguous short ID "T1" must return an ambiguity error
	_, err = reg.Execute(ctx, intent.Command{
		Intent:  intent.AddNote,
		TaskRef: "T1",
		Note:    "Ambiguous note",
	})
	if err == nil {
		t.Fatalf("expected error adding note to ambiguous T1, got nil")
	}
	if !strings.Contains(err.Error(), "matches 2 tasks") {
		t.Fatalf("expected 'matches 2 tasks' error, got: %v", err)
	}

	// 3. add_note with qualified ID "T1.laptop" succeeds
	res, err = reg.Execute(ctx, intent.Command{
		Intent:  intent.AddNote,
		TaskRef: "T1.laptop",
		Note:    "Hinge screw tightened",
	})
	if err != nil {
		t.Fatalf("add_note T1.laptop: %v", err)
	}
	if !strings.Contains(res.Message, "Added a note to T1") {
		t.Fatalf("expected note response with ShortID T1, got: %s", res.Message)
	}

	// 4. add_note with unambiguous short ID "T2" succeeds
	res, err = reg.Execute(ctx, intent.Command{
		Intent:  intent.AddNote,
		TaskRef: "T2",
		Note:    "Compressed air used",
	})
	if err != nil {
		t.Fatalf("add_note T2: %v", err)
	}
	if !strings.Contains(res.Message, "Added a note to T2") {
		t.Fatalf("expected note response with ShortID T2, got: %s", res.Message)
	}
}
