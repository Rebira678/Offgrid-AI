package sqlstore

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"offgrid/internal/identity"
	"offgrid/internal/task"
)

type trioEnv struct {
	a, b, c       *Store
	ida, idb, idc *identity.Identity
	clkA, clkB    *clock
}

// trio is an admin (laptop), and two members (pi, tab), all holding the membership.
func trio(t *testing.T) *trioEnv {
	t.Helper()
	ctx := context.Background()
	e := &trioEnv{ida: newKey(t), idb: newKey(t), idc: newKey(t)}
	e.a, e.clkA = member(t, "laptop", e.ida)
	e.b, e.clkB = member(t, "pi", e.idb)
	e.c, _ = member(t, "tab", e.idc)
	if err := e.a.Found(ctx, "founder"); err != nil {
		t.Fatal(err)
	}
	e.clkA.tick(time.Second)
	if err := e.a.AddMember(ctx, e.idb.PublicKey(), RoleMember, "pi"); err != nil {
		t.Fatal(err)
	}
	e.clkA.tick(time.Second)
	if err := e.a.AddMember(ctx, e.idc.PublicKey(), RoleMember, "tab"); err != nil {
		t.Fatal(err)
	}
	pull(t, e.b, e.a, 50)
	pull(t, e.c, e.a, 50)
	e.clkB.tick(time.Minute)
	return e
}

func viewTitles(t *testing.T, s *Store) string {
	t.Helper()
	ts, err := s.List(context.Background(), task.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, tk := range ts {
		out = append(out, tk.Title)
	}
	return strings.Join(sortedStrings(out), ",")
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func voidOf(t *testing.T, s *Store) []VoidOp {
	t.Helper()
	v, err := s.VoidOps(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestARevokedDevicesLaterOpsAreVoidEverywhereAndEarlierOnesStay(t *testing.T) {
	ctx := context.Background()
	e := trio(t)
	create(t, e.b, "before-1")
	create(t, e.b, "before-2")
	pull(t, e.a, e.b, 50)
	e.clkA.tick(time.Minute)
	rev, err := e.a.Revoke(ctx, e.idb.ID())
	if err != nil {
		t.Fatal(err)
	}
	if len(rev.Cutoffs) != 1 || rev.Cutoffs[0].Node != "pi" || rev.Cutoffs[0].Seq < 2 || len(rev.Cutoffs[0].Hash) != 64 {
		t.Fatalf("the cutoff must name the last op of the device's chain the admin holds: %+v", rev)
	}
	// The Pi has not heard. It keeps working and syncs later.
	create(t, e.b, "after-1")
	create(t, e.b, "after-2")
	pull(t, e.a, e.b, 50)
	if got := viewTitles(t, e.a); got != "before-1,before-2" {
		t.Fatalf("the admin's view = %q, want only the work before the cutoff", got)
	}
	v := voidOf(t, e.a)
	if len(v) != 2 || v[0].Node != "pi" || v[0].Device != e.idb.ID() || !strings.Contains(v[0].Reason, "revoked") {
		t.Fatalf("void ops: %+v", v)
	}
	// Another replica, which never saw the Pi directly, agrees.
	pull(t, e.c, e.a, 50)
	if got := viewTitles(t, e.c); got != "before-1,before-2" || hashOf(t, e.a) != hashOf(t, e.c) {
		t.Fatalf("tab sees %q", got)
	}
	// The void ops are evidence: they are still in the log and still chain-verified.
	mustVerify(t, e.a)
	rep, _ := e.a.Verify(ctx)
	if len(rep.Void) != 2 {
		t.Fatalf("verify must list the void ops: %+v", rep.Void)
	}
}

func TestAReplicaThatAlreadyAppliedTheWorkRefoldsWhenTheRevocationArrives(t *testing.T) {
	ctx := context.Background()
	e := trio(t)
	create(t, e.b, "kept")
	pull(t, e.a, e.b, 50)
	pull(t, e.c, e.a, 50)
	e.clkA.tick(time.Minute)
	rev, err := e.a.Revoke(ctx, e.idb.ID())
	if err != nil {
		t.Fatal(err)
	}
	_ = rev
	create(t, e.b, "late")
	pull(t, e.c, e.b, 50) // tab takes the late work straight from the Pi before it hears of the revocation
	if got := viewTitles(t, e.c); got != "kept,late" {
		t.Fatalf("before the revocation arrives: %q", got)
	}
	res := pull(t, e.c, e.a, 50)
	if !res.Rebuilt {
		t.Fatal("the view must be refolded when a revocation voids work already applied")
	}
	if got := viewTitles(t, e.c); got != "kept" {
		t.Fatalf("after: %q", got)
	}
	if len(voidOf(t, e.c)) != 1 {
		t.Fatalf("%+v", voidOf(t, e.c))
	}
	mustVerify(t, e.c)
}

func TestOnlyAnAdminCanRevokeAndNotTheFounderOrAStranger(t *testing.T) {
	ctx := context.Background()
	e := trio(t)
	if _, err := e.b.Revoke(ctx, e.idc.ID()); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("a member revoking: %v", err)
	}
	if _, err := e.a.Revoke(ctx, newKey(t).ID()); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("a device that is not enrolled: %v", err)
	}
	if _, err := e.a.Revoke(ctx, e.ida.ID()); !errors.Is(err, ErrNotAuthorized) || !strings.Contains(err.Error(), "founder") {
		t.Fatalf("the founder: %v (a stolen founder key is handled by re-founding, not revocation)", err)
	}
	if _, err := e.a.Revoke(ctx, e.idb.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.Revoke(ctx, e.idb.ID()); !errors.Is(err, ErrAlreadyRevoked) {
		t.Fatalf("revoking twice: %v", err)
	}
	rep := mustMembers(t, e.a)
	for _, m := range rep.Members {
		if m.Device == e.idb.ID() && (!m.Revoked || m.RevokedBy != e.ida.ID()) {
			t.Fatalf("%+v", m)
		}
	}
	if rep.Admins() != 1 {
		t.Fatalf("admins = %d", rep.Admins())
	}
}

func TestALaterAdminCannotRevokeAnEarlierOneAndNothingIsWrittenWhenItTries(t *testing.T) {
	ctx := context.Background()
	e := trio(t)
	ide, idd := newKey(t), newKey(t)
	if err := e.a.AddMember(ctx, ide.PublicKey(), RoleAdmin, "elder"); err != nil {
		t.Fatal(err)
	}
	e.clkA.tick(time.Second)
	if err := e.a.AddMember(ctx, idd.PublicKey(), RoleAdmin, "junior"); err != nil {
		t.Fatal(err)
	}
	d, _ := member(t, "dell", idd)
	pull(t, d, e.a, 50)
	before, _ := d.Summary(ctx)
	if _, err := d.Revoke(ctx, ide.ID()); !errors.Is(err, ErrNotAuthorized) || !strings.Contains(err.Error(), "enrolled after") {
		t.Fatalf("err = %v", err)
	}
	if after, _ := d.Summary(ctx); !sameSummary(before, after) {
		t.Fatalf("a refused revocation left an op behind: %v -> %v", before, after)
	}
	// The elder can remove the junior.
	pull(t, e.a, d, 50)
	if _, err := e.a.Revoke(ctx, idd.ID()); err != nil {
		t.Fatal(err)
	}
}

func TestARevokedKeyNeverComesBack(t *testing.T) {
	ctx := context.Background()
	e := trio(t)
	if _, err := e.a.Revoke(ctx, e.idb.ID()); err != nil {
		t.Fatal(err)
	}
	err := e.a.AddMember(ctx, e.idb.PublicKey(), RoleMember, "again")
	if !errors.Is(err, ErrAlreadyMember) {
		t.Fatalf("re-adding a revoked key: %v (it must enroll again with a NEW key)", err)
	}
	if err := e.a.AddMember(ctx, newKey(t).PublicKey(), RoleMember, "new key"); err != nil {
		t.Fatal(err)
	}
}

func TestARevokedAdminsLaterAddsAreVoidButEarlierOnesStand(t *testing.T) {
	ctx := context.Background()
	e := trio(t)
	// make the Pi's friend an admin: the founder invites a second admin first.
	idd := newKey(t)
	d, _ := member(t, "dell", idd)
	if err := e.a.AddMember(ctx, idd.PublicKey(), RoleAdmin, "dell"); err != nil {
		t.Fatal(err)
	}
	pull(t, d, e.a, 50)
	e.clkA.tick(time.Minute)
	idx := newKey(t)
	if err := d.AddMember(ctx, idx.PublicKey(), RoleMember, "enrolled-before-revocation"); err != nil {
		t.Fatal(err)
	}
	pull(t, e.a, d, 50)
	if _, err := e.a.Revoke(ctx, idd.ID()); err != nil {
		t.Fatal(err)
	}
	// Dell, not having heard, enrolls one more.
	idy := newKey(t)
	if err := d.AddMember(ctx, idy.PublicKey(), RoleMember, "enrolled-after-revocation"); err != nil {
		t.Fatal(err)
	}
	pull(t, e.a, d, 50)
	rep := mustMembers(t, e.a)
	if roleOf(rep, idx) != RoleMember {
		t.Fatal("a device enrolled before the cutoff must stay enrolled")
	}
	if roleOf(rep, idy) != "" {
		t.Fatalf("a device enrolled by a revoked admin after the cutoff must not be a member: %+v", rep)
	}
	if len(rep.Problems) != 0 {
		t.Fatalf("a void add is not a rule violation, it is void: %+v", rep.Problems)
	}
	mustVerify(t, e.a)
}

func TestAnOpFromADeviceThatIsNotEnrolledIsRefusedAndOneThatArrivedEarlyIsVoid(t *testing.T) {
	ctx := context.Background()
	e := trio(t)
	idm := newKey(t)
	m, _ := member(t, "mallory", idm)
	create(t, m, "hostile")
	ops, _, err := m.OpsSince(ctx, Summary{}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.ApplyRemote(ctx, ops); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("a signed op from a stranger: %v", err)
	}
	// A replica that has not yet heard of the membership takes it, and when the membership arrives the op is void.
	r, _ := member(t, "fresh", newKey(t))
	if _, err := r.ApplyRemote(ctx, ops); err != nil {
		t.Fatal(err)
	}
	pull(t, r, e.a, 50)
	if got := viewTitles(t, r); got != "" {
		t.Fatalf("the stranger's work must not be in the view once the membership is known: %q", got)
	}
	if v := voidOf(t, r); len(v) != 1 || !strings.Contains(v[0].Reason, "not enrolled") {
		t.Fatalf("%+v", v)
	}
}

func TestVoidingIsAppendOnlyEvidence(t *testing.T) {
	ctx := context.Background()
	e := trio(t)
	e.a.Revoke(ctx, e.idb.ID())
	create(t, e.b, "late")
	pull(t, e.a, e.b, 50)
	var n int
	if err := e.a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM op_void`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("op_void rows = %d (%v): the first time an op is seen void it is recorded", n, err)
	}
	if _, err := e.a.db.ExecContext(ctx, `UPDATE op_void SET reason = 'x'`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("update: %v", err)
	}
	if _, err := e.a.db.ExecContext(ctx, `DELETE FROM op_void`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("delete: %v", err)
	}
	// Syncing again records nothing twice.
	pull(t, e.a, e.b, 50)
	e.a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM op_void`).Scan(&n)
	if n != 1 {
		t.Fatalf("rows = %d", n)
	}
}

func TestACutoffHashThatDoesNotMatchTheChainVoidsEverythingFromThatNode(t *testing.T) {
	ctx := context.Background()
	e := trio(t)
	create(t, e.b, "one")
	create(t, e.b, "two")
	pull(t, e.a, e.b, 50)
	good, err := e.a.cutoffsFor(ctx, e.a.db, e.idb.ID())
	if err != nil || len(good) != 1 {
		t.Fatal(err, good)
	}
	bad := []Cutoff{{Node: good[0].Node, Seq: good[0].Seq, Hash: strings.Repeat("0", 64)}}
	if err := e.a.RevokeAt(ctx, e.idb.ID(), bad); err != nil {
		t.Fatal(err)
	}
	// The admin saw a different chain than the one it signed: the device equivocated. Nothing of it can be trusted.
	if got := viewTitles(t, e.a); got != "" {
		t.Fatalf("view = %q", got)
	}
}

func TestARevocationWithoutAKeyOrWithBadCutoffsIsRefused(t *testing.T) {
	ctx := context.Background()
	e := trio(t)
	cases := map[string][]Cutoff{
		"empty node":       {{Node: "", Seq: 1, Hash: strings.Repeat("a", 64)}},
		"negative":         {{Node: "pi", Seq: -1}},
		"zero with a hash": {{Node: "pi", Seq: 0, Hash: strings.Repeat("a", 64)}},
		"short hash":       {{Node: "pi", Seq: 1, Hash: "abc"}},
		"duplicate node":   {{Node: "pi", Seq: 1, Hash: strings.Repeat("a", 64)}, {Node: "pi", Seq: 2, Hash: strings.Repeat("b", 64)}},
	}
	for name, c := range cases {
		if err := e.a.RevokeAt(ctx, e.idb.ID(), c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	s, _ := freshStore(t)
	if _, err := s.Revoke(ctx, e.idb.ID()); err == nil {
		t.Fatal("a store with no device key cannot revoke")
	}
}

// ---- the pure rule ---------------------------------------------------------------------------

func TestAuthorizeIsAPureTable(t *testing.T) {
	hash := strings.Repeat("a", 64)
	st := MembershipState{Members: map[string]Member{
		"A": {Device: "A", Role: RoleAdmin},
		"M": {Device: "M", Role: RoleMember},
		"R": {Device: "R", Role: RoleMember, Revoked: true, Cutoffs: []Cutoff{{Node: "r", Seq: 5, Hash: hash}}},
	}}
	cases := []struct {
		name string
		op   OpRef
		want error
		rev  bool
	}{
		{"no signature is not attributable", OpRef{Node: "x", Seq: 1, Kind: KindTaskCreated}, nil, false},
		{"a member's task op", OpRef{Node: "m", Seq: 9, Kind: KindTaskCreated, Signer: "M"}, nil, false},
		{"a stranger", OpRef{Node: "z", Seq: 1, Kind: KindTaskCreated, Signer: "Z"}, ErrNotAuthorized, false},
		{"revoked, at the cutoff", OpRef{Node: "r", Seq: 5, Kind: KindTaskCreated, Signer: "R"}, nil, false},
		{"revoked, one past", OpRef{Node: "r", Seq: 6, Kind: KindTaskCreated, Signer: "R"}, ErrRevoked, true},
		{"revoked, another node", OpRef{Node: "other", Seq: 1, Kind: KindTaskCreated, Signer: "R"}, ErrRevoked, true},
		{"a member enrolling", OpRef{Node: "m", Seq: 9, Kind: KindMemberAdd, Signer: "M"}, ErrNotAuthorized, false},
		{"a member revoking", OpRef{Node: "m", Seq: 9, Kind: KindMemberRevoke, Signer: "M"}, ErrNotAuthorized, false},
		{"an admin enrolling", OpRef{Node: "a", Seq: 9, Kind: KindMemberAdd, Signer: "A"}, nil, false},
		{"an admin revoking", OpRef{Node: "a", Seq: 9, Kind: KindMemberRevoke, Signer: "A"}, nil, false},
	}
	for _, c := range cases {
		err := Authorize(st, c.op)
		if !errors.Is(err, c.want) && !(c.want == nil && err == nil) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
		if c.want != nil && errors.Is(err, ErrRevoked) != c.rev {
			t.Errorf("%s: revoked = %v, want %v", c.name, errors.Is(err, ErrRevoked), c.rev)
		}
	}
	if err := Authorize(MembershipState{}, OpRef{Node: "x", Seq: 1, Signer: "anyone"}); err != nil {
		t.Fatalf("with no membership at all everything is allowed (legacy): %v", err)
	}
}

// ---- the property: any order of the same ops gives the same answer ------------------------------

// ev builds a member.add event; rv a member.revoke. Times are small integers so the order is explicit.
func evAdd(ts int64, node string, seq int64, signer, device string, role Role) memberEvent {
	return memberEvent{Kind: KindMemberAdd, Node: node, Seq: seq, TS: time.Unix(ts, 0).UTC(), Signer: signer,
		Add: memberAddPayload{Device: device, PublicKey: pubFor(device), Role: role, Label: device}}
}

func evRevoke(ts int64, node string, seq int64, signer, device string, cut ...Cutoff) memberEvent {
	return memberEvent{Kind: KindMemberRevoke, Node: node, Seq: seq, TS: time.Unix(ts, 0).UTC(), Signer: signer,
		Revoke: memberRevokePayload{Device: device, Cutoffs: cut}}
}

// pubFor makes a distinct fake key per device name; the fold does not verify keys, checkMemberAdd does, so tests of the pure
// rule use real ones.
var pubs = map[string]*identity.Identity{}

func idFor(name string) *identity.Identity {
	if id, ok := pubs[name]; ok {
		return id
	}
	id, _ := identity.Generate()
	pubs[name] = id
	return id
}

func pubFor(device string) string {
	for _, id := range pubs {
		if id.ID() == device {
			return id.PublicKey()
		}
	}
	return idFor(device).PublicKey()
}

func devOf(name string) string { return idFor(name).ID() }

func stateKey(st MembershipState) string {
	var b strings.Builder
	var ds []string
	for d := range st.Members {
		ds = append(ds, d)
	}
	for _, d := range sortedStrings(ds) {
		m := st.Members[d]
		fmt.Fprintf(&b, "%s/%s/rev=%v/%v/by=%s;", d, m.Role, m.Revoked, m.Cutoffs, m.RevokedBy)
	}
	var bs []string
	for k, why := range st.Bad {
		bs = append(bs, fmt.Sprintf("bad %s/%d %s", k.Node, k.Seq, why))
	}
	for k, why := range st.Void {
		bs = append(bs, fmt.Sprintf("void %s/%d %s", k.Node, k.Seq, why))
	}
	for _, s := range sortedStrings(bs) {
		b.WriteString(s + ";")
	}
	return b.String()
}

func TestTwoAdminsRevokingEachOtherGiveOneAnswerInEveryOrder(t *testing.T) {
	// F founds, adds X and Y as admins. Concurrently X revokes Y (x2) and Y revokes X (y2). Neither has seen the other's.
	f, x, y := devOf("F"), devOf("X"), devOf("Y")
	evs := []memberEvent{
		evAdd(1, "f", 1, f, f, RoleAdmin),
		evAdd(2, "f", 2, f, x, RoleAdmin),
		evAdd(3, "f", 3, f, y, RoleAdmin),
		evRevoke(10, "x", 1, x, y),
		evRevoke(10, "y", 1, y, x),
	}
	want := stateKey(foldState(evs, nil))
	rnd := rand.New(rand.NewSource(1))
	for i := 0; i < 200; i++ {
		p := append([]memberEvent(nil), evs...)
		rnd.Shuffle(len(p), func(i, j int) { p[i], p[j] = p[j], p[i] })
		if got := stateKey(foldState(p, nil)); got != want {
			t.Fatalf("order changed the answer:\n%s\n%s", want, got)
		}
	}
	st := foldState(evs, nil)
	// Exactly one of them is revoked: the one enrolled LATER. Neither a backdated clock nor arrival order can change it,
	// and the two revocations cannot cancel each other (a compromised admin could otherwise undo its own removal).
	if !st.Members[y].Revoked || st.Members[x].Revoked {
		t.Fatalf("exactly one admin must be revoked: %s", want)
	}
	if st.Members[f].Revoked {
		t.Fatal("the founder is never revoked")
	}
}

func TestACompromisedLaterAdminCannotCancelItsOwnRemovalByBackdatingARevoke(t *testing.T) {
	f, x, y := devOf("F"), devOf("X"), devOf("Y")
	evs := []memberEvent{
		evAdd(100, "f", 1, f, f, RoleAdmin),
		evAdd(200, "f", 2, f, x, RoleAdmin),
		evAdd(300, "f", 3, f, y, RoleAdmin),
		evRevoke(1000, "x", 1, x, y),
		evRevoke(5, "y", 1, y, x), // Y stamps its counter-revocation long before everything else
	}
	st := foldState(evs, nil)
	if !st.Members[y].Revoked || st.Members[x].Revoked {
		t.Fatalf("Y must be removed whatever it stamps: %s", stateKey(st))
	}
	_, bad := st.Bad[opID{"y", 1}]
	_, void := st.Void[opID{"y", 1}]
	if !bad && !void {
		t.Fatalf("the counter-revocation must not count: %s", stateKey(st))
	}
	// Nobody can revoke someone who outranks them, and an admin cannot revoke itself.
	evs = []memberEvent{evAdd(1, "f", 1, f, f, RoleAdmin), evAdd(2, "f", 2, f, x, RoleAdmin), evRevoke(3, "x", 1, x, x)}
	if st := foldState(evs, nil); st.Members[x].Revoked {
		t.Fatal("an admin revoked itself")
	}
}

func TestARevokedDeviceStaysRevokedWithTheFirstCutoff(t *testing.T) {
	f, a, m := devOf("F"), devOf("A"), devOf("M")
	h := strings.Repeat("d", 64)
	evs := []memberEvent{
		evAdd(1, "f", 1, f, f, RoleAdmin), evAdd(2, "f", 2, f, a, RoleAdmin), evAdd(3, "f", 3, f, m, RoleMember),
		evRevoke(10, "f", 4, f, m, Cutoff{Node: "m", Seq: 3, Hash: h}),
		evRevoke(11, "a", 1, a, m, Cutoff{Node: "m", Seq: 9, Hash: h}), // a later, wider cutoff must not widen the first
	}
	st := foldState(evs, nil)
	if got := st.Members[m].Cutoffs; len(got) != 1 || got[0].Seq != 3 {
		t.Fatalf("cutoffs = %+v: the first revocation stands", got)
	}
	if _, bad := st.Bad[opID{"a", 1}]; !bad {
		t.Fatal("a second revocation of the same device must be refused")
	}
}

func TestPermutationsOfARandomLogGiveTheSameMembershipAndTheSameVoidSet(t *testing.T) {
	rnd := rand.New(rand.NewSource(42))
	names := []string{"F", "A", "B", "C", "D"}
	for round := 0; round < 150; round++ {
		var evs []memberEvent
		seq := map[string]int64{}
		next := func(n string) int64 { seq[n]++; return seq[n] }
		ts := int64(1)
		tick := func() int64 { ts += int64(rnd.Intn(3)); return ts }
		evs = append(evs, evAdd(tick(), "F", next("F"), devOf("F"), devOf("F"), RoleAdmin))
		for i := 0; i < 14; i++ {
			signer := names[rnd.Intn(len(names))]
			target := names[rnd.Intn(len(names))]
			switch rnd.Intn(3) {
			case 0:
				role := RoleMember
				if rnd.Intn(2) == 0 {
					role = RoleAdmin
				}
				evs = append(evs, evAdd(tick(), strings.ToLower(signer), next(signer), devOf(signer), devOf(target), role))
			case 1:
				var cut []Cutoff
				if rnd.Intn(2) == 0 {
					cut = []Cutoff{{Node: strings.ToLower(target), Seq: int64(rnd.Intn(4))}}
					if cut[0].Seq > 0 {
						cut[0].Hash = strings.Repeat("c", 64)
					}
				}
				evs = append(evs, evRevoke(tick(), strings.ToLower(signer), next(signer), devOf(signer), devOf(target), cut...))
			default:
				// an ordinary op by someone: not part of the fold, but Authorize must agree on it too
			}
		}
		want := foldState(evs, nil)
		wantKey := stateKey(want)
		refs := []OpRef{}
		for _, n := range names {
			for s := int64(1); s <= 5; s++ {
				refs = append(refs, OpRef{Node: strings.ToLower(n), Seq: s, Kind: KindTaskCreated, Signer: devOf(n)})
			}
		}
		wantAuth := authKeys(want, refs)
		for k := 0; k < 25; k++ {
			p := append([]memberEvent(nil), evs...)
			rnd.Shuffle(len(p), func(i, j int) { p[i], p[j] = p[j], p[i] })
			got := foldState(p, nil)
			if gk := stateKey(got); gk != wantKey {
				t.Fatalf("round %d: order changed the membership:\n%s\n%s", round, wantKey, gk)
			}
			if !reflect.DeepEqual(authKeys(got, refs), wantAuth) {
				t.Fatalf("round %d: order changed which ops are valid", round)
			}
		}
	}
}

func authKeys(st MembershipState, refs []OpRef) []string {
	var out []string
	for _, r := range refs {
		out = append(out, fmt.Sprint(Authorize(st, r)))
	}
	return out
}
