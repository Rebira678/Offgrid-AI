package phone

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"offgrid/internal/task"
)

// ReplayQueuedOps processes an offline batch sent by a reconnecting phone.
// Every op is stamped on arrival and annotated with late sync attribution.
func ReplayQueuedOps(ctx context.Context, st Store, sess *Session, batch ReplayBatch, now func() time.Time) ReplayResponse {
	if now == nil {
		now = time.Now
	}
	resp := ReplayResponse{
		Results: make([]OpResult, 0, len(batch.Operations)),
	}

	label := "phone"
	if sess != nil && sess.Label != "" {
		label = sess.Label
	}

	for _, op := range batch.Operations {
		stamp := now().UTC()
		res := OpResult{
			OpID:      op.OpID,
			Action:    op.Action,
			TaskID:    op.TaskID,
			StampedAt: stamp,
		}

		switch op.Action {
		case "create_task":
			var payload CreateTaskPayload
			if len(op.Payload) > 0 {
				_ = json.Unmarshal(op.Payload, &payload)
			}
			if strings.TrimSpace(payload.Title) == "" {
				res.Status = "error"
				res.Message = "title cannot be empty"
				resp.Errors++
				resp.Results = append(resp.Results, res)
				continue
			}

			pri := task.Priority(payload.Priority)
			if pri == "" {
				pri = task.PriorityMedium
			}

			// Format late arrival note
			timeStr := op.ClientTime.UTC().Format("15:04:05")
			lateNote := fmt.Sprintf("[late sync from phone %s offline at %s]", label, timeStr)
			if payload.Note != "" {
				lateNote = fmt.Sprintf("%s\n%s", lateNote, payload.Note)
			}

			created, _, err := st.Create(ctx, task.NewTask{
				Title:    payload.Title,
				Priority: pri,
			})
			if err != nil {
				res.Status = "error"
				res.Message = err.Error()
				resp.Errors++
			} else {
				if lateNote != "" {
					_, _ = st.AddNote(ctx, created.ID, lateNote)
				}
				res.Status = "applied"
				res.TaskID = created.ID
				res.Message = fmt.Sprintf("created task %s (marked late sync)", task.ShortID(created.ID))
				resp.Applied++
			}

		case "update_status":
			var payload UpdateStatusPayload
			if len(op.Payload) > 0 {
				_ = json.Unmarshal(op.Payload, &payload)
			}
			stVal := task.Status(payload.Status)
			if stVal == "" {
				res.Status = "error"
				res.Message = "status is required"
				resp.Errors++
				resp.Results = append(resp.Results, res)
				continue
			}

			// Check conflict: task might have been deleted on node
			_, err := st.Get(ctx, op.TaskID)
			if errors.Is(err, task.ErrNotFound) {
				res.Status = "conflict_task_deleted"
				res.Message = fmt.Sprintf("task %s was deleted on node while phone was offline", op.TaskID)
				resp.Conflicts++
				resp.Results = append(resp.Results, res)
				continue
			}

			// Apply status update
			_, _, err = st.Update(ctx, op.TaskID, task.Changes{
				Status: &stVal,
			})
			if err != nil {
				res.Status = "error"
				res.Message = err.Error()
				resp.Errors++
			} else {
				// Record late sync status change note
				_, _ = st.AddNote(ctx, op.TaskID, fmt.Sprintf("[late sync from phone %s]: status changed to %s", label, payload.Status))
				res.Status = "applied"
				res.Message = fmt.Sprintf("updated %s status to %s (marked late)", task.ShortID(op.TaskID), payload.Status)
				resp.Applied++
			}

		case "add_note":
			var payload AddNotePayload
			if len(op.Payload) > 0 {
				_ = json.Unmarshal(op.Payload, &payload)
			}
			if strings.TrimSpace(payload.Text) == "" {
				res.Status = "error"
				res.Message = "note text cannot be empty"
				resp.Errors++
				resp.Results = append(resp.Results, res)
				continue
			}

			// Check conflict: task might have been deleted on node
			_, err := st.Get(ctx, op.TaskID)
			if errors.Is(err, task.ErrNotFound) {
				res.Status = "conflict_task_deleted"
				res.Message = fmt.Sprintf("task %s was deleted on node while phone was offline", op.TaskID)
				resp.Conflicts++
				resp.Results = append(resp.Results, res)
				continue
			}

			annotated := fmt.Sprintf("%s [late sync from phone %s]", payload.Text, label)
			_, err = st.AddNote(ctx, op.TaskID, annotated)
			if err != nil {
				res.Status = "error"
				res.Message = err.Error()
				resp.Errors++
			} else {
				res.Status = "applied"
				res.Message = fmt.Sprintf("added note to %s (marked late)", task.ShortID(op.TaskID))
				resp.Applied++
			}

		case "delete_task":
			if op.TaskID == "" {
				res.Status = "error"
				res.Message = "task_id is required"
				resp.Errors++
				resp.Results = append(resp.Results, res)
				continue
			}

			err := st.DeleteTask(ctx, op.TaskID)
			if errors.Is(err, task.ErrNotFound) {
				res.Status = "conflict_task_deleted"
				res.Message = fmt.Sprintf("task %s was already deleted on node", op.TaskID)
				resp.Conflicts++
			} else if err != nil {
				res.Status = "error"
				res.Message = err.Error()
				resp.Errors++
			} else {
				res.Status = "applied"
				res.Message = fmt.Sprintf("deleted task %s", task.ShortID(op.TaskID))
				resp.Applied++
			}

		case "resolve_suggestion":
			sugID := op.SuggestionID
			if sugID == "" {
				sugID = op.TaskID
			}
			var payload ResolveSuggestionPayload
			if len(op.Payload) > 0 {
				_ = json.Unmarshal(op.Payload, &payload)
			}

			_, err := st.Resolve(ctx, sugID, payload.Accept)
			if errors.Is(err, task.ErrStale) || errors.Is(err, task.ErrSuggestionResolved) {
				res.Status = "conflict_stale"
				res.Message = fmt.Sprintf("suggestion %s was stale or already resolved on node: %v", sugID, err)
				resp.Conflicts++
			} else if errors.Is(err, task.ErrSuggestionNotFound) || errors.Is(err, task.ErrNotFound) {
				res.Status = "conflict_stale"
				res.Message = fmt.Sprintf("suggestion %s was not found: %v", sugID, err)
				resp.Conflicts++
			} else if err != nil {
				res.Status = "error"
				res.Message = err.Error()
				resp.Errors++
			} else {
				res.Status = "applied"
				actionDesc := "dismissed"
				if payload.Accept {
					actionDesc = "accepted"
				}
				res.Message = fmt.Sprintf("%s suggestion %s", actionDesc, sugID)
				resp.Applied++
			}

		default:
			res.Status = "error"
			res.Message = fmt.Sprintf("unknown action %q", op.Action)
			resp.Errors++
		}

		resp.Results = append(resp.Results, res)
	}

	return resp
}
