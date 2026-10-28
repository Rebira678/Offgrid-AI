package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
const projectionVersion = "4"

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
-- Tier 2 answers. The first one recorded for a key, in canonical fold order,
-- is the answer on every replica.
CREATE TABLE answers (
	key      TEXT    PRIMARY KEY,
	question TEXT    NOT NULL,
	text     TEXT    NOT NULL,
	model    TEXT    NOT NULL,
	source   TEXT    NOT NULL,
	at       INTEGER NOT NULL,
	node     TEXT    NOT NULL,
	seq      INTEGER NOT NULL
) WITHOUT ROWID;
-- AI proposals. They sit BESIDE the tasks: nothing here changes a task until a
-- person's accept op is folded in.
CREATE TABLE suggestions (
	node          TEXT    NOT NULL,   -- identity of the op that made it
	seq           INTEGER NOT NULL,
	task_id       TEXT    NOT NULL,
	status        TEXT,               -- proposed values; NULL = not proposed
	priority      TEXT,
	note          TEXT    NOT NULL DEFAULT '',
	base_status   TEXT    NOT NULL,   -- what the task showed at the time
	base_priority TEXT    NOT NULL,
	by            TEXT    NOT NULL,
	key           TEXT    NOT NULL DEFAULT '',
	at            INTEGER NOT NULL,
	state         TEXT    NOT NULL DEFAULT 'pending',
	res_node      TEXT    NOT NULL DEFAULT '',   -- the op that resolved it
	res_seq       INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (node, seq)
) WITHOUT ROWID;
CREATE INDEX suggestions_key ON suggestions (key);
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
	if _, err := x.ExecContext(ctx, `DROP TABLE IF EXISTS suggestions; DROP TABLE IF EXISTS answers; DROP TABLE IF EXISTS task_removals; DROP TABLE IF EXISTS task_marks; DROP TABLE IF EXISTS task_notes; DROP TABLE IF EXISTS tasks;`); err != nil {
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
		if p.Status != nil {
			if _, err := task.ParseStatus(string(*p.Status)); err != nil {
				return bad("%v", err)
			}
		}
		if p.Priority != nil {
			if _, err := task.ParsePriority(string(*p.Priority)); err != nil {
				return bad("%v", err)
			}
		}
		if n, err := setFields(ctx, x, p.ID, p.Status, p.Priority, ts); err != nil {
			return err
		} else if n != 1 {
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
		if n, err := insertNote(ctx, x, p.ID, p.Text, ts, op); err != nil {
			return err
		} else if n != 1 {
			return bad("refers to task %q, which does not exist", p.ID)
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

	case KindAnswerRecorded:
		var p answerPayload
		if err := decodeStrict(op.Payload, &p); err != nil {
			return bad("bad payload: %v", err)
		}
		if strings.TrimSpace(p.Key) == "" || strings.TrimSpace(p.Text) == "" {
			return bad("an answer needs a key and text")
		}
		if p.Source != "local" && p.Source != "cloud" {
			return bad("an answer's source must be local or cloud, not %q", p.Source)
		}
		// OR IGNORE: a second answer for the same key keeps the first. Ops are
		// folded in one canonical order, so "first" is the same on every replica.
		if _, err := x.ExecContext(ctx, `INSERT OR IGNORE INTO answers (key, question, text, model, source, at, node, seq)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, p.Key, p.Question, p.Text, p.Model, p.Source, ts, op.NodeID, op.Seq); err != nil {
			return err
		}

	case KindSuggestionMade:
		var p suggestionPayload
		if err := decodeStrict(op.Payload, &p); err != nil {
			return bad("bad payload: %v", err)
		}
		if p.Task == "" || (p.Status == nil && p.Priority == nil && strings.TrimSpace(p.Note) == "") {
			return bad("a suggestion needs a task and something to change")
		}
		if p.Status != nil {
			if _, err := task.ParseStatus(string(*p.Status)); err != nil {
				return bad("%v", err)
			}
		}
		if p.Priority != nil {
			if _, err := task.ParsePriority(string(*p.Priority)); err != nil {
				return bad("%v", err)
			}
		}
		if _, err := task.ParseStatus(string(p.BaseStatus)); err != nil {
			return bad("base: %v", err)
		}
		if _, err := task.ParsePriority(string(p.BasePriority)); err != nil {
			return bad("base: %v", err)
		}
		var exists int
		if err := x.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE id = ?`, p.Task).Scan(&exists); err != nil {
			return err
		}
		if exists != 1 {
			return bad("suggests a change to task %q, which does not exist", p.Task)
		}
		var st, pr any
		if p.Status != nil {
			st = string(*p.Status)
		}
		if p.Priority != nil {
			pr = string(*p.Priority)
		}
		// Deliberately NOT touching the tasks table: a suggestion is not an edit.
		if _, err := x.ExecContext(ctx, `INSERT INTO suggestions
			(node, seq, task_id, status, priority, note, base_status, base_priority, by, key, at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			op.NodeID, op.Seq, p.Task, st, pr, p.Note, string(p.BaseStatus), string(p.BasePriority), p.By, p.Key, ts); err != nil {
			return bad("suggestion recorded twice: %v", err)
		}

	case KindSuggestionResolved:
		var p resolvedPayload
		if err := decodeStrict(op.Payload, &p); err != nil {
			return bad("bad payload: %v", err)
		}
		if !nodeIDRe.MatchString(p.Node) || p.Seq < 1 || (p.Resolution != "accepted" && p.Resolution != "dismissed") {
			return bad("a resolution names a suggestion (node, seq) and is accepted or dismissed")
		}
		var taskID, state, note string
		var st, pr sql.NullString
		err := x.QueryRowContext(ctx, `SELECT task_id, state, status, priority, note FROM suggestions WHERE node = ? AND seq = ?`,
			p.Node, p.Seq).Scan(&taskID, &state, &st, &pr, &note)
		if errors.Is(err, sql.ErrNoRows) {
			return bad("resolves suggestion %s/%d, which does not exist", p.Node, p.Seq)
		}
		if err != nil {
			return err
		}
		if state != "pending" {
			break // someone resolved it first, in the canonical order: that resolution stands
		}
		if _, err := x.ExecContext(ctx, `UPDATE suggestions SET state = ?, res_node = ?, res_seq = ? WHERE node = ? AND seq = ?`,
			p.Resolution, op.NodeID, op.Seq, p.Node, p.Seq); err != nil {
			return err
		}
		if p.Resolution == "accepted" {
			var s *task.Status
			var q *task.Priority
			if st.Valid {
				v := task.Status(st.String)
				s = &v
			}
			if pr.Valid {
				v := task.Priority(pr.String)
				q = &v
			}
			if s != nil || q != nil {
				if _, err := setFields(ctx, x, taskID, s, q, ts); err != nil {
					return err
				}
			}
			if strings.TrimSpace(note) != "" {
				if _, err := insertNote(ctx, x, taskID, note, ts, op); err != nil {
					return err
				}
			}
			// Accepting is a person editing the task: it counts as an add for delete's add-wins.
			if err := mark(ctx, x, taskID, op); err != nil {
				return err
			}
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

// setFields changes a task's status and/or priority at time ts and returns how
// many rows matched (1, or 0 if the task does not exist). Shared by a person's
// update and by accepting a suggestion, so both mean exactly the same thing.
func setFields(ctx context.Context, x execer, id string, status *task.Status, priority *task.Priority, ts int64) (int64, error) {
	sets := []string{"updated_at = ?"}
	args := []any{ts}
	if status != nil {
		sets = append(sets, "status = ?", "done_at = ?")
		doneAt := int64(0)
		if *status == task.StatusDone {
			doneAt = ts
		}
		args = append(args, string(*status), doneAt)
	}
	if priority != nil {
		sets = append(sets, "priority = ?")
		args = append(args, string(*priority))
	}
	args = append(args, id)
	res, err := x.ExecContext(ctx, `UPDATE tasks SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// insertNote adds a note to a task, identified by the op that wrote it.
func insertNote(ctx context.Context, x execer, id, text string, ts int64, op Op) (int64, error) {
	res, err := x.ExecContext(ctx, `UPDATE tasks SET updated_at = ? WHERE id = ?`, ts, id)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return n, nil
	}
	_, err = x.ExecContext(ctx, `INSERT INTO task_notes (task_id, n, text, at, node, seq)
		VALUES (?, (SELECT COALESCE(MAX(n), 0) + 1 FROM task_notes WHERE task_id = ?), ?, ?, ?, ?)`,
		id, id, text, ts, op.NodeID, op.Seq)
	return 1, err
}
