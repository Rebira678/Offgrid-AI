// Package sqlstore is Offgrid's persistent task store: SQLite, event-sourced.
//
// THE DESIGN IN ONE PARAGRAPH. Nothing is ever updated in place. Every change
// is appended to the ops table as an immutable fact ("task T3 was created",
// "task T3 became done"), tagged with the replica that recorded it and that
// replica's own counter. The tasks and task_notes tables are a materialized
// VIEW of the log, rebuilt by replaying it, so they can be deleted at any time
// without losing anything. The log is the truth; the tables are a cache.
//
// WHY. Sync (Days 15 to 20) needs to know WHAT HAPPENED, not just what the
// state is. Comparing two states cannot tell "I edited it and you did not" from
// "you deleted it and I edited it". Comparing two logs can: each op has an
// identity (node, seq), so replicas exchange only the ops the other lacks, and
// applying the same op twice is detectable instead of harmful.
package sqlstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"offgrid/internal/hlc"
	"offgrid/internal/task"
)

// Store implements task.Store on top of SQLite.
type Store struct {
	db   *sql.DB
	node string
	now  func() time.Time

	// clock stamps every op this replica records. It is restored from the
	// newest stored timestamp on open, so a restart can never go backwards.
	clock *hlc.Clock
}

var _ task.Store = (*Store)(nil)

// Options configures Open.
type Options struct {
	// NodeID names this replica. Empty means: use the one stored in the
	// database, or generate a random one the first time. Once a database has a
	// node ID, a different one is refused: the ID is the replica's identity and
	// the sequence numbers only make sense under it.
	NodeID string
	// Now is the physical clock the hybrid logical clock reads. nil means
	// time.Now. Ops are stamped from the HLC, not from Now directly: if Now
	// stalls or goes backwards, op timestamps still increase.
	Now func() time.Time
	// MaxDrift bounds how far ahead of Now a timestamp received from another
	// replica may be (see hlc.Config). Zero is hlc.DefaultMaxDrift.
	MaxDrift time.Duration
}

// ErrNodeMismatch means the database belongs to a different replica.
var ErrNodeMismatch = errors.New("database belongs to a different node")

var nodeIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// Open opens (creating if needed) the database at path. Use ":memory:" for a
// throwaway store. It migrates the schema, settles the node ID, and makes sure
// the materialized view is present, current and caught up with the log.
func Open(ctx context.Context, path string, opt Options) (*Store, error) {
	if path == "" {
		return nil, errors.New("sqlstore: empty database path")
	}
	if opt.NodeID != "" && !nodeIDRe.MatchString(opt.NodeID) {
		return nil, fmt.Errorf("sqlstore: node ID %q must be 1 to 64 letters, digits, '.', '_' or '-'", opt.NodeID)
	}
	db, err := sql.Open(driverName, dsn(path))
	if err != nil {
		return nil, fmt.Errorf("sqlstore: open %s: %w", path, err)
	}
	// One connection: SQLite has one writer anyway, and it makes ":memory:"
	// databases behave (each connection to ":memory:" is a separate database).
	db.SetMaxOpenConns(1)

	s := &Store{db: db, now: opt.Now}
	if s.now == nil {
		s.now = time.Now
	}
	s.clock = hlc.New(hlc.Config{Now: s.now, MaxDrift: opt.MaxDrift})
	if err := s.init(ctx, opt); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) init(ctx context.Context, opt Options) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("sqlstore: %w", err)
	}
	if err := migrate(ctx, s.db, migrations); err != nil {
		return err
	}
	node, err := s.settleNodeID(ctx, opt.NodeID)
	if err != nil {
		return err
	}
	s.node = node
	if err := s.restoreClock(ctx); err != nil {
		return err
	}
	return s.ensureProjection(ctx)
}

// restoreClock raises the clock to the newest timestamp in the log. Without it
// a restart after the physical clock was reset (a dead battery, a step back by
// NTP) would issue timestamps older than ones already stored, and a later edit
// would sort before an earlier one.
func (s *Store) restoreClock(ctx context.Context) error {
	last, err := s.newestStamp(ctx, s.db)
	if err != nil {
		return err
	}
	s.clock.Restore(last)
	return nil
}

func (s *Store) newestStamp(ctx context.Context, x execer) (hlc.Timestamp, error) {
	var wall int64
	var counter uint32
	err := x.QueryRowContext(ctx, `SELECT ts, counter FROM ops ORDER BY ts DESC, counter DESC LIMIT 1`).Scan(&wall, &counter)
	if errors.Is(err, sql.ErrNoRows) {
		return hlc.Timestamp{}, nil
	}
	if err != nil {
		return hlc.Timestamp{}, err
	}
	return hlc.Timestamp{Wall: wall, Counter: counter}, nil
}

// Clock is this replica's hybrid logical clock. The sync layer calls Update on
// it with the timestamps of the ops it receives, so that whatever this replica
// does next is stamped after everything it has seen.
func (s *Store) Clock() *hlc.Clock { return s.clock }

// settleNodeID returns this database's node ID, creating it if this is a new
// database, and refusing a conflicting request.
func (s *Store) settleNodeID(ctx context.Context, want string) (string, error) {
	have, ok, err := getMeta(ctx, s.db, "node_id")
	if err != nil {
		return "", err
	}
	switch {
	case ok && want != "" && want != have:
		return "", fmt.Errorf("%w: it is %q, but %q was requested", ErrNodeMismatch, have, want)
	case ok:
		return have, nil
	}
	if want == "" {
		var b [6]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", fmt.Errorf("sqlstore: generate node ID: %w", err)
		}
		want = "n-" + hex.EncodeToString(b[:])
	}
	if err := setMeta(ctx, s.db, "node_id", want); err != nil {
		return "", err
	}
	return want, nil
}

// ensureProjection makes the materialized view usable: it rebuilds from the log
// if the view is missing or was built by a different projectionVersion, and
// applies any ops the view has not seen yet.
func (s *Store) ensureProjection(ctx context.Context) error {
	version, _, err := getMeta(ctx, s.db, "projection_version")
	if err != nil {
		return err
	}
	var present int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('tasks', 'task_notes')`).Scan(&present); err != nil {
		return err
	}
	if version != projectionVersion || present != 2 {
		_, err := s.Rebuild(ctx)
		return err
	}
	return s.catchUp(ctx)
}

// catchUp applies ops recorded after the last one the view has applied. Normal
// writes keep the view current in the same transaction, so this only does work
// if ops arrived another way (later: from sync).
func (s *Store) catchUp(ctx context.Context) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	applied, err := appliedPos(ctx, tx)
	if err != nil {
		return err
	}
	n, last, err := replay(ctx, tx, tx, applied)
	if err != nil {
		return err
	}
	if n > 0 {
		if err = setMeta(ctx, tx, "applied_pos", strconv.FormatInt(last, 10)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// NodeID is this replica's identity.
func (s *Store) NodeID() string { return s.node }

// ---- meta -------------------------------------------------------------------

func getMeta(ctx context.Context, x execer, key string) (string, bool, error) {
	var v string
	err := x.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

func setMeta(ctx context.Context, x execer, key, value string) error {
	_, err := x.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func appliedPos(ctx context.Context, x execer) (int64, error) {
	v, ok, err := getMeta(ctx, x, "applied_pos")
	if err != nil || !ok {
		return 0, err
	}
	return strconv.ParseInt(v, 10, 64)
}

// ---- writing ----------------------------------------------------------------

// write runs fn in a transaction. If fn returns an error the transaction is
// rolled back: nothing it did, in the log or in the view, is kept.
func (s *Store) write(ctx context.Context, fn func(tx *sql.Tx) error) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// record appends one op to the log and applies it to the view, in the caller's
// transaction. The two happen together or not at all, so the view can never be
// ahead of, or behind, the log after a commit.
//
// This is where write-time decisions are frozen into the op: the sequence
// number and the hybrid logical clock timestamp are chosen here, once, and
// stored.
func (s *Store) record(ctx context.Context, tx *sql.Tx, kind string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("sqlstore: encode %s: %w", kind, err)
	}
	var seq int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM ops WHERE node_id = ?`, s.node).Scan(&seq); err != nil {
		return err
	}
	stamp := s.clock.Now()
	res, err := tx.ExecContext(ctx, `INSERT INTO ops (node_id, seq, ts, counter, kind, payload) VALUES (?, ?, ?, ?, ?, ?)`,
		s.node, seq, stamp.Wall, stamp.Counter, kind, string(raw))
	if err != nil {
		return fmt.Errorf("sqlstore: append %s: %w", kind, err)
	}
	pos, err := res.LastInsertId()
	if err != nil {
		return err
	}
	op := Op{Pos: pos, NodeID: s.node, Seq: seq, TS: stamp.Time(), Counter: stamp.Counter, Kind: kind, Payload: raw}
	if err := applyOp(ctx, tx, op); err != nil {
		return err
	}
	return setMeta(ctx, tx, "applied_pos", strconv.FormatInt(pos, 10))
}

func (s *Store) Create(ctx context.Context, n task.NewTask) (task.Task, bool, error) {
	if err := ctx.Err(); err != nil {
		return task.Task{}, false, err
	}
	title := strings.TrimSpace(n.Title)
	if title == "" {
		return task.Task{}, false, fmt.Errorf("%w: empty title", task.ErrInvalid)
	}
	if _, err := task.ParsePriority(string(n.Priority)); err != nil {
		return task.Task{}, false, fmt.Errorf("%w: %v", task.ErrInvalid, err)
	}

	var out task.Task
	var created bool
	err := s.write(ctx, func(tx *sql.Tx) error {
		// An unfinished task with the same title and site is the same task:
		// return it and record nothing. The check and the insert share one
		// write-locked transaction, so two racing requests cannot both pass it.
		var dupID string
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM tasks WHERE status != 'done' AND title_key = ? AND site_key = ? ORDER BY n LIMIT 1`,
			task.TitleKey(title), strings.ToLower(n.Site)).Scan(&dupID)
		if err == nil {
			out, err = getTask(ctx, tx, dupID)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		// The ID is decided HERE and written into the op, never at apply time.
		// (It is unique within this replica. Making IDs unique across replicas
		// is a sync-day problem; see docs/day-06.md.)
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM ops WHERE kind = ?`, KindTaskCreated).Scan(&count); err != nil {
			return err
		}
		id := fmt.Sprintf("T%d", count+1)
		if err := s.record(ctx, tx, KindTaskCreated, createdPayload{ID: id, Title: title, Site: n.Site, Priority: n.Priority}); err != nil {
			return err
		}
		created = true
		out, err = getTask(ctx, tx, id)
		return err
	})
	if err != nil {
		return task.Task{}, false, err
	}
	return out, created, nil
}

func (s *Store) Update(ctx context.Context, id string, c task.Changes) (task.Task, bool, error) {
	if err := ctx.Err(); err != nil {
		return task.Task{}, false, err
	}
	if c.Status == nil && c.Priority == nil {
		return task.Task{}, false, fmt.Errorf("%w: nothing to change", task.ErrInvalid)
	}
	if c.Status != nil {
		if _, err := task.ParseStatus(string(*c.Status)); err != nil {
			return task.Task{}, false, fmt.Errorf("%w: %v", task.ErrInvalid, err)
		}
	}
	if c.Priority != nil {
		if _, err := task.ParsePriority(string(*c.Priority)); err != nil {
			return task.Task{}, false, fmt.Errorf("%w: %v", task.ErrInvalid, err)
		}
	}

	var out task.Task
	var changed bool
	err := s.write(ctx, func(tx *sql.Tx) error {
		cur, err := getTask(ctx, tx, id)
		if err != nil {
			return err
		}
		// Record only what actually changes. An update that changes nothing
		// leaves no trace in the log.
		var p updatedPayload
		p.ID = id
		if c.Status != nil && *c.Status != cur.Status {
			p.Status = c.Status
		}
		if c.Priority != nil && *c.Priority != cur.Priority {
			p.Priority = c.Priority
		}
		if p.Status == nil && p.Priority == nil {
			out = cur
			return nil
		}
		if err := s.record(ctx, tx, KindTaskUpdated, p); err != nil {
			return err
		}
		changed = true
		out, err = getTask(ctx, tx, id)
		return err
	})
	if err != nil {
		return task.Task{}, false, err
	}
	return out, changed, nil
}

func (s *Store) AddNote(ctx context.Context, id, text string) (task.Task, error) {
	if err := ctx.Err(); err != nil {
		return task.Task{}, err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return task.Task{}, fmt.Errorf("%w: empty note", task.ErrInvalid)
	}
	var out task.Task
	err := s.write(ctx, func(tx *sql.Tx) error {
		if _, err := getTask(ctx, tx, id); err != nil {
			return err
		}
		if err := s.record(ctx, tx, KindNoteAdded, notePayload{ID: id, Text: text}); err != nil {
			return err
		}
		var err error
		out, err = getTask(ctx, tx, id)
		return err
	})
	return out, err
}

// ---- reading ----------------------------------------------------------------

const taskColumns = `id, title, site, priority, status, created_at, updated_at, done_at`

func scanTask(row interface{ Scan(...any) error }) (task.Task, error) {
	var t task.Task
	var prio, status string
	var created, updated, done int64
	if err := row.Scan(&t.ID, &t.Title, &t.Site, &prio, &status, &created, &updated, &done); err != nil {
		return task.Task{}, err
	}
	t.Priority, t.Status = task.Priority(prio), task.Status(status)
	t.CreatedAt, t.UpdatedAt, t.DoneAt = fromNanos(created), fromNanos(updated), fromNanos(done)
	return t, nil
}

// getTask loads one task with its notes, or task.ErrNotFound.
func getTask(ctx context.Context, x execer, id string) (task.Task, error) {
	t, err := scanTask(x.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return task.Task{}, task.ErrNotFound
	}
	if err != nil {
		return task.Task{}, err
	}
	rows, err := x.QueryContext(ctx, `SELECT text, at FROM task_notes WHERE task_id = ? ORDER BY n`, id)
	if err != nil {
		return task.Task{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var n task.Note
		var at int64
		if err := rows.Scan(&n.Text, &at); err != nil {
			return task.Task{}, err
		}
		n.At = fromNanos(at)
		t.Notes = append(t.Notes, n)
	}
	return t, rows.Err()
}

func (s *Store) Get(ctx context.Context, id string) (task.Task, error) {
	if err := ctx.Err(); err != nil {
		return task.Task{}, err
	}
	return getTask(ctx, s.db, id)
}

func (s *Store) List(ctx context.Context, f task.Filter) ([]task.Task, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return listTasks(ctx, s.db, f)
}

// listTasks is shared with the state hash, which reads the whole view.
func listTasks(ctx context.Context, x execer, f task.Filter) ([]task.Task, error) {
	var where []string
	var args []any
	switch {
	case f.Status != "":
		where, args = append(where, "status = ?"), append(args, string(f.Status))
	case f.ExcludeDone:
		where = append(where, "status != 'done'")
	}
	if f.Priority != "" {
		where, args = append(where, "priority = ?"), append(args, string(f.Priority))
	}
	if f.Site != "" {
		where, args = append(where, "site_key = ?"), append(args, strings.ToLower(f.Site))
	}
	if f.Within != nil {
		where, args = append(where, "created_at >= ? AND created_at < ?"), append(args, nanos(f.Within.Start), nanos(f.Within.End))
	}
	q := `SELECT ` + taskColumns + ` FROM tasks`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	q += ` ORDER BY n`

	rows, err := x.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	out := []task.Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	// Notes for the tasks found, in one pass.
	if len(out) == 0 {
		return out, nil
	}
	index := make(map[string]int, len(out))
	for i, t := range out {
		index[t.ID] = i
	}
	nrows, err := x.QueryContext(ctx, `SELECT task_id, text, at FROM task_notes ORDER BY task_id, n`)
	if err != nil {
		return nil, err
	}
	defer nrows.Close()
	for nrows.Next() {
		var id string
		var n task.Note
		var at int64
		if err := nrows.Scan(&id, &n.Text, &at); err != nil {
			return nil, err
		}
		n.At = fromNanos(at)
		if i, ok := index[id]; ok {
			out[i].Notes = append(out[i].Notes, n)
		}
	}
	return out, nrows.Err()
}
