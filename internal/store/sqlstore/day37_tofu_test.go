package sqlstore

import (
	"context"
	"errors"
	"testing"

	"offgrid/internal/identity"
)

// TestDay37CheckpointTrustOnFirstUseBindingEnforcement tests that once a node
// records its first signed op, any attempt to sync ops from a different device
// key for that node is refused with ErrKeyChange (TOFU invariant).
func TestDay37CheckpointTrustOnFirstUseBindingEnforcement(t *testing.T) {
	ctx := context.Background()
	devKeyA := newKey(t)
	mallory := newKey(t)

	// Legitimate node "pump-station" signed by devKeyA
	orig := signedReplica(t, "pump-station", devKeyA)
	if _, _, err := orig.Create(ctx, newTask("Initial Op")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := orig.Create(ctx, newTask("Second Op")); err != nil {
		t.Fatal(err)
	}
	origOps := opsOf(t, orig)
	if len(origOps) != 2 {
		t.Fatalf("expected 2 ops, got %d", len(origOps))
	}

	// Peer receives first op and binds "pump-station" to devKeyA
	peer := signedReplica(t, "central-hub", newKey(t))
	if _, err := peer.ApplyRemote(ctx, origOps[:1]); err != nil {
		t.Fatalf("peer apply first op: %v", err)
	}

	// Mallory tries to forge op 2 for "pump-station" signed with her key
	forged := make([]Op, 1)
	forged[0] = origOps[1]
	prev := origOps[0].Hash
	h := OpHash(forged[0], prev)
	sig, err := mallory.Sign(sigMessage(h))
	if err != nil {
		t.Fatal(err)
	}
	forged[0].Hash = ""
	forged[0].Sig = &OpSig{Alg: identity.AlgECDSAP256, Device: mallory.ID(), Sig: sig, Key: mallory.PublicKey()}

	// Peer must reject forged op 2 because device key changed
	_, err = peer.ApplyRemote(ctx, forged)
	if !errors.Is(err, ErrKeyChange) {
		t.Fatalf("peer ApplyRemote with changed key: err = %v, want ErrKeyChange", err)
	}

	// Peer database view must be uncorrupted
	sum, err := peer.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum["pump-station"] != 1 {
		t.Errorf("peer last sequence for pump-station: %d, want 1", sum["pump-station"])
	}
}
