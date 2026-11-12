package syncer

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"offgrid/internal/identity"
	"offgrid/internal/store/sqlstore"
)

func newSignedStore(t testing.TB, node string, clk *fakeClock) (*sqlstore.Store, *identity.Identity) {
	t.Helper()
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	s, err := sqlstore.Open(context.Background(), filepath.Join(t.TempDir(), "x.db"), sqlstore.Options{NodeID: node, Now: clk.now, Signer: id})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, id
}

// Found on Day 37 by running two real processes: WireOp had no signature field,
// so a signed op crossed the network unsigned and the receiver stored it as legacy.
func TestSignaturesSurviveTheWire(t *testing.T) {
	clk := &fakeClock{t0}
	a, _ := newSignedStore(t, "laptop", clk)
	add(t, a, "one")
	add(t, a, "two")
	ops, _, err := a.OpsSince(context.Background(), sqlstore.Summary{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	back := FromWire(ToWire(ops))
	for i := range ops {
		if back[i].Sig == nil || ops[i].Sig == nil {
			t.Fatalf("op %d lost its signature on the wire: before %+v, after %+v", i, ops[i].Sig, back[i].Sig)
		}
		if back[i].Sig.Device != ops[i].Sig.Device || back[i].Sig.Alg != ops[i].Sig.Alg || back[i].Sig.Key != ops[i].Sig.Key || string(back[i].Sig.Sig) != string(ops[i].Sig.Sig) {
			t.Fatalf("op %d: signature changed on the wire", i)
		}
	}
	// An unsigned op stays unsigned (no empty signature object invented).
	u := newStore(t, "pi", clk)
	add(t, u, "legacy")
	uops, _, _ := u.OpsSince(context.Background(), sqlstore.Summary{}, 100)
	if FromWire(ToWire(uops))[0].Sig != nil {
		t.Fatal("an unsigned op came out of the wire signed")
	}
}

func TestTwoSignedReplicasSyncOverHTTPAndEveryOpStaysSignedOnBothSides(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{t0}
	a, ida := newSignedStore(t, "laptop", clk)
	b, idb := newSignedStore(t, "pi", clk)
	add(t, a, "a1")
	add(t, a, "a2")
	add(t, b, "b1")
	cl, _ := serve(t, b, clk, key)
	cl.Node = "laptop"
	if _, err := cl.Once(ctx, a); err != nil {
		t.Fatal(err)
	}
	if hash(t, a) != hash(t, b) {
		t.Fatal("views differ")
	}
	for name, s := range map[string]*sqlstore.Store{"laptop": a, "pi": b} {
		rep, err := s.VerifySignatures(ctx)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if rep.Signed != 3 || rep.Unsigned != 0 || len(rep.Devices) != 2 {
			t.Fatalf("%s: %+v, want 3 signed by 2 devices", name, rep)
		}
		got := map[string]bool{}
		for _, d := range rep.Devices {
			got[d.Device] = true
		}
		if !got[ida.ID()] || !got[idb.ID()] {
			t.Fatalf("%s: devices %v, want both %s and %s", name, rep.Devices, ida.ID(), idb.ID())
		}
	}
}

func TestAForgedSignatureOverHTTPIsRefusedWithTheCodeAndTheOp(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{t0}
	a, _ := newSignedStore(t, "laptop", clk)
	add(t, a, "a1")
	b := newStore(t, "pi", clk)
	// The server side is a relay that flips a bit in every signature it forwards.
	ops, _, _ := a.OpsSince(ctx, sqlstore.Summary{}, 100)
	ops[0].Sig.Sig[3] ^= 1
	cl, _ := serve(t, b, clk, key)
	cl.Node = "mallory"
	var pr PushResponse
	err := cl.call(ctx, "POST", PathPush, PushRequest{Ops: ToWire(ops)}, &pr)
	if err == nil || !strings.Contains(err.Error(), "laptop/1") || !strings.Contains(err.Error(), CodeBadSignature) {
		t.Fatalf("err = %v, want the %s code and the op laptop/1", err, CodeBadSignature)
	}
	if rep, _ := b.VerifySignatures(ctx); rep.Ops != 0 {
		t.Fatal("a refused batch left ops behind")
	}
}
