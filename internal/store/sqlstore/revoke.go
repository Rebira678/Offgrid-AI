package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"offgrid/internal/identity"
)

// VoidOp is an op that is in the log but does not count.
type VoidOp struct {
	Node   string
	Seq    int64
	Device string // who signed it ("" if the signer is unknown)
	Reason string
}

// Revocation is what Revoke recorded.
type Revocation struct {
	Device  string
	Cutoffs []Cutoff
}

// voidedOps computes every op the membership does not let count: ops signed by a
// device that is not enrolled, and ops a revoked device signed beyond its
// cutoffs. It is derived from the log alone. Nothing is voided while nobody is
// founded.
func voidedOps(ctx context.Context, x execer, st MembershipState) (map[opID]VoidOp, error) {
	out := map[opID]VoidOp{}
	if len(st.Members) == 0 {
		return out, nil
	}
	rows, err := x.QueryContext(ctx, `SELECT device_id FROM device_keys`)
	if err != nil {
		return nil, err
	}
	var devices []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			rows.Close()
			return nil, err
		}
		devices = append(devices, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, d := range devices {
		m, enrolled := st.Members[d]
		if enrolled && !m.Revoked {
			continue
		}
		r, err := x.QueryContext(ctx, `SELECT node_id, seq FROM op_sigs WHERE device_id = ?`, d)
		if err != nil {
			return nil, err
		}
		for r.Next() {
			var n string
			var s int64
			if err := r.Scan(&n, &s); err != nil {
				r.Close()
				return nil, err
			}
			switch {
			case !enrolled:
				out[opID{n, s}] = VoidOp{Node: n, Seq: s, Device: d, Reason: "signed by a device that is not enrolled"}
			case !m.within(n, s):
				out[opID{n, s}] = VoidOp{Node: n, Seq: s, Device: d, Reason: fmt.Sprintf("signed by %s, revoked by %s, past its cutoff", d, m.RevokedBy)}
			}
		}
		r.Close()
		if err := r.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// currentVoid folds the membership in x and returns its void set.
func currentVoid(ctx context.Context, x execer) (map[opID]VoidOp, MembershipState, error) {
	evs, err := loadMemberEvents(ctx, x)
	if err != nil {
		return nil, MembershipState{}, err
	}
	if len(evs) == 0 {
		return map[opID]VoidOp{}, MembershipState{}, nil
	}
	held, err := loadHeld(ctx, x, evs)
	if err != nil {
		return nil, MembershipState{}, err
	}
	st := foldState(evs, held)
	v, err := voidedOps(ctx, x, st)
	return v, st, err
}

// VoidOps lists the ops that do not count, with the reason, in (replica, sequence) order.
func (s *Store) VoidOps(ctx context.Context) ([]VoidOp, error) {
	v, _, err := currentVoid(ctx, s.db)
	if err != nil {
		return nil, err
	}
	return sortedVoid(v), nil
}

func sortedVoid(v map[opID]VoidOp) []VoidOp {
	out := make([]VoidOp, 0, len(v))
	for _, o := range v {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Node != out[j].Node {
			return out[i].Node < out[j].Node
		}
		return out[i].Seq < out[j].Seq
	})
	return out
}

// reconcileVoid brings the op_void evidence table in line with the log and says
// whether the view must be refolded: it must when an op became void that the view
// may have applied, or stopped being void (a later-arriving earlier founding can
// change who was an admin). Rows are only ever added; a row whose op is no longer
// void stays as the record that it once was, and `store verify` says so.
func reconcileVoid(ctx context.Context, tx *sql.Tx, now int64) (refold bool, err error) {
	v, _, err := currentVoid(ctx, tx)
	if err != nil {
		return false, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT node_id, seq FROM op_void`)
	if err != nil {
		return false, err
	}
	recorded := map[opID]bool{}
	for rows.Next() {
		var k opID
		if err := rows.Scan(&k.Node, &k.Seq); err != nil {
			rows.Close()
			return false, err
		}
		recorded[k] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, o := range sortedVoid(v) {
		if recorded[opID{o.Node, o.Seq}] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO op_void (node_id, seq, device_id, reason, recorded_at) VALUES (?, ?, ?, ?, ?)`,
			o.Node, o.Seq, o.Device, o.Reason, now); err != nil {
			return false, err
		}
		refold = true
	}
	for k := range recorded {
		if _, still := v[k]; !still {
			refold = true
		}
	}
	return refold, nil
}

// refoldView throws the view away and recomputes it, leaving void ops out.
func refoldView(ctx context.Context, tx *sql.Tx) error {
	if err := resetProjection(ctx, tx); err != nil {
		return err
	}
	_, maxPos, err := replayCanonical(ctx, tx, tx)
	if err != nil {
		return wrapApplyError(err)
	}
	return setMeta(ctx, tx, "applied_pos", strconv.FormatInt(maxPos, 10))
}

// cutoffsFor is what this replica holds of a device's chains: for every replica
// the device signed ops on, the highest of them and its chain hash.
func (s *Store) cutoffsFor(ctx context.Context, x execer, device string) ([]Cutoff, error) {
	rows, err := x.QueryContext(ctx, `SELECT s.node_id, MAX(s.seq), c.hash FROM op_sigs s
		JOIN op_chain c ON c.node_id = s.node_id AND c.seq = (SELECT MAX(seq) FROM op_sigs WHERE device_id = s.device_id AND node_id = s.node_id)
		WHERE s.device_id = ? GROUP BY s.node_id ORDER BY s.node_id`, device)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Cutoff{}
	for rows.Next() {
		var c Cutoff
		if err := rows.Scan(&c.Node, &c.Seq, &c.Hash); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Revoke revokes a device, cutting its chains off at what this replica holds.
// Work the device did after that and has not yet synced here is lost to the
// group (an admin can re-enter it by hand). This device must be an admin.
func (s *Store) Revoke(ctx context.Context, device string) (Revocation, error) {
	var rev Revocation
	err := s.write(ctx, func(tx *sql.Tx) error {
		cut, err := s.cutoffsFor(ctx, tx, device)
		if err != nil {
			return err
		}
		rev = Revocation{Device: device, Cutoffs: cut}
		return s.revokeIn(ctx, tx, device, cut)
	})
	return rev, err
}

// RevokeAt is Revoke with the cutoffs given, for an admin who holds more than this replica does.
func (s *Store) RevokeAt(ctx context.Context, device string, cutoffs []Cutoff) error {
	return s.write(ctx, func(tx *sql.Tx) error { return s.revokeIn(ctx, tx, device, cutoffs) })
}

func (s *Store) revokeIn(ctx context.Context, tx *sql.Tx, device string, cutoffs []Cutoff) error {
	rep, err := s.requireAdmin(ctx, tx)
	if err != nil {
		return err
	}
	p := memberRevokePayload{Device: device, Cutoffs: cutoffs}
	if p.Cutoffs == nil {
		p.Cutoffs = []Cutoff{}
	}
	if err := checkMemberRevoke(p); err != nil {
		return fmt.Errorf("sqlstore: member.revoke: %w", err)
	}
	var target *Member
	for i := range rep.Members {
		if rep.Members[i].Device == device {
			target = &rep.Members[i]
		}
	}
	switch {
	case target == nil:
		return fmt.Errorf("%w: %s is not a member", ErrNotAuthorized, device)
	case target.Revoked:
		return fmt.Errorf("%w: %s", ErrAlreadyRevoked, device)
	case target.Device == rep.Founder():
		return fmt.Errorf("%w: %s is the founder and cannot be revoked; a stolen founder key means founding a new group", ErrNotAuthorized, device)
	}
	if _, err := identity.DeviceID(target.PublicKey); err != nil {
		return err
	}
	if err := s.record(ctx, tx, KindMemberRevoke, p); err != nil {
		return err
	}
	// The rules have the last word (for instance, who outranks whom): if they refuse what was just written, undo it.
	after, err := s.membersOf(ctx, tx)
	if err != nil {
		return err
	}
	if len(after.Problems) > len(rep.Problems) {
		for _, pr := range after.Problems {
			known := false
			for _, old := range rep.Problems {
				known = known || (old.Node == pr.Node && old.Seq == pr.Seq)
			}
			if !known {
				return fmt.Errorf("%w: %s", ErrNotAuthorized, pr.Reason)
			}
		}
	}
	refold, err := reconcileVoid(ctx, tx, nanos(s.clock.Now().Time()))
	if err != nil {
		return err
	}
	if refold {
		return refoldView(ctx, tx)
	}
	return nil
}

var _ = errors.New
