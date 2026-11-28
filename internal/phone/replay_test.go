package phone

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
)

func newTestStore(t *testing.T) *sqlstore.Store {
	t.Helper()
	st, err := sqlstore.Open(context.Background(), ":memory:", sqlstore.Options{})
	if err != nil {
		t.Fatalf("open memory sqlstore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestReplayQueuedOpsOfflineStampingAndLateMark(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	nodeClock := time.Date(2026, 11, 28, 6, 0, 0, 0, time.UTC)
	now := func() time.Time { return nodeClock }

	sess := &Session{
		Token: "test-token-123",
		Label: "Field Android 4G",
	}

	// 1. Batch with offline create, offline status update, offline note
	clientTime := time.Date(2026, 11, 28, 5, 30, 0, 0, time.UTC)
	createPayload, _ := json.Marshal(CreateTaskPayload{
		Title:    "Hydraulic valve maintenance",
		Priority: "high",
		Note:     "Observed minor hydraulic fluid weep",
	})

	batch := ReplayBatch{
		ClientID: "Field Android 4G",
		Operations: []QueuedOp{
			{
				OpID:       "op-create-1",
				Action:     "create_task",
				ClientTime: clientTime,
				Payload:    createPayload,
			},
		},
	}

	res := ReplayQueuedOps(ctx, st, sess, batch, now)
	if res.Applied != 1 || res.Conflicts != 0 || res.Errors != 0 {
		t.Fatalf("unexpected replay summary: %+v", res)
	}

	taskID := res.Results[0].TaskID
	createdTask, err := st.Get(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to fetch replayed task: %v", err)
	}

	if createdTask.Title != "Hydraulic valve maintenance" {
		t.Errorf("task title mismatch: got %q", createdTask.Title)
	}
	if createdTask.Priority != task.PriorityHigh {
		t.Errorf("task priority mismatch: got %q", createdTask.Priority)
	}

	// Verify late sync note was added
	foundLateNote := false
	for _, n := range createdTask.Notes {
		if strings.Contains(n.Text, "[late sync from phone Field Android 4G") {
			foundLateNote = true
			break
		}
	}
	if !foundLateNote {
		t.Errorf("expected late sync note on replayed task, notes: %+v", createdTask.Notes)
	}

	// 2. Offline update status and add note
	updatePayload, _ := json.Marshal(UpdateStatusPayload{Status: "in_progress"})
	notePayload, _ := json.Marshal(AddNotePayload{Text: "Torqued flange bolts to 45 Nm"})

	batch2 := ReplayBatch{
		ClientID: "Field Android 4G",
		Operations: []QueuedOp{
			{
				OpID:       "op-upd-1",
				Action:     "update_status",
				TaskID:     taskID,
				ClientTime: clientTime.Add(10 * time.Minute),
				Payload:    updatePayload,
			},
			{
				OpID:       "op-note-1",
				Action:     "add_note",
				TaskID:     taskID,
				ClientTime: clientTime.Add(15 * time.Minute),
				Payload:    notePayload,
			},
		},
	}

	res2 := ReplayQueuedOps(ctx, st, sess, batch2, now)
	if res2.Applied != 2 || res2.Conflicts != 0 || res2.Errors != 0 {
		t.Fatalf("batch 2 failed: %+v", res2)
	}

	updatedTask, err := st.Get(ctx, taskID)
	if err != nil {
		t.Fatalf("fetch updated task: %v", err)
	}
	if updatedTask.Status != task.StatusInProgress {
		t.Errorf("status mismatch: got %q, want in_progress", updatedTask.Status)
	}

	// Check notes for late sync markers
	hasStatusLateNote := false
	hasTextLateNote := false
	for _, n := range updatedTask.Notes {
		if strings.Contains(n.Text, "[late sync from phone Field Android 4G]: status changed to in_progress") {
			hasStatusLateNote = true
		}
		if strings.Contains(n.Text, "Torqued flange bolts to 45 Nm [late sync from phone Field Android 4G]") {
			hasTextLateNote = true
		}
	}
	if !hasStatusLateNote {
		t.Errorf("missing status late note in notes: %+v", updatedTask.Notes)
	}
	if !hasTextLateNote {
		t.Errorf("missing text late note in notes: %+v", updatedTask.Notes)
	}
}

func TestReplayQueuedOpsConflictHandling(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Now

	sess := &Session{Token: "sess-tok", Label: "Tech Bob"}

	// Create a task on the node
	t1, _, err := st.Create(ctx, task.NewTask{Title: "Check pump", Priority: task.PriorityLow})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	// Delete task on the node (simulating concurrent deletion while phone was offline)
	if err := st.DeleteTask(ctx, t1.ID); err != nil {
		t.Fatalf("delete task: %v", err)
	}

	// Phone tries to replay status update and note on the deleted task
	updPayload, _ := json.Marshal(UpdateStatusPayload{Status: "done"})
	notePayload, _ := json.Marshal(AddNotePayload{Text: "Finished inspection"})

	batch := ReplayBatch{
		ClientID: "Tech Bob",
		Operations: []QueuedOp{
			{
				OpID:       "op-conflict-1",
				Action:     "update_status",
				TaskID:     t1.ID,
				ClientTime: now().Add(-20 * time.Minute),
				Payload:    updPayload,
			},
			{
				OpID:       "op-conflict-2",
				Action:     "add_note",
				TaskID:     t1.ID,
				ClientTime: now().Add(-15 * time.Minute),
				Payload:    notePayload,
			},
			{
				OpID:       "op-conflict-3",
				Action:     "delete_task",
				TaskID:     t1.ID,
				ClientTime: now().Add(-10 * time.Minute),
			},
		},
	}

	res := ReplayQueuedOps(ctx, st, sess, batch, now)
	if res.Conflicts != 3 {
		t.Errorf("expected 3 conflicts, got %d (Applied: %d, Errors: %d)", res.Conflicts, res.Applied, res.Errors)
	}
	for _, r := range res.Results {
		if r.Status != "conflict_task_deleted" {
			t.Errorf("expected conflict_task_deleted, got %q (message: %s)", r.Status, r.Message)
		}
	}
}

func TestReplaySuggestionResolution(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Now

	sess := &Session{Token: "sess-sug", Label: "Phone User"}

	// Create task
	t1, _, err := st.Create(ctx, task.NewTask{Title: "Replace radiator", Priority: task.PriorityLow})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	// Create AI suggestion
	highPri := task.PriorityHigh
	sug, _, err := st.Suggest(ctx, task.Draft{
		Task:     t1.ID,
		Priority: &highPri,
		Note:     "Radiator temperature exceeded critical threshold",
		By:       "thermal-model-v1",
	})
	if err != nil {
		t.Fatalf("suggest: %v", err)
	}

	// Replay suggestion resolution
	acceptPayload, _ := json.Marshal(ResolveSuggestionPayload{Accept: true})
	batch := ReplayBatch{
		ClientID: "Phone User",
		Operations: []QueuedOp{
			{
				OpID:         "op-sug-1",
				Action:       "resolve_suggestion",
				SuggestionID: sug.ID,
				ClientTime:   now().Add(-5 * time.Minute),
				Payload:      acceptPayload,
			},
		},
	}

	res := ReplayQueuedOps(ctx, st, sess, batch, now)
	if res.Applied != 1 || res.Conflicts != 0 {
		t.Fatalf("expected 1 applied resolution, got: %+v", res)
	}

	// Task should now be high priority
	updated, err := st.Get(ctx, t1.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if updated.Priority != task.PriorityHigh {
		t.Errorf("expected task priority high after suggestion acceptance, got %v", updated.Priority)
	}

	// Replaying again should detect conflict_stale
	res2 := ReplayQueuedOps(ctx, st, sess, batch, now)
	if res2.Conflicts != 1 {
		t.Errorf("expected 1 conflict on replaying already resolved suggestion, got %d", res2.Conflicts)
	}
	if res2.Results[0].Status != "conflict_stale" {
		t.Errorf("expected conflict_stale, got %q", res2.Results[0].Status)
	}
}
