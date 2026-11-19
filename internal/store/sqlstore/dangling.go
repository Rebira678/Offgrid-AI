package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// Work built on void work.
//
// A revocation makes the revoked device's later ops void: kept in the log as
// evidence, left out of the view. An honest device that has not heard of the
// revocation may meanwhile build on that work: note a task the revoked device made,
// change it, suggest a change, delete it. Those ops are valid and honest. They are
// not void (the device that wrote them is in good standing); they simply refer to
// something that, once the revocation is known, never existed.
//
// That must not read as a corrupt log. A log that refuses its own honest ops stops
// every replica that holds them from syncing, and a stranger could cause it just by
// being revoked at the wrong moment. So when a reference cannot be found, applyOp asks
// one more question: is the thing it refers to void? If so the op is inert: it stays
// in the log and in the chain, and has no effect on the view. If the thing is
// missing for any other reason, it is still corruption.

type voidCtxKey struct{}

type voidScope struct {
	void map[opID]VoidOp
	src  execer // where the log is: a rebuild applies into a scratch database that has no log
}

// withVoid hands a fold's void set, and the place the log lives, to applyOp, so it
// does not recompute the set per op and can read the log when applying elsewhere.
func withVoid(ctx context.Context, v map[opID]VoidOp, src execer) context.Context {
	return context.WithValue(ctx, voidCtxKey{}, voidScope{v, src})
}

// logOf is where to read the log from: the fold's source when there is one, else x itself.
func logOf(ctx context.Context, x execer) execer {
	if sc, ok := ctx.Value(voidCtxKey{}).(voidScope); ok {
		return sc.src
	}
	return x
}

func voidSetFor(ctx context.Context, x execer) (map[opID]VoidOp, error) {
	if sc, ok := ctx.Value(voidCtxKey{}).(voidScope); ok {
		return sc.void, nil
	}
	v, _, err := currentVoid(ctx, x)
	return v, err
}

// taskIsVoid reports whether the task has no place in the view because every op that
// created it is void. A task with no creating op in the log at all is not void: it is missing.
func taskIsVoid(ctx context.Context, x execer, id string) (bool, error) {
	if id == "" {
		return false, nil
	}
	rows, err := logOf(ctx, x).QueryContext(ctx, `SELECT node_id, seq FROM ops WHERE kind = ? AND json_extract(payload, '$.id') = ?`, KindTaskCreated, id)
	if err != nil {
		return false, err
	}
	var made []opID
	for rows.Next() {
		var k opID
		if err := rows.Scan(&k.Node, &k.Seq); err != nil {
			rows.Close()
			return false, err
		}
		made = append(made, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	if len(made) == 0 {
		return false, nil
	}
	v, err := voidSetFor(ctx, x)
	if err != nil {
		return false, err
	}
	for _, k := range made {
		if _, isVoid := v[k]; !isVoid {
			return false, nil
		}
	}
	return true, nil
}

// opIsVoid reports whether the op (node, seq) is void.
func opIsVoid(ctx context.Context, x execer, node string, seq int64) (bool, error) {
	v, err := voidSetFor(ctx, x)
	if err != nil {
		return false, err
	}
	_, isVoid := v[opID{node, seq}]
	return isVoid, nil
}

// suggestionIsVoid reports whether the suggestion (node, seq) is absent from the view
// because its op is void or because the task it concerns is void.
func suggestionIsVoid(ctx context.Context, x execer, node string, seq int64) (bool, error) {
	if node == "" || seq <= 0 {
		return false, nil
	}
	if isVoid, err := opIsVoid(ctx, x, node, seq); err != nil || isVoid {
		return isVoid, err
	}
	var kind, payload string
	err := logOf(ctx, x).QueryRowContext(ctx, `SELECT kind, payload FROM ops WHERE node_id = ? AND seq = ?`, node, seq).Scan(&kind, &payload)
	if errors.Is(err, sql.ErrNoRows) || err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if kind != KindSuggestionMade {
		return false, nil
	}
	var p struct {
		Task string `json:"task"`
	}
	if json.Unmarshal([]byte(payload), &p) != nil || p.Task == "" {
		return false, nil
	}
	return taskIsVoid(ctx, x, p.Task)
}

// dangling decides what a reference to something missing means: the op is inert when
// what it refers to is void, and corrupt otherwise.
func dangling(ctx context.Context, x execer, corrupt error, isVoid func() (bool, error)) error {
	v, err := isVoid()
	if err != nil {
		return err
	}
	if v {
		return nil
	}
	return corrupt
}

// refuseIfRevoked stops a device that knows it was revoked from writing more. Its op
// would be void everywhere, including here; taking it into its own view would leave
// that view different from every other replica's. The refusal wraps ErrRevoked.
// A device that has not yet heard of its revocation writes as before, and its work
// past the cutoff goes void when the revocation reaches it.
func (s *Store) refuseIfRevoked(ctx context.Context, tx *sql.Tx, kind string, seq int64) error {
	if s.signer == nil {
		return nil
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM ops WHERE kind = ?`, KindMemberRevoke).Scan(&n); err != nil || n == 0 {
		return err
	}
	_, st, err := currentVoid(ctx, tx)
	if err != nil {
		return err
	}
	err = Authorize(st, OpRef{Node: s.node, Seq: seq, Signer: s.signer.ID(), Kind: kind})
	if errors.Is(err, ErrRevoked) {
		return err
	}
	return nil
}
