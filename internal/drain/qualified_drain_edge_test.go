package drain

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"offgrid/internal/agent"
	"offgrid/internal/intent"
	"offgrid/internal/jobqueue"
	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
)

// TestInterpretSafelyWithQualifiedTaskIDs verifies that safe AI queue draining
// enforces short ID disambiguation, safely turns task mutations into suggestions
// targeting qualified IDs, and maintains idempotency over job keys.
func TestInterpretSafelyWithQualifiedTaskIDs(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "drain_qual.db")

	st, err := sqlstore.Open(ctx, db, sqlstore.Options{NodeID: "laptop"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	// 1. Seed two tasks with colliding prefix: T1.laptop and T1.phone
	t1, _, err := st.Create(ctx, task.NewTask{Title: "Fix Laptop Screen", Priority: task.PriorityHigh, Site: "A"})
	if err != nil {
		t.Fatalf("Create T1: %v", err)
	}

	remoteOp := []sqlstore.Op{
		{
			NodeID:  "phone",
			Seq:     1,
			TS:      time.Now(),
			Kind:    sqlstore.KindTaskCreated,
			Payload: []byte(`{"id":"T1.phone","title":"Replace Phone Screen","site":"B","priority":"medium"}`),
		},
	}
	if _, err := st.ApplyRemote(ctx, remoteOp); err != nil {
		t.Fatalf("ApplyRemote: %v", err)
	}

	// 2. AI tries to add note to ambiguous "T1"
	mAmbiguous := &fakeModel{
		reply: say(intent.Command{
			Intent:  intent.AddNote,
			TaskRef: "T1",
			Note:    "Screen is cracked",
		}),
	}
	specAmbiguous := InterpretSafely(InterpretSafelyConfig{
		Agent: &agent.Agent{Model: mAmbiguous},
		Store: st,
		Sink:  st,
		Clock: time.Now,
		By:    "qwen2.5:7b",
	})

	job1 := jobqueue.Job{ID: 1, Key: "job-ambig-1", Payload: []byte(`{"text":"add note to T1"}`)}
	msg, err := specAmbiguous.Handle(ctx, job1, &progressLog{})
	if err != nil {
		t.Fatalf("unexpected handler error: %v", err)
	}
	if !strings.Contains(msg, "Not done:") || !strings.Contains(msg, "matches 2 tasks") {
		t.Fatalf("expected 'Not done: ... matches 2 tasks' message, got: %q", msg)
	}

	// Verify no suggestions were created
	sugs, err := st.Suggestions(ctx, true)
	if err != nil {
		t.Fatalf("Suggestions: %v", err)
	}
	if len(sugs) != 0 {
		t.Fatalf("expected 0 suggestions after ambiguous failure, got %d", len(sugs))
	}

	// 3. AI specifies exact qualified ID "T1.laptop"
	mQualified := &fakeModel{
		reply: say(intent.Command{
			Intent:  intent.AddNote,
			TaskRef: t1.ID, // "T1.laptop"
			Note:    "Replace hinge before replacing screen",
		}),
	}
	specQualified := InterpretSafely(InterpretSafelyConfig{
		Agent: &agent.Agent{Model: mQualified},
		Store: st,
		Sink:  st,
		Clock: time.Now,
		By:    "qwen2.5:7b",
	})

	job2 := jobqueue.Job{ID: 2, Key: "job-qual-2", Payload: []byte(`{"text":"add note to T1.laptop"}`)}
	msg, err = specQualified.Handle(ctx, job2, &progressLog{})
	if err != nil {
		t.Fatalf("Handle qualified task note: %v", err)
	}
	if !strings.Contains(msg, "suggested") && !strings.Contains(msg, "proposed") {
		t.Logf("Drain response: %s", msg)
	}

	// The task itself must NOT have the note applied directly
	tk, err := st.Get(ctx, t1.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(tk.Notes) != 0 {
		t.Fatalf("safe drain should not write notes directly to tasks; found %d notes", len(tk.Notes))
	}

	// Exactly 1 suggestion must be waiting for T1.laptop
	sugs, err = st.Suggestions(ctx, false)
	if err != nil || len(sugs) != 1 {
		t.Fatalf("expected 1 pending suggestion, got %d (err: %v)", len(sugs), err)
	}
	if sugs[0].Task != "T1.laptop" {
		t.Fatalf("expected suggestion for T1.laptop, got: %s", sugs[0].Task)
	}

	// 4. Idempotency test: replay job2 with same key
	_, err = specQualified.Handle(ctx, job2, &progressLog{})
	if err != nil {
		t.Fatalf("replaying job2 failed: %v", err)
	}
	sugs, _ = st.Suggestions(ctx, false)
	if len(sugs) != 1 {
		t.Fatalf("idempotency violated: expected 1 suggestion after replay, got %d", len(sugs))
	}
}
