package sqlstore

import (
	"context"
	"fmt"
	"strings"

	"offgrid/internal/task"
)

// The materialized view. It is a CACHE of what the log means, shaped for fast
// reads, and it can be deleted at any time and rebuilt from the ops.
//
// projectionVersion identifies the shape of these tables and the meaning of
// applyOp. Change either, bump the number, and every database rebuilds its view
// the next time it is opened. There is never a data migration for derived data.
const projectionVersion = "1"

const projectionDDL = `
CREATE TABLE tasks (
	id         TEXT    PRIMARY KEY,
	n          INTEGER NOT NULL UNIQUE,   -- creation order
	title      TEXT    NOT NULL,
	title_key  TEXT    NOT NULL,          -- title folded for duplicate detection
	site       TEXT    NOT NULL,
	site_key   TEXT    NOT NULL,          -- site lower-cased for matching
	priority   TEXT    NOT NULL,
	status     TEXT    NOT NULL,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	done_at    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX tasks_dedupe ON tasks (title_key, site_key);
CREATE TABLE task_notes (
	task_id TEXT    NOT NULL,
	n       INTEGER NOT NULL,
	text    TEXT    NOT NULL,
	at      INTEGER NOT NULL,
	PRIMARY KEY (task_id, n)
) WITHOUT ROWID;
`

// resetProjection drops and recreates the materialized tables, empty.
func resetProjection(ctx context.Context, x execer) error {
	if _, err := x.ExecContext(ctx, `DROP TABLE IF EXISTS task_notes; DROP TABLE IF EXISTS tasks;`); err != nil {
		return err
	}
	_, err := x.ExecContext(ctx, projectionDDL)
	return err
}

// applyOp applies one op to the materialized tables. It is the ONLY code that
// writes them: live writes call it, Rebuild calls it, Verify calls it. Because
// there is one implementation, "the rebuilt state equals the live state" is a
// property of the design and a test checks it.
func applyOp(ctx context.Context, x execer, op Op) error {
	bad := func(format string, a ...any) error {
		return &CorruptOpError{Pos: op.Pos, Kind: op.Kind, Reason: fmt.Sprintf(format, a...)}
	}
	ts := nanos(op.TS)

	switch op.Kind {
	case KindTaskCreated:
		var p createdPayload
		if err := decodeStrict(op.Payload, &p); err != nil {
			return bad("bad payload: %v", err)
		}
		if p.ID == "" || strings.TrimSpace(p.Title) == "" {
			return bad("a task needs an id and a title")
		}
		if _, err := task.ParsePriority(string(p.Priority)); err != nil {
			return bad("%v", err)
		}
		_, err := x.ExecContext(ctx, `INSERT INTO tasks
			(id, n, title, title_key, site, site_key, priority, status, created_at, updated_at, done_at)
			VALUES (?, (SELECT COALESCE(MAX(n), 0) + 1 FROM tasks), ?, ?, ?, ?, ?, ?, ?, ?, 0)`,
			p.ID, p.Title, task.TitleKey(p.Title), p.Site, strings.ToLower(p.Site),
			string(p.Priority), string(task.StatusOpen), ts, ts)
		if err != nil {
			return bad("cannot create task %q (created twice?): %v", p.ID, err)
		}

	case KindTaskUpdated:
		var p updatedPayload
		if err := decodeStrict(op.Payload, &p); err != nil {
			return bad("bad payload: %v", err)
		}
		if p.Status == nil && p.Priority == nil {
			return bad("an update must change something")
		}
		sets := []string{"updated_at = ?"}
		args := []any{ts}
		if p.Status != nil {
			if _, err := task.ParseStatus(string(*p.Status)); err != nil {
				return bad("%v", err)
			}
			sets = append(sets, "status = ?", "done_at = ?")
			doneAt := int64(0)
			if *p.Status == task.StatusDone {
				doneAt = ts
			}
			args = append(args, string(*p.Status), doneAt)
		}
		if p.Priority != nil {
			if _, err := task.ParsePriority(string(*p.Priority)); err != nil {
				return bad("%v", err)
			}
			sets = append(sets, "priority = ?")
			args = append(args, string(*p.Priority))
		}
		args = append(args, p.ID)
		res, err := x.ExecContext(ctx, `UPDATE tasks SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return bad("refers to task %q, which does not exist", p.ID)
		}

	case KindNoteAdded:
		var p notePayload
		if err := decodeStrict(op.Payload, &p); err != nil {
			return bad("bad payload: %v", err)
		}
		if strings.TrimSpace(p.Text) == "" {
			return bad("a note needs text")
		}
		res, err := x.ExecContext(ctx, `UPDATE tasks SET updated_at = ? WHERE id = ?`, ts, p.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return bad("refers to task %q, which does not exist", p.ID)
		}
		if _, err := x.ExecContext(ctx, `INSERT INTO task_notes (task_id, n, text, at)
			VALUES (?, (SELECT COALESCE(MAX(n), 0) + 1 FROM task_notes WHERE task_id = ?), ?, ?)`,
			p.ID, p.ID, p.Text, ts); err != nil {
			return err
		}

	default:
		// Refuse, do not skip. A kind this program does not know may change the
		// meaning of everything after it; guessing would silently diverge.
		return bad("unknown kind %q (written by a newer version?)", op.Kind)
	}
	return nil
}
