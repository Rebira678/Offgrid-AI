package sqlstore

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"strings"
	"testing"
	"time"

	"offgrid/internal/identity"
)

// member opens a replica for node, signing with id, on a clock the test controls.
func member(t *testing.T, node string, id *identity.Identity) (*Store, *clock) {
	t.Helper()
	clk := &clock{t: epoch}
	return open(t, newDB(t), Options{NodeID: node, Now: clk.now, Signer: id}), clk
}

func mustMembers(t *testing.T, s *Store) MembershipReport {
	t.Helper()
	rep, err := s.Members(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func roleOf(rep MembershipReport, id *identity.Identity) Role {
	for _, m := range rep.Members {
		if m.Device == id.ID() {
			return m.Role
		}
	}
	return ""
}

func TestFoundingMakesTheFirstAdmin(t *testing.T) {
	ctx := context.Background()
	ida := newKey(t)
	a, _ := member(t, "laptop", ida)
	if err := a.Found(ctx, "Reza's laptop"); err != nil {
		t.Fatal(err)
	}
	rep := mustMembers(t, a)
	if len(rep.Members) != 1 || rep.Members[0].Device != ida.ID() || rep.Members[0].Role != RoleAdmin || rep.Members[0].Label != "Reza's laptop" || len(rep.Problems) != 0 {
		t.Fatalf("%+v", rep)
	}
	if rep.Members[0].PublicKey != ida.PublicKey() {
		t.Fatal("the stored key must be the device's public key")
	}
	// Only once: a second founding would make a second root of trust.
	if err := a.Found(ctx, "again"); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("a second founding: err = %v, want ErrNotAuthorized", err)
	}
	// And it needs a key.
	s, _ := freshStore(t)
	if err := s.Found(ctx, "x"); err == nil {
		t.Fatal("a store with no device key cannot found a membership")
	}
}

func TestAnAdminAddsAMemberAndAMemberCannotAddAnyone(t *testing.T) {
	ctx := context.Background()
	ida, idb, idc := newKey(t), newKey(t), newKey(t)
	a, _ := member(t, "laptop", ida)
	if err := a.Found(ctx, "founder"); err != nil {
		t.Fatal(err)
	}
	if err := a.AddMember(ctx, idb.PublicKey(), RoleMember, "the Pi"); err != nil {
		t.Fatal(err)
	}
	b, _ := member(t, "pi", idb)
	pull(t, b, a, 10)
	rb := mustMembers(t, b)
	if len(rb.Members) != 2 || roleOf(rb, idb) != RoleMember {
		t.Fatalf("%+v", rb)
	}
	// The Pi is a member, not an admin: its attempt to add a device is refused, and writes nothing.
	before := len(opsOf(t, b))
	if err := b.AddMember(ctx, idc.PublicKey(), RoleMember, "intruder"); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("a member adding a device: err = %v, want ErrNotAuthorized", err)
	}
	if len(opsOf(t, b)) != before {
		t.Fatal("a refused add left an op behind")
	}
}

func TestAMemberAddFromANonAdminIsRefusedWhenItArrives(t *testing.T) {
	ctx := context.Background()
	ida, idb, idm := newKey(t), newKey(t), newKey(t)
	a, _ := member(t, "laptop", ida)
	a.Found(ctx, "founder")
	a.AddMember(ctx, idb.PublicKey(), RoleMember, "pi")
	// Mallory is a member (so her ops are signed by an enrolled key) but writes a member.add on her own replica,
	// with the check bypassed by building the op the way a hostile peer would.
	mal, _ := member(t, "mallory", idm)
	forged := forgeAdd(t, mal, idm, newKey(t).PublicKey(), RoleAdmin)
	b, _ := member(t, "pi", idb)
	pull(t, b, a, 10)
	_, err := b.ApplyRemote(ctx, forged)
	if !errors.Is(err, ErrNotAuthorized) || !strings.Contains(err.Error(), "mallory/1") {
		t.Fatalf("err = %v, want ErrNotAuthorized naming mallory/1", err)
	}
	if rep := mustMembers(t, b); len(rep.Members) != 2 {
		t.Fatalf("the refused op changed the membership: %+v", rep)
	}
}

// forgeAdd builds a correctly signed member.add the way a hostile peer would, skipping the local check.
func forgeAdd(t *testing.T, s *Store, signer *identity.Identity, targetKey string, role Role) []Op {
	t.Helper()
	dev, _ := identity.DeviceID(targetKey)
	raw, _ := json.Marshal(memberAddPayload{Device: dev, PublicKey: targetKey, Role: role, Label: "forged"})
	op := Op{NodeID: s.node, Seq: 1, TS: epoch, Kind: KindMemberAdd, Payload: raw}
	op.Hash = OpHash(op, GenesisHash)
	sig, err := signer.Sign(sigMessage(op.Hash))
	if err != nil {
		t.Fatal(err)
	}
	op.Sig = &OpSig{Alg: identity.AlgECDSAP256, Device: signer.ID(), Sig: sig, Key: signer.PublicKey()}
	return []Op{op}
}

func TestAnUnenrolledDeviceCannotFoundOverAnExistingMembership(t *testing.T) {
	ctx := context.Background()
	ida, idm := newKey(t), newKey(t)
	a, _ := member(t, "laptop", ida)
	a.Found(ctx, "founder")
	mal, _ := member(t, "mallory", idm)
	forged := forgeAdd(t, mal, idm, idm.PublicKey(), RoleAdmin) // she adds herself as an admin: a self-signed "founding"
	if _, err := a.ApplyRemote(ctx, forged); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("err = %v, want ErrNotAuthorized: only the FIRST add may be self-signed", err)
	}
}

func TestAnUnsignedMemberAddIsRefused(t *testing.T) {
	op := Op{NodeID: "x", Seq: 1, TS: epoch, Kind: KindMemberAdd, Payload: json.RawMessage(`{}`)}
	if err := validateRemote(op); !errors.Is(err, ErrBadOp) {
		t.Fatalf("err = %v", err)
	}
}

func TestMemberAddPayloadIsChecked(t *testing.T) {
	ida, idb := newKey(t), newKey(t)
	good := memberAddPayload{Device: idb.ID(), PublicKey: idb.PublicKey(), Role: RoleMember, Label: "pi"}
	if err := checkMemberAdd(good); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]memberAddPayload{
		"device does not match the key": {Device: ida.ID(), PublicKey: idb.PublicKey(), Role: RoleMember, Label: "x"},
		"unknown role":                  {Device: idb.ID(), PublicKey: idb.PublicKey(), Role: "root", Label: "x"},
		"no role":                       {Device: idb.ID(), PublicKey: idb.PublicKey(), Label: "x"},
		"label with a control char":     {Device: idb.ID(), PublicKey: idb.PublicKey(), Role: RoleMember, Label: "a\x00b"},
		"label too long":                {Device: idb.ID(), PublicKey: idb.PublicKey(), Role: RoleMember, Label: strings.Repeat("x", 65)},
		"not a key":                     {Device: idb.ID(), PublicKey: "nope", Role: RoleMember, Label: "x"},
	} {
		if err := checkMemberAdd(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestMembershipSyncsAndEveryReplicaAgrees(t *testing.T) {
	ctx := context.Background()
	ida, idb, idc := newKey(t), newKey(t), newKey(t)
	a, _ := member(t, "laptop", ida)
	a.Found(ctx, "founder")
	a.AddMember(ctx, idb.PublicKey(), RoleAdmin, "second admin")
	b, _ := member(t, "pi", idb)
	pull(t, b, a, 1) // one op at a time: each add is checked against what is stored
	if err := b.AddMember(ctx, idc.PublicKey(), RoleMember, "phone"); err != nil {
		t.Fatalf("the second admin must be able to add: %v", err)
	}
	pull(t, a, b, 1)
	ra, rb := mustMembers(t, a), mustMembers(t, b)
	if len(ra.Members) != 3 || len(rb.Members) != 3 || len(ra.Problems)+len(rb.Problems) != 0 {
		t.Fatalf("%+v\n%+v", ra, rb)
	}
	for i := range ra.Members {
		if ra.Members[i] != rb.Members[i] {
			t.Fatalf("replicas disagree on member %d: %+v vs %+v", i, ra.Members[i], rb.Members[i])
		}
	}
	if ra.Admins() != 2 {
		t.Fatalf("admins = %d, want 2", ra.Admins())
	}
}

func TestAddingTheSameDeviceTwiceIsRefused(t *testing.T) {
	ctx := context.Background()
	ida, idb := newKey(t), newKey(t)
	a, _ := member(t, "laptop", ida)
	a.Found(ctx, "founder")
	if err := a.AddMember(ctx, idb.PublicKey(), RoleMember, "pi"); err != nil {
		t.Fatal(err)
	}
	if err := a.AddMember(ctx, idb.PublicKey(), RoleAdmin, "pi, promoted"); !errors.Is(err, ErrAlreadyMember) {
		t.Fatalf("err = %v: roles are fixed at enrollment, so a second add (a promotion) is refused", err)
	}
}

// ---- the pure fold -------------------------------------------------------------------------

func ev(node string, seq int64, wall int, counter int64, signer string, p memberAddPayload) memberEvent {
	return memberEvent{Node: node, Seq: seq, TS: epoch.Add(time.Duration(wall) * time.Second), Counter: counter, Signer: signer, Add: p}
}

func TestFoldIsAFunctionOfTheSetNotTheOrder(t *testing.T) {
	ka, kb, kc, kd, ke := newKey(t), newKey(t), newKey(t), newKey(t), newKey(t)
	add := func(k *identity.Identity, r Role) memberAddPayload {
		return memberAddPayload{Device: k.ID(), PublicKey: k.PublicKey(), Role: r, Label: "x"}
	}
	events := []memberEvent{
		ev("a", 1, 1, 0, ka.ID(), add(ka, RoleAdmin)),  // founding
		ev("a", 2, 2, 0, ka.ID(), add(kb, RoleAdmin)),  // a adds b (admin)
		ev("b", 1, 3, 0, kb.ID(), add(kc, RoleMember)), // b adds c
		ev("a", 3, 3, 0, ka.ID(), add(kd, RoleMember)), // same second as b's add: ties break on (counter, node, seq)
		ev("c", 1, 4, 0, kc.ID(), add(ke, RoleAdmin)),  // c is a member: invalid
		ev("e", 1, 5, 0, ke.ID(), add(ke, RoleAdmin)),  // a self-signed add after the founding: invalid
		ev("a", 4, 6, 0, ka.ID(), add(kb, RoleMember)), // b again: invalid (already a member)
	}
	want, wantBad := foldMembership(events)
	if len(want) != 4 || len(wantBad) != 3 {
		t.Fatalf("members %d, problems %d: %+v %+v", len(want), len(wantBad), want, wantBad)
	}
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 300; i++ {
		perm := append([]memberEvent(nil), events...)
		rng.Shuffle(len(perm), func(a, b int) { perm[a], perm[b] = perm[b], perm[a] })
		got, gotBad := foldMembership(perm)
		if len(got) != len(want) || len(gotBad) != len(wantBad) {
			t.Fatalf("permutation %d changed the result", i)
		}
		for d, m := range want {
			if got[d] != m {
				t.Fatalf("permutation %d: member %s differs", i, d)
			}
		}
		for k, v := range wantBad {
			if gotBad[k] != v {
				t.Fatalf("permutation %d: problem %v differs", i, k)
			}
		}
	}
}

func TestFoldTieBreaksAreDeterministicForConcurrentFoundings(t *testing.T) {
	// Two devices both "found" at the same instant on different replicas: exactly one root wins, the same on every replica.
	ka, kb := newKey(t), newKey(t)
	p := func(k *identity.Identity) memberAddPayload {
		return memberAddPayload{Device: k.ID(), PublicKey: k.PublicKey(), Role: RoleAdmin, Label: "x"}
	}
	events := []memberEvent{ev("a", 1, 1, 0, ka.ID(), p(ka)), ev("b", 1, 1, 0, kb.ID(), p(kb))}
	m1, bad1 := foldMembership(events)
	m2, bad2 := foldMembership([]memberEvent{events[1], events[0]})
	if len(m1) != 1 || len(bad1) != 1 || len(m2) != 1 || len(bad2) != 1 {
		t.Fatalf("%v %v %v %v", m1, bad1, m2, bad2)
	}
	for d := range m1 {
		if _, ok := m2[d]; !ok {
			t.Fatal("two orders, two different roots of trust")
		}
	}
}

func TestVerifyReportsAMembershipThatDoesNotHold(t *testing.T) {
	ctx := context.Background()
	ida := newKey(t)
	a, _ := member(t, "laptop", ida)
	a.Found(ctx, "founder")
	idm := newKey(t)
	// Write the forged op straight into the log, as someone with the file would.
	forged := forgeAdd(t, a, idm, idm.PublicKey(), RoleAdmin)[0]
	forged.NodeID, forged.Seq = "mallory", 1
	forged.Hash = OpHash(forged, GenesisHash)
	sig, _ := idm.Sign(sigMessage(forged.Hash))
	forged.Sig = &OpSig{Alg: identity.AlgECDSAP256, Device: idm.ID(), Sig: sig, Key: idm.PublicKey()}
	for _, q := range []string{} {
		_ = q
	}
	if _, err := a.db.ExecContext(ctx, `INSERT INTO ops (node_id, seq, ts, counter, kind, payload) VALUES ('mallory', 1, ?, 0, 'member.add', ?)`, forged.TS.UnixNano(), string(forged.Payload)); err != nil {
		t.Fatal(err)
	}
	if _, err := chainAppend(ctx, a.db, forged); err != nil {
		t.Fatal(err)
	}
	if err := storeSig(ctx, a.db, "mallory", 1, *forged.Sig, idm.PublicKey()); err != nil {
		t.Fatal(err)
	}
	rep := mustMembers(t, a)
	if len(rep.Problems) != 1 || rep.Problems[0].Node != "mallory" || len(rep.Members) != 1 {
		t.Fatalf("%+v", rep)
	}
	if _, err := a.Verify(ctx); !errors.Is(err, ErrNotAuthorized) || !strings.Contains(err.Error(), "mallory/1") {
		t.Fatalf("store verify must refuse a membership that does not hold: %v", err)
	}
}
