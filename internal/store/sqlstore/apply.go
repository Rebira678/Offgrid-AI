package sqlstore

import (
	"context"
	"encoding/json"
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
const projectionVersion = "2"

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
	done_at    INTEGER NOT NULL DEFAULT 0,
	deleted    INTEGER NOT NULL DEFAULT 0 -- 1 while a delete the task's other ops did not outlive stands
);
CREATE INDEX tasks_dedupe ON tasks (title_key, site_key);
CREATE TABLE task_notes (
	task_id TEXT    NOT NULL,
	n       INTEGER NOT NULL,
	text    TEXT    NOT NULL,
	at      INTEGER NOT NULL,
	node    TEXT    NOT NULL,           -- identity of the op that added the note,
	seq     INTEGER NOT NULL,           -- so a removal can name exactly this note
	removed INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (task_id, n)
) WITHOUT ROWID;
CREATE UNIQUE INDEX task_notes_op ON task_notes (task_id, node, seq);
-- Every op that "adds" a task (create, update, note) is recorded, and every
-- delete with what its author had seen, so whether a task is deleted is a
-- function of the SET of ops, not of the order they were applied in.
CREATE TABLE task_marks (
	task_id TEXT    NOT NULL,
	node    TEXT    NOT NULL,
	seq     INTEGER NOT NULL,
	PRIMARY KEY (task_id, node, seq)
) WITHOUT ROWID;
CREATE TABLE task_removals (
	task_id TEXT    NOT NULL,
	node    TEXT    NOT NULL,
	seq     INTEGER NOT NULL,
	seen    TEXT    NOT NULL,
	PRIMARY KEY (task_id, node, seq)
) WITHOUT ROWID;
`

// resetProjection drops and recreates the materialized tables, empty.
func resetProjection(ctx context.Context, x execer) error {
	if _, err := x.ExecContext(ctx, `DROP TABLE IF EXISTS task_removals; DROP TABLE IF EXISTS task_marks; DROP TABLE IF EXISTS task_notes; DROP TABLE IF EXISTS tasks;`); err != nil {
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
		if err := mark(ctx, x, p.ID, op); err != nil {
			return err
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
		if err := mark(ctx, x, p.ID, op); err != nil {
			return err
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
		if _, err := x.ExecContext(ctx, `INSERT INTO task_notes (task_id, n, text, at, node, seq)
			VALUES (?, (SELECT COALESCE(MAX(n), 0) + 1 FROM task_notes WHERE task_id = ?), ?, ?, ?, ?)`,
			p.ID, p.ID, p.Text, ts, op.NodeID, op.Seq); err != nil {
			return err
		}
		if err := mark(ctx, x, p.ID, op); err != nil {
			return err
		}

	case KindNoteRemoved:
		var p noteRemovedPayload
		if err := decodeStrict(op.Payload, &p); err != nil {
			return bad("bad payload: %v", err)
		}
		if p.ID == "" || !nodeIDRe.MatchString(p.Node) || p.Seq < 1 {
			return bad("a note removal needs a task id and the identity (node, seq) of the note")
		}
		res, err := x.ExecContext(ctx, `UPDATE task_notes SET removed = 1 WHERE task_id = ? AND node = ? AND seq = ?`, p.ID, p.Node, p.Seq)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return bad("removes a note (%s/%d) of task %q that does not exist", p.Node, p.Seq, p.ID)
		}

	case KindTaskRemoved:
		var p removedPayload
		if err := decodeStrict(op.Payload, &p); err != nil {
			return bad("bad payload: %v", err)
		}
		if p.ID == "" {
			return bad("a task removal needs a task id")
		}
		for node, seq := range p.Seen {
			if !nodeIDRe.MatchString(node) || seq < 0 {
				return bad("the removal's seen map has an invalid entry %q: %d", node, seq)
			}
		}
		var exists int
		if err := x.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE id = ?`, p.ID).Scan(&exists); err != nil {
			return err
		}
		if exists != 1 {
			return bad("removes task %q, which does not exist", p.ID)
		}
		seen, _ := json.Marshal(p.Seen) // map keys are sorted: the same text on every replica
		if _, err := x.ExecContext(ctx, `INSERT INTO task_removals (task_id, node, seq, seen) VALUES (?, ?, ?, ?)`,
			p.ID, op.NodeID, op.Seq, string(seen)); err != nil {
			return bad("removal recorded twice: %v", err)
		}
		if err := settleDeleted(ctx, x, p.ID); err != nil {
			return err
		}

	default:
		// Refuse, do not skip. A kind this program does not know may change the
		// meaning of everything after it; guessing would silently diverge.
		return bad("unknown kind %q (written by a newer version?)", op.Kind)
	}
	return nil
}

// mark records that op touched (re-added) a task, then re-decides whether the
// task is deleted. Only tasks that have been deleted at least once pay for the
// second step.
func mark(ctx context.Context, x execer, id string, op Op) error {
	if _, err := x.ExecContext(ctx, `INSERT INTO task_marks (task_id, node, seq) VALUES (?, ?, ?)`, id, op.NodeID, op.Seq); err != nil {
		return err
	}
	var removals int
	if err := x.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_removals WHERE task_id = ?`, id).Scan(&removals); err != nil {
		return err
	}
	if removals == 0 {
		return nil
	}
	return settleDeleted(ctx, x, id)
}

// settleDeleted decides whether a task is deleted. A task is alive if some op
// that touched it (its creation, an edit, a note) was NOT seen by every delete:
// someone changed it without knowing it was being deleted, and that change
// wins. The answer depends only on which ops exist, so every replica that holds
// the same ops decides the same way, whatever order it applied them in.
func settleDeleted(ctx context.Context, x execer, id string) error {
	rows, err := x.QueryContext(ctx, `SELECT seen FROM task_removals WHERE task_id = ?`, id)
	if err != nil {
		return err
	}
	var seens []map[string]int64
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		m := map[string]int64{}
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			rows.Close()
			return err
		}
		seens = append(seens, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	marks, err := x.QueryContext(ctx, `SELECT node, seq FROM task_marks WHERE task_id = ?`, id)
	if err != nil {
		return err
	}
	alive := false
	for marks.Next() && !alive {
		var node string
		var seq int64
		if err := marks.Scan(&node, &seq); err != nil {
			marks.Close()
			return err
		}
		unseenByAll := true
		for _, seen := range seens {
			if seq <= seen[node] {
				unseenByAll = false
				break
			}
		}
		alive = unseenByAll
	}
	marks.Close()
	if err := marks.Err(); err != nil {
		return err
	}
	deleted := 1
	if alive {
		deleted = 0
	}
	_, err = x.ExecContext(ctx, `UPDATE tasks SET deleted = ? WHERE id = ?`, deleted, id)
	return err
}
