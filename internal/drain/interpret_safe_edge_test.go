package drain

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"offgrid/internal/agent"
	"offgrid/internal/intent"
	"offgrid/internal/jobqueue"
	"offgrid/internal/store/sqlstore"
)

func TestInterpretSafelyEdgeNilStoreOrSinkGuards(t *testing.T) {
	ctx := context.Background()
	m := &fakeModel{reply: say(intent.Command{Intent: intent.ListTasks})}
	ag := &agent.Agent{Model: m}
	st, err := sqlstore.Open(ctx, filepath.Join(t.TempDir(), "t.db"), sqlstore.Options{NodeID: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Case 1: Nil Store
	specNilStore := InterpretSafely(InterpretSafelyConfig{
		Agent: ag,
		Store: nil,
		Sink:  st,
	})
	job := jobqueue.Job{ID: 1, Payload: []byte(`{"text":"list"}`)}
	_, err = specNilStore.Handle(ctx, job, &progressLog{})
	if err == nil || err.Error() != "drain: store and sink are required" {
		t.Fatalf("expected nil store guard error, got: %v", err)
	}

	// Case 2: Nil Sink
	specNilSink := InterpretSafely(InterpretSafelyConfig{
		Agent: ag,
		Store: st,
		Sink:  nil,
	})
	_, err = specNilSink.Handle(ctx, job, &progressLog{})
	if err == nil || err.Error() != "drain: store and sink are required" {
		t.Fatalf("expected nil sink guard error, got: %v", err)
	}
}

func TestInterpretSafelyEdgeContextCancellation(t *testing.T) {
	ctx := context.Background()
	m := &fakeModel{reply: say(intent.Command{Intent: intent.ListTasks})}
	ag := &agent.Agent{Model: m}
	st, err := sqlstore.Open(ctx, filepath.Join(t.TempDir(), "t.db"), sqlstore.Options{NodeID: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	spec := InterpretSafely(InterpretSafelyConfig{
		Agent: ag,
		Store: st,
		Sink:  st,
	})

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel() // canceled upfront

	job := jobqueue.Job{ID: 1, Payload: []byte(`{"text":"list"}`)}
	_, err = spec.Handle(canceledCtx, job, &progressLog{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
}

func TestInterpretSafelyEdgeTaskNotFoundReturnsNotDone(t *testing.T) {
	r := newSafeRig(t, say(intent.Command{
		Intent:   intent.UpdateTask,
		TaskRef:  "T999",
		Priority: "high",
	}))

	out, err := r.run("job-not-found", "make T999 high priority", &progressLog{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "Not done: task not found" {
		t.Fatalf("unexpected output message: %q", out)
	}
	sugs, err := r.st.Suggestions(context.Background(), true)
	if err != nil || len(sugs) != 0 {
		t.Fatalf("no suggestion should be drafted for non-existent task, got %+v", sugs)
	}
}

func TestInterpretSafelyEdgeExecutingMarkerOnRetry(t *testing.T) {
	r := newSafeRig(t, say(intent.Command{Intent: intent.CreateTask, Title: "Check valve"}))

	job := jobqueue.Job{
		ID:       42,
		Payload:  []byte(fmt.Sprintf(`{"text":%q}`, "create valve task")),
		Progress: markExecuting, // was previously interrupted mid-create
	}

	_, err := r.spec.Handle(context.Background(), job, &progressLog{})
	if !errors.Is(err, ErrPermanent) {
		t.Fatalf("expected ErrPermanent on retrying executing task create, got: %v", err)
	}
}
