package sqlstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"offgrid/internal/hlc"
	"offgrid/internal/task"
)

// This file is what a replica needs in order to take part in sync: say which
// ops it has, hand out the ones another replica lacks, and take in ops from
// another replica without ever applying the same one twice.
//
// The rules that make it safe:
//
//   - An op's identity is (node, seq). A replica's ops are numbered 1, 2, 3...
//     with no gaps, so "I have everything node X wrote up to 17" is one number
//     per node. That is the whole version summary.
//   - Ops are taken in only if they continue a node's sequence exactly: seq
//     have+1, have+2... An op that would leave a gap is refused, because later
//     ops may depend on the missing one. An op already held must be identical
//     to the copy held; the same identity with different content is a fork and
//     is refused loudly.
//   - The view (the tasks and notes tables) is the log folded in HLC order:
//     (timestamp, counter, node, seq). That order is the same on every replica
//     that holds the same ops, so the views are the same too. A new op that
//     sorts after everything held is applied on top; one that sorts before
//     something held makes the view be rebuilt, in the same transaction.
//   - Everything an incoming batch does (insert, apply, or rebuild) is one
//     transaction. A batch that cannot be applied cleanly changes nothing.

// Summary says, for each node, the highest sequence number held. Because a
// replica's ops have no gaps, that is also how many it holds.
type Summary map[string]int64

// Errors a sync can run into. Each is wrapped with detail; test with errors.Is.
var (
	// ErrGap: a batch skipped ops (it does not continue a node's sequence).
	ErrGap = errors.New("sync: the ops do not continue this replica's sequence for a node")
	// ErrFork: an op with an identity already held, but different content.
	ErrFork = errors.New("sync: an op with the same identity but different content already exists")
	// ErrBadOp: an op that is malformed or cannot be applied.
	ErrBadOp = errors.New("sync: an incoming op is not valid")
	// ErrIDCollision: two replicas created different tasks with the same ID,
	// which happens when both started separately, before ever syncing.
	ErrIDCollision = errors.New("sync: two replicas created different tasks with the same ID")
	// ErrClockDrift: an incoming timestamp is too far ahead of this clock.
	// It wraps hlc.ErrDrift, so errors.Is works for both.
	ErrClockDrift = errors.New("sync: an incoming timestamp is far ahead of this replica's clock")
)

// maxRemotePayload bounds one incoming op's payload.
const maxRemotePayload = 64 << 10

// Summary returns what this replica holds, by node.
func (s *Store) Summary(ctx context.Context) (Summary, error) {
	return summaryOf(ctx, s.db)
}

func summaryOf(ctx context.Context, x execer) (Summary, error) {
	rows, err := x.QueryContext(ctx, `SELECT node_id, MAX(seq) FROM ops GROUP BY node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := Summary{}
	for rows.Next() {
		var node string
		var max int64
		if err := rows.Scan(&node, &max); err != nil {
			return nil, err
		}
		out[node] = max
	}
	return out, rows.Err()
}

// OpsSince returns the ops this replica holds that a replica with summary have
// does not: for each node, the ops after have[node]. They come in HLC
// fold order (see below), at most limit of them (limit < 1 means 1000), and more
// says whether there are further ones.
func (s *Store) OpsSince(ctx context.Context, have Summary, limit int) (ops []Op, more bool, err error) {
	if limit < 1 {
		limit = replayBatch
	}
	mine, err := summaryOf(ctx, s.db)
	if err != nil {
		return nil, false, err
	}
	nodes := make([]string, 0, len(mine))
	for n := range mine {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	for _, node := range nodes {
		if mine[node] <= have[node] {
			continue
		}
		rows, err := s.db.QueryContext(ctx,
			`SELECT pos, node_id, seq, ts, counter, kind, payload FROM ops WHERE node_id = ? AND seq > ? ORDER BY seq LIMIT ?`,
			node, have[node], limit+1)
		if err != nil {
			return nil, false, err
		}
		got, err := scanOps(rows)
		if err != nil {
			return nil, false, err
		}
		ops = append(ops, got...)
	}
	// Hand them out in fold order (timestamp first), not node by node. An op
	// can refer to a task another replica created; that creation has an
	// earlier stamp (the clock guarantees it), so it comes first, and a
	// receiver that gets the answer in several batches never sees an op before
	// the op it depends on. A replica's own stamps rise with its sequence
	// numbers, so cutting the list at limit still leaves each node's ops as an
	// unbroken run from have[node]+1.
	sort.Slice(ops, func(i, j int) bool { return keyOf(ops[i]).less(keyOf(ops[j])) })
	if len(ops) > limit {
		return ops[:limit], true, nil
	}
	return ops, false, nil
}

func scanOps(rows *sql.Rows) ([]Op, error) {
	defer rows.Close()
	var out []Op
	for rows.Next() {
		var op Op
		var ts int64
		var payload string
		if err := rows.Scan(&op.Pos, &op.NodeID, &op.Seq, &ts, &op.Counter, &op.Kind, &payload); err != nil {
			return nil, err
		}
		op.TS = fromNanos(ts)
		op.Payload = json.RawMessage(payload)
		out = append(out, op)
	}
	return out, rows.Err()
}

// ApplyResult says what ApplyRemote did.
type ApplyResult struct {
	Applied    int  // ops that were new and are now in the log
	Duplicates int  // ops already held, identical, skipped
	Rebuilt    bool // an op sorted before something already applied, so the view was rebuilt
}

// knownKinds are the op kinds this version understands.
var knownKinds = map[string]bool{KindTaskCreated: true, KindTaskUpdated: true, KindNoteAdded: true, KindTaskRemoved: true, KindNoteRemoved: true, KindAnswerRecorded: true}

func validateRemote(op Op) error {
	switch {
	case !nodeIDRe.MatchString(op.NodeID):
		return fmt.Errorf("%w: node ID %q is not valid", ErrBadOp, op.NodeID)
	case op.Seq < 1:
		return fmt.Errorf("%w: %s/%d: sequence numbers start at 1", ErrBadOp, op.NodeID, op.Seq)
	case nanos(op.TS) < 0 || op.TS.IsZero():
		return fmt.Errorf("%w: %s/%d: the timestamp is missing or before 1970", ErrBadOp, op.NodeID, op.Seq)
	case !knownKinds[op.Kind]:
		return fmt.Errorf("%w: %s/%d: unknown kind %q (written by a newer version?)", ErrBadOp, op.NodeID, op.Seq, op.Kind)
	case len(op.Payload) == 0 || len(op.Payload) > maxRemotePayload || !json.Valid(op.Payload):
		return fmt.Errorf("%w: %s/%d: the payload is empty, too large or not JSON", ErrBadOp, op.NodeID, op.Seq)
	}
	return nil
}

// sameOp reports whether two ops have the same content, ignoring the local
// position and JSON whitespace.
func sameOp(a, b Op) bool {
	if a.NodeID != b.NodeID || a.Seq != b.Seq || nanos(a.TS) != nanos(b.TS) || a.Counter != b.Counter || a.Kind != b.Kind {
		return false
	}
	if bytes.Equal(a.Payload, b.Payload) {
		return true
	}
	var ca, cb bytes.Buffer
	if json.Compact(&ca, a.Payload) != nil || json.Compact(&cb, b.Payload) != nil {
		return false
	}
	return bytes.Equal(ca.Bytes(), cb.Bytes())
}

// opKey is the order the view is folded in. It is the same on every replica.
type opKey struct {
	TS      int64
	Counter uint32
	Node    string
	Seq     int64
}

func keyOf(op Op) opKey { return opKey{nanos(op.TS), op.Counter, op.NodeID, op.Seq} }

func (k opKey) less(o opKey) bool {
	switch {
	case k.TS != o.TS:
		return k.TS < o.TS
	case k.Counter != o.Counter:
		return k.Counter < o.Counter
	case k.Node != o.Node:
		return k.Node < o.Node
	}
	return k.Seq < o.Seq
}

// ApplyRemote takes in ops from another replica. It is idempotent: ops already
// held are skipped, so a batch can be delivered twice, or overlap another. It
// is atomic: if any op is refused, nothing from the batch is kept. The ops may
// arrive in any order within the batch.
//
// After a successful call this replica's clock is past every timestamp in the
// batch, so whatever it does next is ordered after what it has just seen.
func (s *Store) ApplyRemote(ctx context.Context, ops []Op) (res ApplyResult, err error) {
	if err := ctx.Err(); err != nil {
		return res, err
	}
	for _, op := range ops {
		if err := validateRemote(op); err != nil {
			return res, err
		}
	}
	sorted := append([]Op(nil), ops...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].NodeID != sorted[j].NodeID {
			return sorted[i].NodeID < sorted[j].NodeID
		}
		return sorted[i].Seq < sorted[j].Seq
	})

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	have, err := summaryOf(ctx, tx)
	if err != nil {
		return res, err
	}
	// Decide, per op: skip (held and identical), take (continues the sequence), or refuse.
	var fresh []Op
	next := map[string]int64{} // the next sequence number expected per node, as the batch is read
	for i, op := range sorted {
		if i > 0 && sorted[i-1].NodeID == op.NodeID && sorted[i-1].Seq == op.Seq {
			if !sameOp(sorted[i-1], op) {
				return res, fmt.Errorf("%w: the batch holds two different ops for %s/%d", ErrFork, op.NodeID, op.Seq)
			}
			continue // the same op twice in one batch
		}
		if op.Seq <= have[op.NodeID] {
			var held Op
			var ts int64
			var payload string
			err = tx.QueryRowContext(ctx, `SELECT node_id, seq, ts, counter, kind, payload FROM ops WHERE node_id = ? AND seq = ?`,
				op.NodeID, op.Seq).Scan(&held.NodeID, &held.Seq, &ts, &held.Counter, &held.Kind, &payload)
			if err != nil {
				return res, err
			}
			held.TS, held.Payload = fromNanos(ts), json.RawMessage(payload)
			if !sameOp(held, op) {
				return res, fmt.Errorf("%w: %s/%d is held with different content", ErrFork, op.NodeID, op.Seq)
			}
			res.Duplicates++
			continue
		}
		want, ok := next[op.NodeID]
		if !ok {
			want = have[op.NodeID] + 1
		}
		if op.Seq != want {
			return res, fmt.Errorf("%w: %s: have %d, received %d next (missing %d..%d)", ErrGap, op.NodeID, have[op.NodeID], op.Seq, want, op.Seq-1)
		}
		next[op.NodeID] = op.Seq + 1
		fresh = append(fresh, op)
	}
	if len(fresh) == 0 {
		return res, tx.Commit()
	}

	// Move this replica's clock past everything it is about to hold. A far-future
	// stamp is refused here, before anything is written.
	var newest hlc.Timestamp
	for _, op := range fresh {
		if st := op.Stamp(); st.After(newest) {
			newest = st
		}
	}
	if _, derr := s.clock.Update(newest); derr != nil {
		return res, fmt.Errorf("%w: %w", ErrClockDrift, derr)
	}

	var maxHeld opKey
	var anyHeld bool
	var wall int64
	var counter uint32
	var node string
	var seq int64
	switch qerr := tx.QueryRowContext(ctx, `SELECT ts, counter, node_id, seq FROM ops ORDER BY ts DESC, counter DESC, node_id DESC, seq DESC LIMIT 1`).
		Scan(&wall, &counter, &node, &seq); {
	case qerr == nil:
		maxHeld, anyHeld = opKey{wall, counter, node, seq}, true
	case !errors.Is(qerr, sql.ErrNoRows):
		return res, qerr
	}

	for i := range fresh {
		var compact bytes.Buffer
		if err = json.Compact(&compact, fresh[i].Payload); err != nil {
			return res, fmt.Errorf("%w: %v", ErrBadOp, err)
		}
		r, ierr := tx.ExecContext(ctx, `INSERT INTO ops (node_id, seq, ts, counter, kind, payload) VALUES (?, ?, ?, ?, ?, ?)`,
			fresh[i].NodeID, fresh[i].Seq, nanos(fresh[i].TS), fresh[i].Counter, fresh[i].Kind, compact.String())
		if ierr != nil {
			return res, fmt.Errorf("sqlstore: append remote op %s/%d: %w", fresh[i].NodeID, fresh[i].Seq, ierr)
		}
		if fresh[i].Pos, err = r.LastInsertId(); err != nil {
			return res, err
		}
		fresh[i].Payload = json.RawMessage(compact.Bytes())
	}
	sort.Slice(fresh, func(i, j int) bool { return keyOf(fresh[i]).less(keyOf(fresh[j])) })
	res.Applied = len(fresh)

	var maxPos int64
	for _, op := range fresh {
		if op.Pos > maxPos {
			maxPos = op.Pos
		}
	}
	if !anyHeld || maxHeld.less(keyOf(fresh[0])) {
		// every new op sorts after everything already applied: apply on top
		for _, op := range fresh {
			if err = applyOp(ctx, tx, op); err != nil {
				return res, wrapApplyError(err)
			}
		}
		prev, aerr := appliedPos(ctx, tx)
		if aerr != nil {
			return res, aerr
		}
		if maxPos < prev {
			maxPos = prev
		}
	} else {
		// something new sorts before something already applied: fold it all again, in order
		res.Rebuilt = true
		if err = resetProjection(ctx, tx); err != nil {
			return res, err
		}
		if _, maxPos, err = replayCanonical(ctx, tx, tx); err != nil {
			return res, wrapApplyError(err)
		}
	}
	if err = setMeta(ctx, tx, "applied_pos", strconv.FormatInt(maxPos, 10)); err != nil {
		return res, err
	}
	if err = tx.Commit(); err != nil {
		return res, err
	}
	return res, nil
}

// wrapApplyError says what kind of refusal an apply failure is.
func wrapApplyError(err error) error {
	var c *CorruptOpError
	if errors.As(err, &c) && c.Kind == KindTaskCreated && strings.Contains(c.Reason, "cannot create task") {
		return fmt.Errorf("%w: %v (both replicas created a task with this ID before they ever synced; start the second replica from an empty database and let it pull)", ErrIDCollision, err)
	}
	return fmt.Errorf("%w: %v", ErrBadOp, err)
}

// ---- folding the log in HLC order ----------------------------------------------------------

// loadOpsCanonical reads up to limit ops that sort after key, in fold order.
func loadOpsCanonical(ctx context.Context, x execer, after opKey, limit int) ([]Op, error) {
	rows, err := x.QueryContext(ctx,
		`SELECT pos, node_id, seq, ts, counter, kind, payload FROM ops
		 WHERE (ts, counter, node_id, seq) > (?, ?, ?, ?)
		 ORDER BY ts, counter, node_id, seq LIMIT ?`, after.TS, after.Counter, after.Node, after.Seq, limit)
	if err != nil {
		return nil, err
	}
	return scanOps(rows)
}

// replayCanonical applies every op in src to dst in fold order and returns how
// many it applied and the highest log position it saw.
func replayCanonical(ctx context.Context, dst, src execer) (n int, maxPos int64, err error) {
	after := opKey{TS: -1}
	for {
		ops, err := loadOpsCanonical(ctx, src, after, replayBatch)
		if err != nil {
			return n, maxPos, err
		}
		if len(ops) == 0 {
			return n, maxPos, nil
		}
		for _, op := range ops {
			if err := applyOp(ctx, dst, op); err != nil {
				return n, maxPos, err
			}
			n++
			if op.Pos > maxPos {
				maxPos = op.Pos
			}
			after = keyOf(op)
		}
	}
}

var _ = task.StatusOpen
