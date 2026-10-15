package jobqueue

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"regexp"
	"strings"
	"time"
)

// Status is where a job is in its life.
//
//	queued ──claim──▶ running ──complete──▶ done
//	   ▲                 │
//	   └──retry later────┤ fail (attempts left)
//	                     └─ fail (none left, or permanent) ──▶ dead
//
// "dead" is the dead-letter state: the job gave up, but it is kept, with its
// last error, so a person can look at it and requeue it.
type Status string

const (
	Queued  Status = "queued"
	Running Status = "running"
	Done    Status = "done"
	Dead    Status = "dead"
)

func (s Status) valid() bool {
	return s == Queued || s == Running || s == Done || s == Dead
}

// Job is one unit of work.
type Job struct {
	ID          int64
	Key         string // idempotency key: the same key is the same job
	Kind        string // what to do, e.g. "summarize"
	Payload     json.RawMessage
	Status      Status
	Attempts    int // how many times it has been claimed
	MaxAttempts int
	RunAt       time.Time // earliest time it may run (backoff pushes this forward)
	LeaseUntil  time.Time // while running: when the claim expires
	LastError   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Errors callers can test for with errors.Is.
var (
	// ErrKeyConflict: the idempotency key exists but with different content.
	// Silently returning the old job would hide a bug in whoever chose the key.
	ErrKeyConflict = errors.New("jobqueue: that key is already used by a different job")
	// ErrStaleClaim: the claim is no longer yours. Your lease expired and the
	// job was handed to someone else, or it was already finished.
	ErrStaleClaim = errors.New("jobqueue: this claim is no longer valid")
	// ErrNotFound: no such job.
	ErrNotFound = errors.New("jobqueue: no such job")
	// ErrNotDead: only dead jobs can be requeued.
	ErrNotDead = errors.New("jobqueue: only a dead job can be requeued")
	// ErrNewerSchema: the file was written by a newer Offgrid.
	ErrNewerSchema = errors.New("jobqueue: the queue file was created by a newer version of Offgrid")
)

// Limits protect the queue from a runaway producer.
const (
	maxKey     = 200
	maxPayload = 64 << 10
)

var kindRe = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)

// Config tunes a Queue. Zero values get sensible defaults.
type Config struct {
	Backoff            Backoff
	DefaultMaxAttempts int              // default 5
	Now                func() time.Time // injectable clock for tests
	Rand               func() float64   // injectable randomness in [0,1) for tests
}

// Queue is the durable job queue. It is safe for concurrent use.
type Queue struct {
	db  *sql.DB
	cfg Config
}

// New prepares the queue's tables in db (creating or upgrading them) and
// returns a Queue. db should come from sqlstore.OpenDB. The caller keeps
// ownership of db and closes it.
func New(ctx context.Context, db *sql.DB, cfg Config) (*Queue, error) {
	if cfg.Backoff == (Backoff{}) {
		cfg.Backoff = DefaultBackoff()
	}
	if err := cfg.Backoff.Validate(); err != nil {
		return nil, fmt.Errorf("jobqueue: %w", err)
	}
	if cfg.DefaultMaxAttempts <= 0 {
		cfg.DefaultMaxAttempts = 5
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.Float64
	}
	if err := migrate(ctx, db); err != nil {
		return nil, err
	}
	return &Queue{db: db, cfg: cfg}, nil
}

// ---- schema -----------------------------------------------------------

// migrations are applied in order; the number applied is kept in SQLite's
// built-in PRAGMA user_version, so no bookkeeping table is needed. Never edit
// one that has shipped; append a new one.
var migrations = []string{
	`CREATE TABLE jobs (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		key          TEXT    NOT NULL UNIQUE,          -- idempotency key
		kind         TEXT    NOT NULL,
		payload      TEXT    NOT NULL CHECK (json_valid(payload)),
		status       TEXT    NOT NULL CHECK (status IN ('queued','running','done','dead')),
		attempts     INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
		max_attempts INTEGER NOT NULL CHECK (max_attempts >= 1),
		run_at       INTEGER NOT NULL,                 -- unix ns: not before this time
		lease_until  INTEGER NOT NULL DEFAULT 0,       -- unix ns: when a running claim expires
		last_error   TEXT    NOT NULL DEFAULT '',
		created_at   INTEGER NOT NULL,
		updated_at   INTEGER NOT NULL
	);
	CREATE INDEX jobs_ready ON jobs (status, run_at, id);`,
}

func migrate(ctx context.Context, db *sql.DB) error {
	var have int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&have); err != nil {
		return fmt.Errorf("jobqueue: read schema version: %w", err)
	}
	if have > len(migrations) {
		return fmt.Errorf("%w (file is version %d, this build knows %d)", ErrNewerSchema, have, len(migrations))
	}
	for v := have; v < len(migrations); v++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("jobqueue: migrate: %w", err)
		}
		if _, err := tx.ExecContext(ctx, migrations[v]); err != nil {
			tx.Rollback()
			return fmt.Errorf("jobqueue: migration %d: %w", v+1, err)
		}
		// user_version changes inside the transaction, so a crash can never
		// leave the table created but the version not recorded (or the reverse).
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, v+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("jobqueue: migration %d: %w", v+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("jobqueue: migration %d: %w", v+1, err)
		}
	}
	return nil
}

// ---- time helpers -----------------------------------------------------

func nanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func fromNanos(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

const jobCols = `id, key, kind, payload, status, attempts, max_attempts, run_at, lease_until, last_error, created_at, updated_at`

type scanner interface{ Scan(dest ...any) error }

func scanJob(s scanner) (Job, error) {
	var j Job
	var payload, status string
	var runAt, lease, created, updated int64
	if err := s.Scan(&j.ID, &j.Key, &j.Kind, &payload, &status, &j.Attempts, &j.MaxAttempts, &runAt, &lease, &j.LastError, &created, &updated); err != nil {
		return Job{}, err
	}
	j.Payload = json.RawMessage(payload)
	j.Status = Status(status)
	j.RunAt, j.LeaseUntil, j.CreatedAt, j.UpdatedAt = fromNanos(runAt), fromNanos(lease), fromNanos(created), fromNanos(updated)
	return j, nil
}

// ---- enqueue ----------------------------------------------------------

// EnqueueOptions are optional settings for Enqueue.
type EnqueueOptions struct {
	MaxAttempts int       // 0 means the queue's default
	RunAt       time.Time // zero means "as soon as possible"
}

// Enqueue stores a job and returns it. created is false when the key already
// existed with the same kind and payload: that is an idempotent repeat, and
// the existing job is returned untouched. A retry of the same request (the
// user pressed send twice, the program crashed after enqueue but before
// printing "queued") therefore never creates a duplicate.
//
// Once Enqueue returns, the job is on disk.
func (q *Queue) Enqueue(ctx context.Context, key, kind string, payload json.RawMessage, opt EnqueueOptions) (job Job, created bool, err error) {
	if err := validate(key, kind); err != nil {
		return Job{}, false, err
	}
	payload, err = normalizePayload(payload)
	if err != nil {
		return Job{}, false, err
	}
	max := opt.MaxAttempts
	if max == 0 {
		max = q.cfg.DefaultMaxAttempts
	}
	if max < 1 {
		return Job{}, false, fmt.Errorf("jobqueue: max attempts must be at least 1 (got %d)", max)
	}

	err = q.tx(ctx, func(tx *sql.Tx) error {
		now := q.cfg.Now()
		runAt := opt.RunAt
		if runAt.IsZero() {
			runAt = now
		}
		// Look first, insert only if the key is new. (INSERT ... ON CONFLICT DO
		// NOTHING would also be correct, but SQLite still burns an id number
		// on the attempt, leaving visible gaps: job #2, then #4.) The write
		// lock is already held, so nobody can add the key in between.
		existing, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE key = ?`, key))
		switch {
		case err == nil:
			if existing.Kind != kind || !sameJSON(existing.Payload, payload) {
				return ErrKeyConflict
			}
			job, created = existing, false
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO jobs (key, kind, payload, status, max_attempts, run_at, created_at, updated_at)
			VALUES (?, ?, ?, 'queued', ?, ?, ?, ?)`, key, kind, string(payload), max, nanos(runAt), nanos(now), nanos(now)); err != nil {
			return err
		}
		job, err = scanJob(tx.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE key = ?`, key))
		if err != nil {
			return err
		}
		created = true
		return nil
	})
	if err != nil {
		return Job{}, false, err
	}
	return job, created, nil
}

func validate(key, kind string) error {
	if strings.TrimSpace(key) == "" || len(key) > maxKey {
		return fmt.Errorf("jobqueue: key must be 1 to %d characters", maxKey)
	}
	if !kindRe.MatchString(kind) {
		return fmt.Errorf("jobqueue: kind %q must start with a lower-case letter and use only a-z, 0-9, '.', '_' or '-' (at most 64)", kind)
	}
	return nil
}

func normalizePayload(p json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(p)) == 0 {
		return json.RawMessage(`{}`), nil
	}
	if len(p) > maxPayload {
		return nil, fmt.Errorf("jobqueue: payload is %d bytes, at most %d", len(p), maxPayload)
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, p); err != nil {
		return nil, fmt.Errorf("jobqueue: payload is not valid JSON: %w", err)
	}
	return buf.Bytes(), nil
}

func sameJSON(a, b json.RawMessage) bool {
	var ca, cb bytes.Buffer
	if json.Compact(&ca, a) != nil || json.Compact(&cb, b) != nil {
		return false
	}
	return bytes.Equal(ca.Bytes(), cb.Bytes())
}

// ---- claim / complete / fail -----------------------------------------

// Claim hands out the next job that is ready, or reports that there is none.
// The job is leased to the caller for `lease`: if the caller crashes or hangs,
// the lease expires and the job becomes claimable again (a lost worker must
// not strand a job in "running" forever). Each claim counts as an attempt.
//
// The returned job carries Attempts, which is the caller's fencing token:
// pass it back to Complete, Fail and Extend. If the lease expired and another
// worker claimed the job, the token no longer matches and the stale worker's
// report is rejected with ErrStaleClaim instead of overwriting the new one.
func (q *Queue) Claim(ctx context.Context, lease time.Duration) (job Job, ok bool, err error) {
	if lease <= 0 {
		return Job{}, false, errors.New("jobqueue: lease must be positive")
	}
	err = q.tx(ctx, func(tx *sql.Tx) error {
		now := nanos(q.cfg.Now())

		// A job whose worker vanished on its LAST attempt has nothing left to
		// retry with: it goes to the dead-letter state rather than running forever.
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status='dead', lease_until=0, updated_at=?,
				last_error='lease expired on the final attempt: the worker never reported back'
			WHERE status='running' AND lease_until <= ? AND attempts >= max_attempts`, now, now); err != nil {
			return err
		}

		row := tx.QueryRowContext(ctx, `UPDATE jobs SET
				last_error = CASE WHEN status='running' THEN 'lease expired: the worker never reported back' ELSE last_error END,
				status='running', attempts=attempts+1, lease_until=?, updated_at=?
			WHERE id = (
				SELECT id FROM jobs
				WHERE (status='queued' AND run_at <= ?) OR (status='running' AND lease_until <= ?)
				ORDER BY run_at, id LIMIT 1)
			RETURNING `+jobCols, now+int64(lease), now, now, now)
		j, err := scanJob(row)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		job, ok = j, true
		return nil
	})
	if err != nil {
		return Job{}, false, err
	}
	return job, ok, nil
}

// Complete marks a claimed job done.
func (q *Queue) Complete(ctx context.Context, id int64, attempt int) error {
	return q.tx(ctx, func(tx *sql.Tx) error {
		now := nanos(q.cfg.Now())
		res, err := tx.ExecContext(ctx, `UPDATE jobs SET status='done', lease_until=0, updated_at=?
			WHERE id=? AND status='running' AND attempts=?`, now, id, attempt)
		if err != nil {
			return err
		}
		return q.mustHit(ctx, tx, res, id)
	})
}

// Extend renews the lease of a claimed job, for work that takes longer than
// the lease (a model summarizing on a CPU can take minutes).
func (q *Queue) Extend(ctx context.Context, id int64, attempt int, lease time.Duration) error {
	if lease <= 0 {
		return errors.New("jobqueue: lease must be positive")
	}
	return q.tx(ctx, func(tx *sql.Tx) error {
		now := q.cfg.Now()
		res, err := tx.ExecContext(ctx, `UPDATE jobs SET lease_until=?, updated_at=?
			WHERE id=? AND status='running' AND attempts=?`, nanos(now.Add(lease)), nanos(now), id, attempt)
		if err != nil {
			return err
		}
		return q.mustHit(ctx, tx, res, id)
	})
}

// FailResult says what happened to a job that was reported failed.
type FailResult struct {
	Status Status    // Queued (will retry) or Dead
	RunAt  time.Time // when the retry may run (zero if Dead)
	Delay  time.Duration
}

// Fail reports that an attempt failed. If the job has attempts left and the
// failure is not permanent, it goes back to queued, not to run before the
// backoff delay has passed. Otherwise it goes to dead, keeping cause.
// permanent is for errors that retrying cannot fix (the payload is invalid).
func (q *Queue) Fail(ctx context.Context, id int64, attempt int, cause error, permanent bool) (FailResult, error) {
	var out FailResult
	msg := "unknown error"
	if cause != nil {
		msg = cause.Error()
	}
	if len(msg) > 1000 {
		msg = msg[:1000]
	}
	err := q.tx(ctx, func(tx *sql.Tx) error {
		var max int
		err := tx.QueryRowContext(ctx, `SELECT max_attempts FROM jobs WHERE id=? AND status='running' AND attempts=?`, id, attempt).Scan(&max)
		if errors.Is(err, sql.ErrNoRows) {
			return q.missingOrStale(ctx, tx, id)
		}
		if err != nil {
			return err
		}
		now := q.cfg.Now()
		if permanent || attempt >= max {
			out = FailResult{Status: Dead}
		} else {
			d := q.cfg.Backoff.Delay(attempt, q.cfg.Rand())
			out = FailResult{Status: Queued, RunAt: now.Add(d), Delay: d}
		}
		_, err = tx.ExecContext(ctx, `UPDATE jobs SET status=?, run_at=CASE WHEN ?='queued' THEN ? ELSE run_at END,
				lease_until=0, last_error=?, updated_at=?
			WHERE id=? AND status='running' AND attempts=?`,
			string(out.Status), string(out.Status), nanos(out.RunAt), msg, nanos(now), id, attempt)
		return err
	})
	if err != nil {
		return FailResult{}, err
	}
	return out, nil
}

// Retry puts a dead job back in the queue with a fresh set of attempts.
// This is the operator's decision, never automatic.
func (q *Queue) Retry(ctx context.Context, id int64) error {
	return q.tx(ctx, func(tx *sql.Tx) error {
		now := nanos(q.cfg.Now())
		res, err := tx.ExecContext(ctx, `UPDATE jobs SET status='queued', attempts=0, run_at=?, lease_until=0, updated_at=?
			WHERE id=? AND status='dead'`, now, now, id)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 1 {
			return nil
		}
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM jobs WHERE id=?`, id).Scan(&one); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return ErrNotDead
	})
}

func (q *Queue) mustHit(ctx context.Context, tx *sql.Tx, res sql.Result, id int64) error {
	if n, _ := res.RowsAffected(); n == 1 {
		return nil
	}
	return q.missingOrStale(ctx, tx, id)
}

func (q *Queue) missingOrStale(ctx context.Context, tx *sql.Tx, id int64) error {
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM jobs WHERE id=?`, id).Scan(&one); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return ErrStaleClaim
}

// ---- reading ----------------------------------------------------------

// Get returns one job.
func (q *Queue) Get(ctx context.Context, id int64) (Job, error) {
	j, err := scanJob(q.db.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	return j, err
}

// List returns jobs oldest first. An empty status means all of them; limit 0
// means no limit.
func (q *Queue) List(ctx context.Context, status Status, limit int) ([]Job, error) {
	if status != "" && !status.valid() {
		return nil, fmt.Errorf("jobqueue: unknown status %q", status)
	}
	query := `SELECT ` + jobCols + ` FROM jobs`
	var args []any
	if status != "" {
		query += ` WHERE status=?`
		args = append(args, string(status))
	}
	query += ` ORDER BY id`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := q.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// Stats summarizes the queue.
type Stats struct {
	Queued, Running, Done, Dead int
	// NextReady is when the next job becomes claimable: the earliest queued
	// run_at or expiring lease. Zero if there is nothing waiting. A worker
	// that has nothing to do right now can sleep until then.
	NextReady time.Time
}

// Total is every job in the queue.
func (s Stats) Total() int { return s.Queued + s.Running + s.Done + s.Dead }

func (q *Queue) Stats(ctx context.Context) (Stats, error) {
	var s Stats
	rows, err := q.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM jobs GROUP BY status`)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return s, err
		}
		switch Status(st) {
		case Queued:
			s.Queued = n
		case Running:
			s.Running = n
		case Done:
			s.Done = n
		case Dead:
			s.Dead = n
		}
	}
	if err := rows.Err(); err != nil {
		return s, err
	}
	var next sql.NullInt64
	err = q.db.QueryRowContext(ctx, `SELECT MIN(t) FROM (
			SELECT run_at AS t FROM jobs WHERE status='queued'
			UNION ALL SELECT lease_until FROM jobs WHERE status='running')`).Scan(&next)
	if err != nil {
		return s, err
	}
	if next.Valid {
		s.NextReady = fromNanos(next.Int64)
	}
	return s, nil
}

// tx runs fn in a transaction (SQLite takes the write lock at the start; see
// _txlock in the DSN), committing if fn returns nil and rolling back if not.
func (q *Queue) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("jobqueue: %w", err)
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("jobqueue: commit: %w", err)
	}
	return nil
}
