package sqlstore

// Membership: who may take part, as a log of signed ops.
//
// A `member.add` op, signed by an admin, enrolls a device: its public key, its
// role (fixed at enrollment) and a human label. There is no membership table
// to edit: the membership is a PURE FUNCTION of the member.add ops in the log
// (foldMembership), so every replica that holds the same ops reaches the same
// answer, in any order they arrived. Day 39 adds revocation to the same fold.
//
// The rules, in the order the fold applies them (HLC order, ties broken by
// replica and sequence, so it is the same on every replica):
//
//   - the FIRST add is a founding: an admin adds itself, signed by itself. This
//     is the trust root (ADR 005): whoever founds is trusted, so the founder's
//     device ID is shown by `store members` for a human to check, and an invite
//     carries it so a new device pins it;
//   - every later add must be signed by a device that is an admin at that point;
//   - a device is added once. Roles are fixed at enrollment: there is no
//     promotion, to make a second admin enroll one.
//
// What this does NOT do yet: it does not stop an unenrolled device from
// writing ordinary ops, or from syncing. Deciding who may do what is
// Authorize (Day 39), and who may connect is mutual TLS (Day 40).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"offgrid/internal/identity"
)

// KindMemberAdd enrolls a device.
const KindMemberAdd = "member.add"

// Role is what a member may do. Fixed at enrollment.
type Role string

const (
	RoleAdmin  Role = "admin"  // may enroll devices and make invites
	RoleMember Role = "member" // an ordinary device
)

var (
	// ErrNotAuthorized: the op or request needs a role its signer does not have.
	ErrNotAuthorized = errors.New("sync: not authorized by the membership")
	// ErrAlreadyMember: the device is enrolled already.
	ErrAlreadyMember = errors.New("sqlstore: the device is already a member")
)

type memberAddPayload struct {
	Device    string `json:"device"`
	PublicKey string `json:"public_key"`
	Role      Role   `json:"role"`
	Label     string `json:"label"`
}

const maxLabel = 64

func checkLabel(label string) error {
	if strings.TrimSpace(label) == "" {
		return errors.New("the label is empty")
	}
	if len(label) > maxLabel {
		return fmt.Errorf("the label is longer than %d bytes", maxLabel)
	}
	for _, r := range label {
		if unicode.IsControl(r) {
			return errors.New("the label has a control character")
		}
	}
	return nil
}

func checkMemberAdd(p memberAddPayload) error {
	dev, err := identity.DeviceID(p.PublicKey)
	if err != nil {
		return fmt.Errorf("the public key is not usable: %w", err)
	}
	if dev != p.Device {
		return errors.New("the device ID is not the hash of the public key")
	}
	if p.Role != RoleAdmin && p.Role != RoleMember {
		return fmt.Errorf("role %q is not admin or member", p.Role)
	}
	return checkLabel(p.Label)
}

// Member is an enrolled device.
type Member struct {
	Device    string
	PublicKey string
	Role      Role
	Label     string
	AddedBy   string // the device that signed the member.add
	Node      string // the op that enrolled it: replica ...
	Seq       int64  // ... and sequence
	At        time.Time
}

// MemberProblem is a member.add in the log that the rules do not allow.
type MemberProblem struct {
	Node   string
	Seq    int64
	Reason string
}

// MembershipReport is the fold of every member.add in the log.
type MembershipReport struct {
	Members  []Member
	Problems []MemberProblem
}

// Admins counts the admins.
func (r MembershipReport) Admins() int {
	n := 0
	for _, m := range r.Members {
		if m.Role == RoleAdmin {
			n++
		}
	}
	return n
}

// Founder is the device that founded, or "".
func (r MembershipReport) Founder() string {
	if len(r.Members) == 0 {
		return ""
	}
	return r.Members[0].Device
}

type opID struct {
	Node string
	Seq  int64
}

// memberEvent is a member.add as the fold sees it.
type memberEvent struct {
	Node    string
	Seq     int64
	TS      time.Time
	Counter int64
	Signer  string // the device that signed the op; "" if unsigned
	Add     memberAddPayload
}

// foldMembership is the whole membership rule as a pure function. It returns
// the enrolled devices and, for every event the rules refuse, why. The input
// order does not matter: events are sorted first.
func foldMembership(events []memberEvent) (map[string]Member, map[opID]string) {
	evs := append([]memberEvent(nil), events...)
	sort.Slice(evs, func(i, j int) bool {
		a, b := evs[i], evs[j]
		switch {
		case !a.TS.Equal(b.TS):
			return a.TS.Before(b.TS)
		case a.Counter != b.Counter:
			return a.Counter < b.Counter
		case a.Node != b.Node:
			return a.Node < b.Node
		}
		return a.Seq < b.Seq
	})
	members := map[string]Member{}
	bad := map[opID]string{}
	for _, e := range evs {
		k := opID{e.Node, e.Seq}
		if err := checkMemberAdd(e.Add); err != nil {
			bad[k] = err.Error()
			continue
		}
		if len(members) == 0 {
			if e.Signer != e.Add.Device || e.Add.Role != RoleAdmin {
				bad[k] = "the first add must be a founding: an admin adding itself, signed by itself"
				continue
			}
		} else {
			if s, ok := members[e.Signer]; !ok || s.Role != RoleAdmin {
				bad[k] = fmt.Sprintf("signed by %q, which is not an admin", e.Signer)
				continue
			}
			if _, dup := members[e.Add.Device]; dup {
				bad[k] = "the device is already a member (roles are fixed at enrollment)"
				continue
			}
		}
		members[e.Add.Device] = Member{Device: e.Add.Device, PublicKey: e.Add.PublicKey, Role: e.Add.Role, Label: e.Add.Label,
			AddedBy: e.Signer, Node: e.Node, Seq: e.Seq, At: e.TS}
	}
	return members, bad
}

// loadMemberEvents reads every member.add in the log with the device that signed it.
func loadMemberEvents(ctx context.Context, x execer) ([]memberEvent, error) {
	rows, err := x.QueryContext(ctx, `SELECT o.node_id, o.seq, o.ts, o.counter, o.payload, COALESCE(s.device_id, '')
		FROM ops o LEFT JOIN op_sigs s ON s.node_id = o.node_id AND s.seq = o.seq
		WHERE o.kind = ?`, KindMemberAdd)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []memberEvent
	for rows.Next() {
		var e memberEvent
		var wall int64
		var payload string
		if err := rows.Scan(&e.Node, &e.Seq, &wall, &e.Counter, &payload, &e.Signer); err != nil {
			return nil, err
		}
		e.TS = fromNanos(wall)
		// A payload that does not decode stays zero, which the fold refuses.
		_ = decodeStrict(json.RawMessage(payload), &e.Add)
		out = append(out, e)
	}
	return out, rows.Err()
}

func reportOf(members map[string]Member, bad map[opID]string) MembershipReport {
	var rep MembershipReport
	for _, m := range members {
		rep.Members = append(rep.Members, m)
	}
	sort.Slice(rep.Members, func(i, j int) bool {
		a, b := rep.Members[i], rep.Members[j]
		switch {
		case !a.At.Equal(b.At):
			return a.At.Before(b.At)
		case a.Node != b.Node:
			return a.Node < b.Node
		}
		return a.Seq < b.Seq
	})
	for k, why := range bad {
		rep.Problems = append(rep.Problems, MemberProblem{k.Node, k.Seq, why})
	}
	sort.Slice(rep.Problems, func(i, j int) bool {
		a, b := rep.Problems[i], rep.Problems[j]
		if a.Node != b.Node {
			return a.Node < b.Node
		}
		return a.Seq < b.Seq
	})
	return rep
}

// Members folds the log into the current membership.
func (s *Store) Members(ctx context.Context) (MembershipReport, error) {
	return s.membersOf(ctx, s.db)
}

func (s *Store) membersOf(ctx context.Context, x execer) (MembershipReport, error) {
	evs, err := loadMemberEvents(ctx, x)
	if err != nil {
		return MembershipReport{}, err
	}
	m, bad := foldMembership(evs)
	return reportOf(m, bad), nil
}

// checkMembershipFresh refuses ops about to be stored whose member.add the
// rules do not allow, naming the first one. The fold runs over what is stored
// PLUS the new ops, so a batch that carries an admin's add and that admin's
// later adds together is judged as a whole.
func (s *Store) checkMembershipFresh(ctx context.Context, tx execer, fresh []Op) error {
	var mine []memberEvent
	for _, op := range fresh {
		if op.Kind != KindMemberAdd {
			continue
		}
		e := memberEvent{Node: op.NodeID, Seq: op.Seq, TS: op.TS, Counter: int64(op.Counter)}
		if op.Sig != nil {
			e.Signer = op.Sig.Device
		}
		_ = decodeStrict(op.Payload, &e.Add)
		mine = append(mine, e)
	}
	if len(mine) == 0 {
		return nil
	}
	stored, err := loadMemberEvents(ctx, tx)
	if err != nil {
		return err
	}
	_, bad := foldMembership(append(stored, mine...))
	for _, e := range mine { // fresh is in (node, seq) order, so the first named is the lowest
		if why, ok := bad[opID{e.Node, e.Seq}]; ok {
			return fmt.Errorf("%w: op %s/%d: %s", ErrNotAuthorized, e.Node, e.Seq, why)
		}
	}
	return nil
}

// requireSigner is the local half of the same rule, before anything is written.
func (s *Store) requireAdmin(ctx context.Context, tx execer) (MembershipReport, error) {
	if s.signer == nil {
		return MembershipReport{}, errors.New("sqlstore: enrolling devices needs a device key (offgrid identity init)")
	}
	rep, err := s.membersOf(ctx, tx)
	if err != nil {
		return rep, err
	}
	for _, m := range rep.Members {
		if m.Device == s.signer.ID() && m.Role == RoleAdmin {
			return rep, nil
		}
	}
	return rep, fmt.Errorf("%w: this device (%s) is not an admin of the membership", ErrNotAuthorized, s.signer.ID())
}

func (s *Store) recordAdd(ctx context.Context, tx *sql.Tx, pub string, role Role, label string) error {
	dev, err := identity.DeviceID(pub)
	if err != nil {
		return fmt.Errorf("sqlstore: %w", err)
	}
	p := memberAddPayload{Device: dev, PublicKey: pub, Role: role, Label: label}
	if err := checkMemberAdd(p); err != nil {
		return fmt.Errorf("sqlstore: member.add: %w", err)
	}
	return s.record(ctx, tx, KindMemberAdd, p)
}

// Found makes this device the first admin: the trust root. Only once, and only
// while nobody is enrolled.
func (s *Store) Found(ctx context.Context, label string) error {
	if s.signer == nil {
		return errors.New("sqlstore: founding a membership needs a device key (offgrid identity init)")
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		rep, err := s.membersOf(ctx, tx)
		if err != nil {
			return err
		}
		if len(rep.Members) > 0 {
			return fmt.Errorf("%w: a membership already exists (founded by %s); a second founding would be a second root of trust", ErrNotAuthorized, rep.Founder())
		}
		return s.recordAdd(ctx, tx, s.signer.PublicKey(), RoleAdmin, label)
	})
}

// AddMember enrolls a device whose public key is already known. This device must be an admin.
func (s *Store) AddMember(ctx context.Context, publicKey string, role Role, label string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		rep, err := s.requireAdmin(ctx, tx)
		if err != nil {
			return err
		}
		return s.addChecked(ctx, tx, rep, publicKey, role, label)
	})
}

func (s *Store) addChecked(ctx context.Context, tx *sql.Tx, rep MembershipReport, publicKey string, role Role, label string) error {
	dev, err := identity.DeviceID(publicKey)
	if err != nil {
		return fmt.Errorf("sqlstore: %w", err)
	}
	for _, m := range rep.Members {
		if m.Device == dev {
			return fmt.Errorf("%w: %s is enrolled as %s", ErrAlreadyMember, dev, m.Role)
		}
	}
	return s.recordAdd(ctx, tx, publicKey, role, label)
}
