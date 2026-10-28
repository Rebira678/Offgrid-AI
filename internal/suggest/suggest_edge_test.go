package suggest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"offgrid/internal/task"
)

func TestSuggestEdgeUnicodeAndExtremeNotes(t *testing.T) {
	ctx := context.Background()
	s, real, sk := fixture(t)

	// Unicode multi-byte strings: Amharic, Japanese, German, Emoji
	unicodeNote := "በአስቸኳይ የሚስተካከል 🧠⚡️ こんにちは世界 Äpfel Übergröße ∑(x_i) = 100% " + strings.Repeat("🔥", 100)
	got, err := s.AddNote(ctx, "T1", unicodeNote)
	var sug *Suggested
	if !errors.As(err, &sug) {
		t.Fatalf("expected *Suggested error, got %v", err)
	}
	if sug.Suggestion.Note != unicodeNote {
		t.Fatalf("note mismatch: got %q, want %q", sug.Suggestion.Note, unicodeNote)
	}
	if len(sk.drafts) != 1 || sk.drafts[0].Note != unicodeNote {
		t.Fatalf("draft note mismatch: %+v", sk.drafts)
	}
	// Real task must remain untouched
	realTask, _ := real.Get(ctx, "T1")
	if len(realTask.Notes) != 0 || len(got.Notes) != 0 {
		t.Fatalf("task notes modified: real=%d got=%d", len(realTask.Notes), len(got.Notes))
	}
}

func TestSuggestEdgeIdenticalValuesNoOp(t *testing.T) {
	ctx := context.Background()
	s, real, sk := fixture(t)

	realTask, _ := real.Get(ctx, "T1") // Priority is PriorityMedium, Status is StatusOpen
	med := task.PriorityMedium
	open := task.StatusOpen

	// Propose identical priority and status
	got, changed, err := s.Update(ctx, "T1", task.Changes{Priority: &med, Status: &open})
	if err != nil {
		t.Fatalf("unexpected error on identical update: %v", err)
	}
	if changed {
		t.Fatal("expected changed=false for identical update")
	}
	if len(sk.drafts) != 0 {
		t.Fatalf("expected 0 drafts for identical update, got %d", len(sk.drafts))
	}
	if got.Priority != realTask.Priority || got.Status != realTask.Status {
		t.Fatalf("task fields altered: %+v", got)
	}
}

func TestSuggestEdgeNonExistentTask(t *testing.T) {
	ctx := context.Background()
	s, _, _ := fixture(t)

	hi := task.PriorityHigh
	_, _, err := s.Update(ctx, "T999", task.Changes{Priority: &hi})
	if !errors.Is(err, task.ErrNotFound) {
		t.Fatalf("expected task.ErrNotFound on non-existent task, got %v", err)
	}

	_, err = s.AddNote(ctx, "T999", "some note")
	if !errors.Is(err, task.ErrNotFound) {
		t.Fatalf("expected task.ErrNotFound on non-existent task note, got %v", err)
	}
}

func TestSuggestEdgeConcurrentProposals(t *testing.T) {
	ctx := context.Background()
	now := func() time.Time { return time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC) }
	real := task.NewMemStore(now)
	if _, _, err := real.Create(ctx, task.NewTask{Title: "Generator", Site: "Main", Priority: task.PriorityLow}); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	draftCount := 0
	threadSafeSink := &mockSafeSink{
		onSuggest: func(d task.Draft) {
			mu.Lock()
			draftCount++
			mu.Unlock()
		},
	}

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		workerID := i
		go func() {
			defer wg.Done()
			s := Store{
				Store: real,
				Sink:  threadSafeSink,
				By:    fmt.Sprintf("worker-%d", workerID),
				Key:   fmt.Sprintf("key-%d", workerID),
			}
			hi := task.PriorityHigh
			_, _, err := s.Update(ctx, "T1", task.Changes{Priority: &hi})
			var sug *Suggested
			if !errors.As(err, &sug) {
				t.Errorf("worker %d expected *Suggested, got: %v", workerID, err)
			}
		}()
	}

	wg.Wait()

	if draftCount != goroutines {
		t.Fatalf("expected %d suggestions recorded, got %d", goroutines, draftCount)
	}

	// Real task remains completely untouched at PriorityLow
	tk, err := real.Get(ctx, "T1")
	if err != nil {
		t.Fatal(err)
	}
	if tk.Priority != task.PriorityLow {
		t.Fatalf("real task priority changed: %s", tk.Priority)
	}
}

type mockSafeSink struct {
	onSuggest func(d task.Draft)
}

func (m *mockSafeSink) Suggest(ctx context.Context, d task.Draft) (task.Suggestion, bool, error) {
	if m.onSuggest != nil {
		m.onSuggest(d)
	}
	return task.Suggestion{ID: "node.1", Task: d.Task, Priority: d.Priority, Status: d.Status, Note: d.Note, By: d.By, Key: d.Key}, true, nil
}
func (m *mockSafeSink) Suggestions(context.Context, bool) ([]task.Suggestion, error) { return nil, nil }
func (m *mockSafeSink) Resolve(context.Context, string, bool) (task.Task, error) {
	return task.Task{}, nil
}
