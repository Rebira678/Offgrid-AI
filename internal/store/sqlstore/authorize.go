package sqlstore

// Revocation and Authorize (Day 39).
//
// The membership state is: enrolled devices (each added by an admin) plus a
// grow-only set of revoked devices. A `member.revoke` op, signed by an admin,
// names the device and a CUTOFF per replica: the last (sequence, chain hash) of
// the device's chain the admin held. Ops the device signed beyond its cutoff
// are VOID: still in the log, still chained, kept as evidence, but excluded from
// the view. Ops at or before the cutoff stay valid.
//
// Why a cutoff and not a time: a timestamp can be backdated, a (sequence, hash)
// cannot, because each op commits to everything before it (ADR 004).
//
// Everything here is a pure function of the SET of ops, so every replica that
// holds the same ops reaches the same answer, in any order. The one subtlety is
// that voiding an op can change who was an admin, which can change which
// revocations were valid. foldState settles that by repeating the fold, only
// ever adding to the void set, until nothing new is voided; the void set only
// grows within a run, so it ends, and the input is sorted first, so the answer
// does not depend on arrival order (TestPermutationsOf...).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// KindMemberRevoke revokes a device.
const KindMemberRevoke = "member.revoke"

var (
	// ErrRevoked: the op's signer was revoked and the op is past its cutoff.
	// It wraps ErrNotAuthorized. Such an op is stored and voided, not refused.
	ErrRevoked = fmt.Errorf("%w: the signer was revoked and this op is past its cutoff", ErrNotAuthorized)
	// ErrAlreadyRevoked: revoked keys never come back, and are revoked once.
	ErrAlreadyRevoked = errors.New("sqlstore: the device is already revoked")
)

// Cutoff is the last op of one replica's chain, signed by the revoked device, that still counts.
// Seq 0 with no hash means: none of this replica's ops count.
type Cutoff struct {
	Node string `json:"node"`
	Seq  int64  `json:"seq"`
	Hash string `json:"hash,omitempty"`
}

type memberRevokePayload struct {
	Device  string   `json:"device"`
	Cutoffs []Cutoff `json:"cutoffs"`
}

func isHex64(s string) bool {
	return len(s) == 64 && strings.Trim(strings.ToLower(s), "0123456789abcdef") == ""
}

func checkMemberRevoke(p memberRevokePayload) error {
	if !strings.HasPrefix(p.Device, "dev_") {
		return errors.New("the device ID is not a device ID")
	}
	seen := map[string]bool{}
	for _, c := range p.Cutoffs {
		switch {
		case c.Node == "":
			return errors.New("a cutoff names no replica")
		case seen[c.Node]:
			return fmt.Errorf("two cutoffs for replica %q", c.Node)
		case c.Seq < 0:
			return errors.New("a cutoff has a negative sequence")
		case c.Seq == 0 && c.Hash != "":
			return errors.New("a cutoff at sequence 0 has no hash")
		case c.Seq > 0 && !isHex64(c.Hash):
			return errors.New("a cutoff's hash is not a SHA-256 in hex")
		}
		seen[c.Node] = true
	}
	return nil
}

// memberEvent is a member.add or member.revoke as the fold sees it.
type memberEvent struct {
	Kind    string // KindMemberAdd (also when empty) or KindMemberRevoke
	Node    string
	Seq     int64
	TS      time.Time
	Counter int64
	Signer  string // the device that signed the op; "" if unsigned
	Add     memberAddPayload
	Revoke  memberRevokePayload
}

func decodeEvent(e *memberEvent, payload json.RawMessage) {
	if e.Kind == KindMemberRevoke {
		_ = decodeStrict(payload, &e.Revoke)
		return
	}
	_ = decodeStrict(payload, &e.Add)
}

// MembershipState is the whole result of the fold.
type MembershipState struct {
	Members map[string]Member // every enrolled device, revoked ones included (marked)
	Bad     map[opID]string   // member ops the rules refuse, and why
	Void    map[opID]string   // member ops voided because their signer was revoked past its cutoff
}

// OpRef is what Authorize needs to know about an op.
type OpRef struct {
	Node   string
	Seq    int64
	Kind   string
	Signer string // "" for an unsigned op
}

func (m Member) within(node string, seq int64) bool {
	for _, c := range m.Cutoffs {
		if c.Node == node {
			return seq <= c.Seq
		}
	}
	return false
}

// Authorize says whether the membership lets this op count. It is pure: it
// reads only st and o.
//
//   - no membership at all (nothing founded): everything is allowed, as before;
//   - an unsigned op is not attributable to anyone, so it is not judged here;
//   - a signer that is not enrolled: ErrNotAuthorized;
//   - a revoked signer past its cutoff (or on a replica the cutoffs do not name): ErrRevoked;
//   - member.add and member.revoke need an admin.
func Authorize(st MembershipState, o OpRef) error {
	if len(st.Members) == 0 || o.Signer == "" {
		return nil
	}
	m, ok := st.Members[o.Signer]
	if !ok {
		return fmt.Errorf("%w: %s is not enrolled", ErrNotAuthorized, o.Signer)
	}
	if m.Revoked && !m.within(o.Node, o.Seq) {
		return ErrRevoked
	}
	if (o.Kind == KindMemberAdd || o.Kind == KindMemberRevoke) && m.Role != RoleAdmin {
		return fmt.Errorf("%w: %s is not an admin", ErrNotAuthorized, o.Signer)
	}
	return nil
}

func sortEvents(evs []memberEvent) {
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
}

// foldMembership is the Day 38 view of the fold: who is enrolled, and what was refused.
func foldMembership(events []memberEvent) (map[string]Member, map[opID]string) {
	st := foldState(events, nil)
	return st.Members, st.Bad
}

// foldState is the whole membership rule as a pure function. held, if not nil,
// says what chain hash this replica holds for (node, seq): a cutoff whose hash
// disagrees with it names a chain the revoked device did not write here, so it
// is read as "none of this replica's ops count".
func foldState(events []memberEvent, held func(node string, seq int64) (string, bool)) MembershipState {
	evs := append([]memberEvent(nil), events...)
	sortEvents(evs)
	void := map[opID]string{}
	for {
		st := foldOnce(evs, void, held)
		grew := false
		for _, m := range st.Members {
			if !m.Revoked {
				continue
			}
			for _, e := range evs {
				k := opID{e.Node, e.Seq}
				if _, done := void[k]; done || e.Signer != m.Device || m.within(e.Node, e.Seq) {
					continue
				}
				void[k] = fmt.Sprintf("signed by %s, revoked by %s, past its cutoff", m.Device, m.RevokedBy)
				grew = true
			}
		}
		if !grew {
			st.Void = void
			return st
		}
	}
}

func foldOnce(evs []memberEvent, void map[opID]string, held func(string, int64) (string, bool)) MembershipState {
	st := MembershipState{Members: map[string]Member{}, Bad: map[opID]string{}}
	founder := ""
	for _, e := range evs {
		k := opID{e.Node, e.Seq}
		if _, v := void[k]; v {
			continue
		}
		if e.Kind == KindMemberRevoke {
			if why := revokeProblem(st, e, founder); why != "" {
				st.Bad[k] = why
				continue
			}
			m := st.Members[e.Revoke.Device]
			m.Revoked, m.RevokedBy = true, e.Signer
			m.Cutoffs = effectiveCutoffs(e.Revoke.Cutoffs, held)
			st.Members[m.Device] = m
			continue
		}
		if err := checkMemberAdd(e.Add); err != nil {
			st.Bad[k] = err.Error()
			continue
		}
		if len(st.Members) == 0 {
			if e.Signer != e.Add.Device || e.Add.Role != RoleAdmin {
				st.Bad[k] = "the first add must be a founding: an admin adding itself, signed by itself"
				continue
			}
			founder = e.Add.Device
		} else {
			if s, ok := st.Members[e.Signer]; !ok || s.Role != RoleAdmin {
				st.Bad[k] = fmt.Sprintf("signed by %q, which is not an admin", e.Signer)
				continue
			}
			if _, dup := st.Members[e.Add.Device]; dup {
				st.Bad[k] = "the device is already a member (roles are fixed at enrollment, and a revoked key never comes back)"
				continue
			}
		}
		st.Members[e.Add.Device] = Member{Device: e.Add.Device, PublicKey: e.Add.PublicKey, Role: e.Add.Role, Label: e.Add.Label,
			AddedBy: e.Signer, Node: e.Node, Seq: e.Seq, At: e.TS, rank: len(st.Members)}
	}
	return st
}

func revokeProblem(st MembershipState, e memberEvent, founder string) string {
	if err := checkMemberRevoke(e.Revoke); err != nil {
		return err.Error()
	}
	if s, ok := st.Members[e.Signer]; !ok || s.Role != RoleAdmin {
		return fmt.Sprintf("signed by %q, which is not an admin", e.Signer)
	}
	t, ok := st.Members[e.Revoke.Device]
	if ok && t.Role == RoleAdmin && st.Members[e.Signer].rank >= t.rank {
		// Without this, two admins could each revoke the other and each revocation would void the other: a compromised
		// admin could cancel its own removal. An admin may only remove an admin who was enrolled after it, an order
		// the group's own log fixes and a backdated clock cannot change.
		return "an admin can only revoke an admin who was enrolled after it"
	}
	switch {
	case !ok:
		return "the device is not a member"
	case t.Device == founder:
		return "the founder cannot be revoked (a stolen founder key is handled by founding a new group)"
	case t.Revoked:
		return "the device is already revoked"
	}
	return ""
}

func effectiveCutoffs(in []Cutoff, held func(string, int64) (string, bool)) []Cutoff {
	out := make([]Cutoff, 0, len(in))
	for _, c := range in {
		if c.Seq > 0 && held != nil {
			if h, ok := held(c.Node, c.Seq); ok && !strings.EqualFold(h, c.Hash) {
				c = Cutoff{Node: c.Node}
			}
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out
}

// loadHeld reads the chain hashes this replica holds for every cutoff in events.
func loadHeld(ctx context.Context, x execer, evs []memberEvent) (func(string, int64) (string, bool), error) {
	have := map[opID]string{}
	for _, e := range evs {
		if e.Kind != KindMemberRevoke {
			continue
		}
		for _, c := range e.Revoke.Cutoffs {
			if c.Seq <= 0 {
				continue
			}
			var h string
			switch err := x.QueryRowContext(ctx, `SELECT hash FROM op_chain WHERE node_id = ? AND seq = ?`, c.Node, c.Seq).Scan(&h); {
			case err == nil:
				have[opID{c.Node, c.Seq}] = h
			case errors.Is(err, sql.ErrNoRows):
			default:
				return nil, err
			}
		}
	}
	return func(n string, s int64) (string, bool) { h, ok := have[opID{n, s}]; return h, ok }, nil
}
