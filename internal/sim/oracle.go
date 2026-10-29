package sim

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
)

// The oracles recompute two decisions from the raw op log, without any of the
// store's code, and compare them with what the store's view says. Convergence
// alone would pass a store that converges on the wrong answer (every replica
// agrees that a delete beat an edit it had not seen); these catch that.

type opPayload struct {
	ID         string           `json:"id"`
	Seen       map[string]int64 `json:"seen"`
	Task       string           `json:"task"`
	Node       string           `json:"node"`
	Seq        int64            `json:"seq"`
	Resolution string           `json:"resolution"`
}

type opRef struct {
	node string
	seq  int64
}

// checkAgainstTheLog compares the converged store with a recomputation from its
// own log. ops must be in fold order.
func checkAgainstTheLog(ctx context.Context, s *sqlstore.Store, ops []sqlstore.Op) *Failure {
	created := map[string]bool{}
	marks := map[string][]opRef{}
	removals := map[string][]map[string]int64{}
	madeOn := map[string]string{}    // suggestion "node.seq" -> task
	resolved := map[string]string{}  // suggestion -> resolution of the FIRST resolve in fold order
	resolvedBy := map[string]opRef{} // suggestion -> that resolve op
	for _, op := range ops {
		var p opPayload
		if err := json.Unmarshal(op.Payload, &p); err != nil {
			return &Failure{"oracle", fmt.Sprintf("%s/%d: %v", op.NodeID, op.Seq, err)}
		}
		switch op.Kind {
		case sqlstore.KindTaskCreated:
			created[p.ID] = true
			marks[p.ID] = append(marks[p.ID], opRef{op.NodeID, op.Seq})
		case sqlstore.KindTaskUpdated, sqlstore.KindNoteAdded:
			marks[p.ID] = append(marks[p.ID], opRef{op.NodeID, op.Seq})
		case sqlstore.KindTaskRemoved:
			removals[p.ID] = append(removals[p.ID], p.Seen)
		case sqlstore.KindSuggestionMade:
			madeOn[fmt.Sprintf("%s.%d", op.NodeID, op.Seq)] = p.Task
		case sqlstore.KindSuggestionResolved:
			key := fmt.Sprintf("%s.%d", p.Node, p.Seq)
			if _, done := resolved[key]; !done {
				if _, known := madeOn[key]; known {
					resolved[key] = p.Resolution
					resolvedBy[key] = opRef{op.NodeID, op.Seq}
				}
			}
		}
	}
	// An accepted suggestion is a person editing the task.
	for key, res := range resolved {
		if res == "accepted" {
			t := madeOn[key]
			marks[t] = append(marks[t], resolvedBy[key])
		}
	}

	// A task is deleted when it has been deleted at least once and no op that
	// touched it went unseen by every delete.
	live, err := s.List(ctx, task.Filter{})
	if err != nil {
		return &Failure{"oracle", err.Error()}
	}
	have := map[string]bool{}
	for _, t := range live {
		have[t.ID] = true
	}
	var ids []string
	for id := range created {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		wantAlive := true
		if len(removals[id]) > 0 {
			wantAlive = false
			for _, m := range marks[id] {
				unseenByAll := true
				for _, seen := range removals[id] {
					if m.seq <= seen[m.node] {
						unseenByAll = false
						break
					}
				}
				if unseenByAll {
					wantAlive = true
					break
				}
			}
		}
		if have[id] != wantAlive {
			return &Failure{"delete-rule", fmt.Sprintf("task %s: the log says it should be %s (%d deletes, %d edits), the view has it %s",
				id, word(wantAlive), len(removals[id]), len(marks[id]), word(have[id]))}
		}
	}

	// A suggestion's outcome is its first resolution in fold order.
	sugs, err := s.Suggestions(ctx, true)
	if err != nil {
		return &Failure{"oracle", err.Error()}
	}
	for _, sg := range sugs {
		want := string(task.SuggestionPending)
		if r, ok := resolved[sg.ID]; ok {
			want = r
		}
		if string(sg.State) != want {
			return &Failure{"suggestion-rule", fmt.Sprintf("suggestion %s: the first resolution in the log says %q, the view has %q", sg.ID, want, sg.State)}
		}
	}
	if len(sugs) != len(madeOn) {
		return &Failure{"suggestion-rule", fmt.Sprintf("the log holds %d suggestions, the view %d", len(madeOn), len(sugs))}
	}
	return nil
}

func word(alive bool) string {
	if alive {
		return "alive"
	}
	return "deleted"
}
