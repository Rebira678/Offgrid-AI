package suggest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"offgrid/internal/intent"
	"offgrid/internal/task"
	"offgrid/internal/tools"
)

type sink struct {
	drafts []task.Draft
	err    error
}

func (s *sink) Suggest(ctx context.Context, d task.Draft) (task.Suggestion, bool, error) {
	if s.err != nil {
		return task.Suggestion{}, false, s.err
	}
	s.drafts = append(s.drafts, d)
	return task.Suggestion{ID: "n.7", Task: d.Task, Status: d.Status, Priority: d.Priority, Note: d.Note, By: d.By, Key: d.Key}, true, nil
}
func (s *sink) Suggestions(context.Context, bool) ([]task.Suggestion, error) { return nil, nil }
func (s *sink) Resolve(context.Context, string, bool) (task.Task, error) {
	panic("the AI side must never be able to resolve a suggestion")
}

func fixture(t *testing.T) (Store, task.Store, *sink) {
	t.Helper()
	now := func() time.Time { return time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC) }
	real := task.NewMemStore(now)
	if _, _, err := real.Create(context.Background(), task.NewTask{Title: "pump", Site: "4", Priority: task.PriorityMedium}); err != nil {
		t.Fatal(err)
	}
	sk := &sink{}
	return Store{Store: real, Sink: sk, By: "qwen", Key: "job-9"}, real, sk
}

func TestAnUpdateBecomesASuggestionAndTheTaskIsUntouched(t *testing.T) {
	ctx := context.Background()
	s, real, sk := fixture(t)
	hi := task.PriorityHigh
	got, changed, err := s.Update(ctx, "T1", task.Changes{Priority: &hi})
	var sug *Suggested
	if !errors.As(err, &sug) || changed || !sug.Created {
		t.Fatalf("err=%v changed=%v", err, changed)
	}
	if got.Priority != task.PriorityMedium {
		t.Fatalf("the returned task shows the change: %+v", got)
	}
	if cur, _ := real.Get(ctx, "T1"); cur.Priority != task.PriorityMedium || !cur.UpdatedAt.Equal(got.UpdatedAt) {
		t.Fatalf("the real task changed: %+v", cur)
	}
	if len(sk.drafts) != 1 || sk.drafts[0].Task != "T1" || *sk.drafts[0].Priority != task.PriorityHigh || sk.drafts[0].By != "qwen" || sk.drafts[0].Key != "job-9" || sk.drafts[0].Status != nil {
		t.Fatalf("%+v", sk.drafts)
	}
	if !strings.Contains(err.Error(), "suggested, not applied") || !strings.Contains(err.Error(), "set priority to high on T1") {
		t.Fatalf("%v", err)
	}
}

func TestOnlyTheFieldsThatWouldChangeAreProposed(t *testing.T) {
	ctx := context.Background()
	s, _, sk := fixture(t)
	same, done := task.PriorityMedium, task.StatusDone
	if _, _, err := s.Update(ctx, "T1", task.Changes{Priority: &same, Status: &done}); err == nil {
		t.Fatal("expected a suggestion for the status")
	}
	if len(sk.drafts) != 1 || sk.drafts[0].Priority != nil || *sk.drafts[0].Status != task.StatusDone {
		t.Fatalf("%+v", sk.drafts)
	}
	// and the same for a status that already has the proposed value
	sk.drafts = nil
	open := task.StatusOpen
	hi := task.PriorityHigh
	if _, _, err := s.Update(ctx, "T1", task.Changes{Status: &open, Priority: &hi}); err == nil {
		t.Fatal("expected a suggestion for the priority")
	}
	if len(sk.drafts) != 1 || sk.drafts[0].Status != nil || *sk.drafts[0].Priority != task.PriorityHigh {
		t.Fatalf("%+v", sk.drafts)
	}
	// nothing would change: behaves like a normal store, proposes nothing
	sk.drafts = nil
	if _, changed, err := s.Update(ctx, "T1", task.Changes{Priority: &same}); err != nil || changed || len(sk.drafts) != 0 {
		t.Fatalf("err=%v changed=%v drafts=%v", err, changed, sk.drafts)
	}
}

func TestANoteBecomesASuggestionAndNoNoteIsAdded(t *testing.T) {
	ctx := context.Background()
	s, real, sk := fixture(t)
	_, err := s.AddNote(ctx, "T1", "seal is cracked")
	var sug *Suggested
	if !errors.As(err, &sug) || len(sk.drafts) != 1 || sk.drafts[0].Note != "seal is cracked" {
		t.Fatalf("err=%v drafts=%+v", err, sk.drafts)
	}
	if cur, _ := real.Get(ctx, "T1"); len(cur.Notes) != 0 {
		t.Fatal("a note was written")
	}
}

func TestCreatingAndReadingPassThroughAndMissingTasksAreNotSuggestedAbout(t *testing.T) {
	ctx := context.Background()
	s, real, sk := fixture(t)
	tk, created, err := s.Create(ctx, task.NewTask{Title: "valve", Site: "2", Priority: task.PriorityLow})
	if err != nil || !created {
		t.Fatal(err)
	}
	if _, err := real.Get(ctx, tk.ID); err != nil {
		t.Fatal("a created task must really exist")
	}
	if l, _ := s.List(ctx, task.Filter{}); len(l) != 2 {
		t.Fatal("List must see the real tasks")
	}
	hi := task.PriorityHigh
	if _, _, err := s.Update(ctx, "T9", task.Changes{Priority: &hi}); !errors.Is(err, task.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if _, err := s.AddNote(ctx, "T9", "x"); !errors.Is(err, task.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if len(sk.drafts) != 0 {
		t.Fatal("proposed a change to a task that does not exist")
	}
	sk.err = errors.New("disk full")
	if _, _, err := s.Update(ctx, "T1", task.Changes{Priority: &hi}); err == nil || errors.As(err, new(*Suggested)) {
		t.Fatalf("a failed save must be an error, not a suggestion: %v", err)
	}
}

// Through the real tools: no command a model can issue changes an existing task.
func TestNoToolCommandChangesAnExistingTask(t *testing.T) {
	ctx := context.Background()
	s, real, sk := fixture(t)
	reg := tools.NewRegistry(s, time.Now)
	before, _ := real.Get(ctx, "T1")
	for _, cmd := range []intent.Command{
		{Intent: intent.UpdateTask, TaskRef: "T1", Status: "done"},
		{Intent: intent.UpdateTask, TaskRef: "pump", Priority: "high"},
		{Intent: intent.AddNote, TaskRef: "T1", Note: "leaking again"},
	} {
		_, err := reg.Execute(ctx, cmd)
		if !errors.As(err, new(*Suggested)) {
			t.Fatalf("%+v: %v", cmd, err)
		}
	}
	after, _ := real.Get(ctx, "T1")
	if before.Status != after.Status || before.Priority != after.Priority || len(after.Notes) != 0 || !before.UpdatedAt.Equal(after.UpdatedAt) {
		t.Fatalf("%+v -> %+v", before, after)
	}
	if len(sk.drafts) != 3 {
		t.Fatalf("%d suggestions", len(sk.drafts))
	}
}
