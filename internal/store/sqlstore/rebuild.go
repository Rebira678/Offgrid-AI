package sqlstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"offgrid/internal/hlc"
	"offgrid/internal/task"
)

const replayBatch = 1000

// replay applies, in log order (by position), every op in src with pos > after to dst, and
// returns how many it applied and the last position. It reads in batches so a
// large log does not have to fit in memory.
func replay(ctx context.Context, dst, src execer, after int64) (n int, last int64, err error) {
	last = after
	for {
		ops, err := loadOps(ctx, src, last, replayBatch)
		if err != nil {
			return n, last, err
		}
		if len(ops) == 0 {
			return n, last, nil
		}
		for _, op := range ops {
			if err := applyOp(ctx, dst, op); err != nil {
				return n, last, err
			}
			n++
			last = op.Pos
		}
	}
}

// RebuildReport says what a rebuild did.
type RebuildReport struct {
	Ops   int    // ops replayed
	Tasks int    // tasks in the rebuilt view
	Hash  string // StateHash of the rebuilt view
}

// Rebuild throws the materialized view away and recomputes it from the log.
// It is one transaction: if any op cannot be applied (a corrupt log), the old
// view is left exactly as it was and the error names the bad op.
func (s *Store) Rebuild(ctx context.Context) (rep RebuildReport, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return rep, err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	if err = resetProjection(ctx, tx); err != nil {
		return rep, err
	}
	n, last, err := replayCanonical(ctx, tx, tx)
	if err != nil {
		return rep, fmt.Errorf("sqlstore: rebuild: %w", err)
	}
	if err = setMeta(ctx, tx, "applied_pos", strconv.FormatInt(last, 10)); err != nil {
		return rep, err
	}
	if err = setMeta(ctx, tx, "projection_version", projectionVersion); err != nil {
		return rep, err
	}
	hash, err := stateHash(ctx, tx)
	if err != nil {
		return rep, err
	}
	tasks, err := listTasks(ctx, tx, task.Filter{})
	if err != nil {
		return rep, err
	}
	if err = tx.Commit(); err != nil {
		return rep, err
	}
	return RebuildReport{Ops: n, Tasks: len(tasks), Hash: hash}, nil
}

// canonical forms for hashing: explicit fields in a fixed order, times as
// integers, so the hash does not depend on formatting or time zones.
type canonNote struct {
	Text string `json:"text"`
	At   int64  `json:"at"`
}

type canonTask struct {
	ID       string      `json:"id"`
	Title    string      `json:"title"`
	Site     string      `json:"site"`
	Priority string      `json:"priority"`
	Status   string      `json:"status"`
	Created  int64       `json:"created"`
	Updated  int64       `json:"updated"`
	Done     int64       `json:"done"`
	Notes    []canonNote `json:"notes"`
}

// stateHash is a fingerprint of everything in the materialized view. Two
// replicas, or one replica before and after a rebuild, have the same hash if and
// only if they hold the same state. (Day 20 uses this to prove convergence.)
func stateHash(ctx context.Context, x execer) (string, error) {
	tasks, err := listTasks(ctx, x, task.Filter{})
	if err != nil {
		return "", err
	}
	canon := make([]canonTask, 0, len(tasks))
	for _, t := range tasks {
		c := canonTask{
			ID: t.ID, Title: t.Title, Site: t.Site, Priority: string(t.Priority), Status: string(t.Status),
			Created: nanos(t.CreatedAt), Updated: nanos(t.UpdatedAt), Notes: []canonNote{},
		}
		if !t.DoneAt.IsZero() {
			c.Done = nanos(t.DoneAt)
		}
		for _, n := range t.Notes {
			c.Notes = append(c.Notes, canonNote{Text: n.Text, At: nanos(n.At)})
		}
		canon = append(canon, c)
	}
	raw, err := json.Marshal(canon)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// StateHash fingerprints the current materialized view.
func (s *Store) StateHash(ctx context.Context) (string, error) {
	return stateHash(ctx, s.db)
}

// ErrDiverged means the materialized view does not match what the log says.
var ErrDiverged = errors.New("the materialized view does not match the op log")

// VerifyReport says what Verify checked.
type VerifyReport struct {
	Ops        int
	LiveHash   string // hash of the view in the database
	ReplayHash string // hash of the view recomputed from the log in a scratch database
}

// Verify recomputes the view from the log in a separate scratch database and
// compares it with the live view, without touching the live one. It also checks
// that every replica's sequence numbers have no gaps. A mismatch returns
// ErrDiverged (the view was edited or damaged: Rebuild fixes it); a bad log
// returns ErrCorruptLog (a person must look).
func (s *Store) Verify(ctx context.Context) (VerifyReport, error) {
	var rep VerifyReport
	if err := s.checkSequences(ctx); err != nil {
		return rep, err
	}
	scratch, err := sql.Open(driverName, dsn(":memory:"))
	if err != nil {
		return rep, err
	}
	defer scratch.Close()
	scratch.SetMaxOpenConns(1)
	if err := resetProjection(ctx, scratch); err != nil {
		return rep, err
	}
	n, _, err := replayCanonical(ctx, scratch, s.db)
	if err != nil {
		return rep, fmt.Errorf("sqlstore: verify: %w", err)
	}
	rep.Ops = n
	if rep.ReplayHash, err = stateHash(ctx, scratch); err != nil {
		return rep, err
	}
	if rep.LiveHash, err = stateHash(ctx, s.db); err != nil {
		return rep, err
	}
	if rep.LiveHash != rep.ReplayHash {
		return rep, fmt.Errorf("%w (live %s, replayed %s)", ErrDiverged, rep.LiveHash[:12], rep.ReplayHash[:12])
	}
	return rep, nil
}

// checkSequences makes sure each replica's ops are numbered 1..N with no gaps.
// A gap means an op is missing, and every later op may have been built on it.
func (s *Store) checkSequences(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT node_id, COUNT(*), MAX(seq) FROM ops GROUP BY node_id ORDER BY node_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var node string
		var count, max int64
		if err := rows.Scan(&node, &count, &max); err != nil {
			return err
		}
		if count != max {
			return &CorruptOpError{Kind: "sequence", Reason: fmt.Sprintf("replica %q has %d ops but its highest sequence number is %d: some are missing", node, count, max)}
		}
	}
	return rows.Err()
}

// Info describes the store.
type Info struct {
	NodeID            string
	SchemaVersion     int
	ProjectionVersion string
	Ops               int
	LastPos           int64
	AppliedPos        int64
	Tasks             int
	StateHash         string
	// NewestStamp is the newest op timestamp in the log (zero if empty), and
	// ClockAhead how far it is beyond the physical clock now. A large
	// ClockAhead means an op was once stamped from a clock that was far ahead
	// (or the physical clock is now far behind).
	NewestStamp hlc.Timestamp
	ClockAhead  time.Duration
}

func (s *Store) Info(ctx context.Context) (Info, error) {
	info := Info{NodeID: s.node}
	var err error
	if info.SchemaVersion, err = schemaVersion(ctx, s.db); err != nil {
		return info, err
	}
	info.ProjectionVersion, _, err = getMeta(ctx, s.db, "projection_version")
	if err != nil {
		return info, err
	}
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(pos), 0) FROM ops`).Scan(&info.Ops, &info.LastPos); err != nil {
		return info, err
	}
	if info.AppliedPos, err = appliedPos(ctx, s.db); err != nil {
		return info, err
	}
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks`).Scan(&info.Tasks); err != nil {
		return info, err
	}
	if info.NewestStamp, err = s.newestStamp(ctx, s.db); err != nil {
		return info, err
	}
	info.ClockAhead = s.clock.Ahead()
	info.StateHash, err = stateHash(ctx, s.db)
	return info, err
}

// Ops returns up to limit ops with position greater than after, in log order.
func (s *Store) Ops(ctx context.Context, after int64, limit int) ([]Op, error) {
	if limit < 1 {
		limit = replayBatch
	}
	return loadOps(ctx, s.db, after, limit)
}

// LastOps returns the most recent n ops, oldest first.
func (s *Store) LastOps(ctx context.Context, n int) ([]Op, error) {
	if n < 1 {
		return nil, nil
	}
	var first int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MIN(pos), 0) FROM (SELECT pos FROM ops ORDER BY pos DESC LIMIT ?)`, n).Scan(&first)
	if err != nil {
		return nil, err
	}
	if first == 0 {
		return nil, nil
	}
	return loadOps(ctx, s.db, first-1, n)
}
