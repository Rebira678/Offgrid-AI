package sqlstore

// A tamper-evident log.
//
// Every op is chained to the one before it FROM THE SAME REPLICA: the hash of
// an op covers its content and the hash of that replica's previous op. Change
// anything in op 7 of laptop (a word in a title, a timestamp) and its hash
// changes, and so does the hash of op 8, 9 and every later laptop op, because
// each contains the one before. Verify recomputes the chains and names the
// first op that no longer matches what was stored when it was written.
//
// The chain is per replica, not one global chain, on purpose. Ops reach a
// replica in different orders (sync), so "the op before this one in my log" is
// different on every replica, but "the op before this one from the same
// replica" is a fact about the op itself. Every replica therefore computes the
// same hash for the same op, and sync can compare them.
//
// What this does and does not protect is spelled out in docs/day-27.md. In one
// line: it detects an edit that does not also rewrite the rest of the chain.
// Someone who can rewrite the whole file can recompute the whole chain; that is
// caught by comparing against a head saved somewhere else (Heads, VerifyAnchors)
// or against another replica (sync compares hashes).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
)

// GenesisHash is the "previous hash" of a replica's first op.
const GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// ErrTampered means the log no longer matches the hashes stored with it.
var ErrTampered = errors.New("the op log does not match its hash chain")

// ErrChain is returned by ApplyRemote when an incoming op's hash differs from
// the hash this replica computes for it: the two replicas disagree about that
// replica's EARLIER ops, even if the op itself looks the same.
var ErrChain = errors.New("sync: the op's hash chain does not match this replica's copy of that replica's history")

// TamperError says which op no longer matches, and how.
type TamperError struct {
	Node   string
	Seq    int64
	Reason string
}

func (e *TamperError) Error() string {
	return fmt.Sprintf("op %s/%d: %s", e.Node, e.Seq, e.Reason)
}
func (e *TamperError) Unwrap() error { return ErrTampered }

// OpHash is the hash of an op given the hash of the same replica's previous op
// (GenesisHash for its first). Every field is written length-prefixed, so no
// two different ops can produce the same bytes by shifting text between fields,
// and the payload is compacted so JSON whitespace never matters.
func OpHash(op Op, prev string) string {
	h := sha256.New()
	put := func(b []byte) {
		var n [binary.MaxVarintLen64]byte
		h.Write(n[:binary.PutUvarint(n[:], uint64(len(b)))])
		h.Write(b)
	}
	put([]byte("offgrid-op-v1"))
	put([]byte(op.NodeID))
	put([]byte(strconv.FormatInt(op.Seq, 10)))
	put([]byte(strconv.FormatInt(nanos(op.TS), 10)))
	put([]byte(strconv.FormatUint(uint64(op.Counter), 10)))
	put([]byte(op.Kind))
	put([]byte(prev))
	var c bytes.Buffer
	if err := json.Compact(&c, op.Payload); err != nil {
		c.Reset()
		c.Write(op.Payload) // not JSON: hash the bytes as they are; Verify will still notice a change
	}
	put(c.Bytes())
	return hex.EncodeToString(h.Sum(nil))
}

// chainPrev is the stored hash of node's op seq-1, or GenesisHash for seq 1.
func chainPrev(ctx context.Context, x execer, node string, seq int64) (string, error) {
	if seq <= 1 {
		return GenesisHash, nil
	}
	var h string
	err := x.QueryRowContext(ctx, `SELECT hash FROM op_chain WHERE node_id = ? AND seq = ?`, node, seq-1).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("sqlstore: no chain entry for %s/%d", node, seq-1)
	}
	return h, err
}

// chainAppend stores the chain entry of an op that has just been written and
// returns its hash.
func chainAppend(ctx context.Context, x execer, op Op) (string, error) {
	prev, err := chainPrev(ctx, x, op.NodeID, op.Seq)
	if err != nil {
		return "", err
	}
	hash := OpHash(op, prev)
	if _, err := x.ExecContext(ctx, `INSERT INTO op_chain (node_id, seq, prev, hash) VALUES (?, ?, ?, ?)`, op.NodeID, op.Seq, prev, hash); err != nil {
		return "", fmt.Errorf("sqlstore: chain %s/%d: %w", op.NodeID, op.Seq, err)
	}
	return hash, nil
}

// backfillChain chains every op that has no chain entry yet. A database made
// before the chain existed has all its ops in that state: they are chained as
// they stand today, so they are protected from now on, not from when they were
// written. How many were chained that way is remembered in the meta table so
// Verify can say so.
func (s *Store) backfillChain(ctx context.Context) (err error) {
	var missing int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ops o LEFT JOIN op_chain c ON c.node_id = o.node_id AND c.seq = o.seq WHERE c.hash IS NULL`).Scan(&missing); err != nil {
		return err
	}
	if missing == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	ops, err := loadMissingChain(ctx, tx)
	if err != nil {
		return err
	}
	for _, op := range ops {
		if _, err = chainAppend(ctx, tx, op); err != nil {
			return err
		}
	}
	prior := 0
	if v, ok, gerr := getMeta(ctx, tx, metaRetroChained); gerr != nil {
		return gerr
	} else if ok {
		prior, _ = strconv.Atoi(v)
	}
	if err = setMeta(ctx, tx, metaRetroChained, strconv.Itoa(prior+len(ops))); err != nil {
		return err
	}
	return tx.Commit()
}

// metaRetroChained counts the ops chained after the fact (see backfillChain).
const metaRetroChained = "chain_retro_ops"

func loadMissingChain(ctx context.Context, x execer) ([]Op, error) {
	rows, err := x.QueryContext(ctx, `SELECT o.pos, o.node_id, o.seq, o.ts, o.counter, o.kind, o.payload
		FROM ops o LEFT JOIN op_chain c ON c.node_id = o.node_id AND c.seq = o.seq
		WHERE c.hash IS NULL ORDER BY o.node_id, o.seq`)
	if err != nil {
		return nil, err
	}
	return scanOps(rows)
}

// Head is the newest chain entry of one replica. Written down somewhere the
// log's owner cannot rewrite (a note, a post, another machine), it lets
// VerifyAnchors detect a log that was rewritten end to end.
type Head struct {
	Node string
	Seq  int64
	Hash string
}

func (h Head) String() string { return fmt.Sprintf("%s %d %s", h.Node, h.Seq, h.Hash) }

// Heads lists the newest stored chain entry of every replica, by name. It
// reads what is stored; Verify is what proves the stored hashes are right.
func (s *Store) Heads(ctx context.Context) ([]Head, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.node_id, c.seq, c.hash FROM op_chain c
		WHERE c.seq = (SELECT MAX(seq) FROM op_chain WHERE node_id = c.node_id) ORDER BY c.node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Head
	for rows.Next() {
		var h Head
		if err := rows.Scan(&h.Node, &h.Seq, &h.Hash); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ChainReport says what VerifyChain checked.
type ChainReport struct {
	Ops   int    // ops whose hash was recomputed and matched
	Heads []Head // the newest entry of every replica
	// Retro is how many ops were chained when this database was upgraded to
	// the chain, rather than when they were written. An edit made to them
	// before that upgrade cannot be seen.
	Retro int
}

// VerifyChain recomputes every replica's chain from the ops themselves and
// compares it with what was stored when each op was written. It returns a
// *TamperError (matching ErrTampered) naming the first op that differs.
func (s *Store) VerifyChain(ctx context.Context) (ChainReport, error) {
	var rep ChainReport
	rows, err := s.db.QueryContext(ctx, `SELECT o.pos, o.node_id, o.seq, o.ts, o.counter, o.kind, o.payload, c.prev, c.hash
		FROM ops o LEFT JOIN op_chain c ON c.node_id = o.node_id AND c.seq = o.seq
		ORDER BY o.node_id, o.seq`)
	if err != nil {
		return rep, err
	}
	defer rows.Close()
	var node string
	var last int64
	var lastHash string
	for rows.Next() {
		var op Op
		var ts int64
		var payload string
		var prev, hash sql.NullString
		if err := rows.Scan(&op.Pos, &op.NodeID, &op.Seq, &ts, &op.Counter, &op.Kind, &payload, &prev, &hash); err != nil {
			return rep, err
		}
		op.TS, op.Payload = fromNanos(ts), []byte(payload)
		if op.NodeID != node {
			node, last, lastHash = op.NodeID, 0, GenesisHash
		}
		switch {
		case op.Seq != last+1:
			return rep, &TamperError{op.NodeID, last + 1, "is missing from the log"}
		case !hash.Valid:
			return rep, &TamperError{op.NodeID, op.Seq, "has no entry in the hash chain"}
		case prev.String != lastHash:
			return rep, &TamperError{op.NodeID, op.Seq, "is not linked to the op before it: the chain was edited"}
		}
		if got := OpHash(op, lastHash); got != hash.String {
			return rep, &TamperError{op.NodeID, op.Seq, "was changed after it was recorded (its content no longer matches its hash)"}
		}
		last, lastHash = op.Seq, hash.String
		rep.Ops++
	}
	if err := rows.Err(); err != nil {
		return rep, err
	}
	rows.Close()
	// A chain entry with no op: an op was deleted.
	var orphanNode string
	var orphanSeq int64
	err = s.db.QueryRowContext(ctx, `SELECT c.node_id, c.seq FROM op_chain c LEFT JOIN ops o ON o.node_id = c.node_id AND o.seq = c.seq
		WHERE o.pos IS NULL ORDER BY c.node_id, c.seq LIMIT 1`).Scan(&orphanNode, &orphanSeq)
	switch {
	case err == nil:
		return rep, &TamperError{orphanNode, orphanSeq, "has a hash but no op: the op was removed from the log"}
	case !errors.Is(err, sql.ErrNoRows):
		return rep, err
	}
	if rep.Heads, err = s.Heads(ctx); err != nil {
		return rep, err
	}
	if v, ok, err := getMeta(ctx, s.db, metaRetroChained); err != nil {
		return rep, err
	} else if ok {
		rep.Retro, _ = strconv.Atoi(v)
	}
	return rep, nil
}

// VerifyAnchors checks heads saved earlier against the log: for each, the op
// must still be there and carry the same hash. It does not recompute anything
// (run VerifyChain first). A log rewritten from some point on, with all its
// hashes recomputed to look consistent, fails here for every anchor at or
// after that point; a log that has lost its newest ops fails because the
// anchored op is gone.
func (s *Store) VerifyAnchors(ctx context.Context, anchors []Head) error {
	sorted := append([]Head(nil), anchors...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Node != sorted[j].Node {
			return sorted[i].Node < sorted[j].Node
		}
		return sorted[i].Seq < sorted[j].Seq
	})
	for _, a := range sorted {
		var h string
		err := s.db.QueryRowContext(ctx, `SELECT hash FROM op_chain WHERE node_id = ? AND seq = ?`, a.Node, a.Seq).Scan(&h)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return &TamperError{a.Node, a.Seq, "was saved as an anchor but this log no longer has it: ops were removed"}
		case err != nil:
			return err
		case h != a.Hash:
			return &TamperError{a.Node, a.Seq, "is not what it was when the anchor was saved: the history before it was rewritten"}
		}
	}
	return nil
}
