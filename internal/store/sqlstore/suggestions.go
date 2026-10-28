package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"offgrid/internal/task"
)

var _ task.Suggester = (*Store)(nil)

func suggestionID(node string, seq int64) string { return node + "." + strconv.FormatInt(seq, 10) }

// splitSuggestionID undoes suggestionID. Node IDs may contain dots, so the
// sequence number is whatever follows the LAST dot.
func splitSuggestionID(id string) (node string, seq int64, err error) {
	i := strings.LastIndex(id, ".")
	if i <= 0 || i == len(id)-1 {
		return "", 0, fmt.Errorf("%w: %q is not a suggestion ID (they look like laptop.12)", task.ErrSuggestionNotFound, id)
	}
	seq, err = strconv.ParseInt(id[i+1:], 10, 64)
	if err != nil || seq < 1 || !nodeIDRe.MatchString(id[:i]) {
		return "", 0, fmt.Errorf("%w: %q is not a suggestion ID (they look like laptop.12)", task.ErrSuggestionNotFound, id)
	}
	return id[:i], seq, nil
}

const suggestionColumns = `node, seq, task_id, status, priority, note, base_status, base_priority, by, key, at, state`

// scanSuggestion reads one row. It does not query: the store has ONE database
// connection, so asking a second question while rows are still open would wait
// forever. Staleness is worked out afterwards by withStale.
func scanSuggestion(row interface{ Scan(...any) error }) (task.Suggestion, error) {
	var s task.Suggestion
	var node string
	var seq, at int64
	var st, pr sql.NullString
	var bs, bp, state string
	if err := row.Scan(&node, &seq, &s.Task, &st, &pr, &s.Note, &bs, &bp, &s.By, &s.Key, &at, &state); err != nil {
		return s, err
	}
	s.ID = suggestionID(node, seq)
	if st.Valid {
		v := task.Status(st.String)
		s.Status = &v
	}
	if pr.Valid {
		v := task.Priority(pr.String)
		s.Priority = &v
	}
	s.BaseStatus, s.BasePriority = task.Status(bs), task.Priority(bp)
	s.At = fromNanos(at)
	s.State = task.SuggestionState(state)
	return s, nil
}

// withStale marks a pending suggestion stale if a person changed, since it was
// made, a field it would change.
func withStale(ctx context.Context, x execer, s task.Suggestion) (task.Suggestion, error) {
	if s.State != task.SuggestionPending {
		return s, nil
	}
	var cs, cp string
	err := x.QueryRowContext(ctx, `SELECT status, priority FROM tasks WHERE id = ?`, s.Task).Scan(&cs, &cp)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	s.Stale = (s.Status != nil && task.Status(cs) != s.BaseStatus) || (s.Priority != nil && task.Priority(cp) != s.BasePriority)
	return s, nil
}

// oneSuggestion reads a single suggestion, with staleness.
func oneSuggestion(ctx context.Context, x execer, where string, args ...any) (task.Suggestion, error) {
	sg, err := scanSuggestion(x.QueryRowContext(ctx, `SELECT `+suggestionColumns+` FROM suggestions WHERE `+where, args...))
	if err != nil {
		return sg, err
	}
	return withStale(ctx, x, sg)
}

func allSuggestions(ctx context.Context, x execer, where string) ([]task.Suggestion, error) {
	rows, err := x.QueryContext(ctx, `SELECT `+suggestionColumns+` FROM suggestions `+where+` ORDER BY at, node, seq`)
	if err != nil {
		return nil, err
	}
	var out []task.Suggestion
	for rows.Next() {
		sg, err := scanSuggestion(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, sg)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i], err = withStale(ctx, x, out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Suggest records an AI proposal. It never changes the task: the task row, its
// notes and its updated time are untouched.
func (s *Store) Suggest(ctx context.Context, d task.Draft) (sug task.Suggestion, created bool, err error) {
	if err := ctx.Err(); err != nil {
		return sug, false, err
	}
	err = s.write(ctx, func(tx *sql.Tx) error {
		if d.Key != "" {
			if got, err := oneSuggestion(ctx, tx, `key = ? ORDER BY at, node, seq LIMIT 1`, d.Key); err == nil {
				sug = got
				return nil
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		cur, err := getTask(ctx, tx, d.Task)
		if err != nil {
			return err
		}
		note := strings.TrimSpace(d.Note)
		if d.Status == nil && d.Priority == nil && note == "" {
			return task.ErrNothingToSuggest
		}
		if err := s.record(ctx, tx, KindSuggestionMade, suggestionPayload{
			Task: d.Task, Status: d.Status, Priority: d.Priority, Note: note,
			BaseStatus: cur.Status, BasePriority: cur.Priority, By: d.By, Key: d.Key,
		}); err != nil {
			return err
		}
		var seq int64
		if err := tx.QueryRowContext(ctx, `SELECT MAX(seq) FROM ops WHERE node_id = ?`, s.node).Scan(&seq); err != nil {
			return err
		}
		sug, err = oneSuggestion(ctx, tx, `node = ? AND seq = ?`, s.node, seq)
		created = true
		return err
	})
	return sug, created, err
}

// Suggestions lists proposals, oldest first.
func (s *Store) Suggestions(ctx context.Context, all bool) ([]task.Suggestion, error) {
	if all {
		return allSuggestions(ctx, s.db, "")
	}
	return allSuggestions(ctx, s.db, "WHERE state = 'pending'")
}

// Resolve accepts or dismisses a pending suggestion. Accepting a stale one is
// refused with task.ErrStale and nothing changes.
func (s *Store) Resolve(ctx context.Context, id string, accept bool) (task.Task, error) {
	var out task.Task
	node, seq, err := splitSuggestionID(id)
	if err != nil {
		return out, err
	}
	err = s.write(ctx, func(tx *sql.Tx) error {
		sg, err := oneSuggestion(ctx, tx, `node = ? AND seq = ?`, node, seq)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", task.ErrSuggestionNotFound, id)
		}
		if err != nil {
			return err
		}
		if sg.State != task.SuggestionPending {
			return fmt.Errorf("%w (%s)", task.ErrSuggestionResolved, sg.State)
		}
		if accept && sg.Stale {
			return fmt.Errorf("%w: it was made when %s was %s/%s", task.ErrStale, sg.Task, sg.BaseStatus, sg.BasePriority)
		}
		if _, err := getTask(ctx, tx, sg.Task); err != nil {
			return err
		}
		res := "dismissed"
		if accept {
			res = "accepted"
		}
		if err := s.record(ctx, tx, KindSuggestionResolved, resolvedPayload{Node: node, Seq: seq, Resolution: res}); err != nil {
			return err
		}
		out, err = getTask(ctx, tx, sg.Task)
		return err
	})
	return out, err
}

type canonSuggestion struct {
	ID       string `json:"id"`
	Task     string `json:"task"`
	Status   string `json:"status,omitempty"`
	Priority string `json:"priority,omitempty"`
	Note     string `json:"note,omitempty"`
	BaseS    string `json:"base_status"`
	BaseP    string `json:"base_priority"`
	By       string `json:"by"`
	Key      string `json:"key,omitempty"`
	At       int64  `json:"at"`
	State    string `json:"state"`
}

func canonSuggestions(ctx context.Context, x execer) ([]canonSuggestion, error) {
	list, err := allSuggestions(ctx, x, "")
	if err != nil {
		return nil, err
	}
	var out []canonSuggestion
	for _, sg := range list {
		c := canonSuggestion{ID: sg.ID, Task: sg.Task, Note: sg.Note, BaseS: string(sg.BaseStatus), BaseP: string(sg.BasePriority),
			By: sg.By, Key: sg.Key, At: nanos(sg.At), State: string(sg.State)}
		if sg.Status != nil {
			c.Status = string(*sg.Status)
		}
		if sg.Priority != nil {
			c.Priority = string(*sg.Priority)
		}
		out = append(out, c)
	}
	return out, nil
}
