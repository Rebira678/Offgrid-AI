package syncer_test

import (
	"context"
	"encoding/base64"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"offgrid/internal/identity"
	"offgrid/internal/store/sqlstore"
	"offgrid/internal/syncer"
	"offgrid/internal/task"
)

// TestSyncKeyPermissionSecurity closes Threat Model finding N10:
// LoadKey must enforce owner-only permissions (0600 or 0400 on non-Windows)
// and reject directories, symlinks, or overly open key files.
func TestSyncKeyPermissionSecurity(t *testing.T) {
	dir := t.TempDir()

	// 1. Valid 0600 key file
	validPath := filepath.Join(dir, "sync.key")
	if err := os.WriteFile(validPath, []byte("this-is-a-valid-secret-key-32b!!"), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := syncer.LoadKey(validPath)
	if err != nil {
		t.Fatalf("LoadKey failed on 0600 file: %v", err)
	}
	if len(key) == 0 {
		t.Fatal("expected non-empty key")
	}

	if runtime.GOOS != "windows" {
		// 2. Overly permissive 0644 key file must be refused
		loosePath := filepath.Join(dir, "loose.key")
		if err := os.WriteFile(loosePath, []byte("this-is-a-valid-secret-key-32b!!"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err = syncer.LoadKey(loosePath)
		if err == nil || !strings.Contains(err.Error(), "mode 0644") || !strings.Contains(err.Error(), "chmod 600") {
			t.Fatalf("expected permission error with chmod hint, got %v", err)
		}

		// 3. Symlink rejected
		symPath := filepath.Join(dir, "sym.key")
		if err := os.Symlink(validPath, symPath); err != nil {
			t.Fatal(err)
		}
		_, err = syncer.LoadKey(symPath)
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("expected symlink rejection, got %v", err)
		}
	}

	// 4. Directory rejected
	dirPath := filepath.Join(dir, "dir.key")
	if err := os.Mkdir(dirPath, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = syncer.LoadKey(dirPath)
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("expected directory rejection, got %v", err)
	}
}

// TestEndToEndSignedOpsSync verifies that two stores replicating via HTTP
// correctly push and pull signed ops, verify signatures on receipt,
// and store signatures into op_sigs.
func TestEndToEndSignedOpsSync(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	idA, _ := identity.Generate()
	idB, _ := identity.Generate()

	storeA, err := sqlstore.Open(ctx, filepath.Join(dir, "nodeA.db"), sqlstore.Options{
		NodeID:           "node-a",
		Identity:         idA,
		RequireSignedOps: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer storeA.Close()

	storeB, err := sqlstore.Open(ctx, filepath.Join(dir, "nodeB.db"), sqlstore.Options{
		NodeID:           "node-b",
		Identity:         idB,
		RequireSignedOps: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer storeB.Close()

	// Create tasks on storeA
	_, _, err = storeA.Create(ctx, task.NewTask{Title: "Task from Node A", Priority: task.PriorityHigh})
	if err != nil {
		t.Fatal(err)
	}

	// Set up HTTP sync server on node A
	syncKey := []byte("super-secret-sync-key-32-bytes!!")
	handler := syncer.Handler(storeA, syncer.ServerOptions{Key: syncKey})
	server := httptest.NewServer(handler)
	defer server.Close()

	// Node B syncs from Node A
	client := &syncer.Client{Peer: server.URL, Key: syncKey}
	res, err := client.Once(ctx, storeB)
	if err != nil {
		t.Fatalf("client.Once failed: %v", err)
	}
	if res.Pulled != 1 {
		t.Fatalf("expected 1 pulled op, got %d", res.Pulled)
	}

	// Verify that Store B has verified and stored the signature
	reportB, err := storeB.Verify(ctx)
	if err != nil {
		t.Fatalf("storeB.Verify failed: %v", err)
	}
	if reportB.SignedOps != 1 {
		t.Fatalf("expected 1 signed op in storeB, got %d", reportB.SignedOps)
	}

	// Verify task exists in Store B
	tasksB, err := storeB.List(ctx, task.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasksB) != 1 || tasksB[0].Title != "Task from Node A" {
		t.Fatalf("unexpected tasks in storeB: %+v", tasksB)
	}
}

// TestSyncRejectsForgedRemoteSignature tests that if a malicious remote node
// transmits a WireOp with a forged or invalid signature, ApplyRemote refuses it
// and reports a tamper error.
func TestSyncRejectsForgedRemoteSignature(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	idB, _ := identity.Generate()
	storeB, err := sqlstore.Open(ctx, filepath.Join(dir, "nodeB.db"), sqlstore.Options{
		NodeID:           "node-b",
		Identity:         idB,
		RequireSignedOps: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer storeB.Close()

	// Construct an op from node-attacker with a fake signature
	idAttacker, _ := identity.Generate()
	fakeOp := sqlstore.Op{
		NodeID:   "node-attacker",
		Seq:      1,
		TS:       time.Now().UTC(),
		Counter:  0,
		Kind:     sqlstore.KindTaskCreated,
		Payload:  []byte(`{"id":"T1","title":"Injected","site":"","priority":"high"}`),
		Sig:      make([]byte, 64), // all zeroes, invalid signature!
		PubKey:   idAttacker.PublicKey(),
		DeviceID: idAttacker.ID(),
		Alg:      identity.AlgECDSAP256,
	}
	fakeOp.Hash = sqlstore.OpHash(fakeOp, sqlstore.GenesisHash)

	_, err = storeB.ApplyRemote(ctx, []sqlstore.Op{fakeOp})
	if err == nil || !strings.Contains(err.Error(), "forged or edited signature") {
		t.Fatalf("expected ApplyRemote to reject forged signature, got %v", err)
	}
}

// TestWireOpConversionRoundTrip tests serialization and deserialization
// of WireOp with signatures, base64 encoding, and algorithm fields.
func TestWireOpConversionRoundTrip(t *testing.T) {
	id, _ := identity.Generate()
	sigBytes := []byte("0123456789012345678901234567890101234567890123456789012345678901") // 64 bytes
	orig := sqlstore.Op{
		NodeID:   "node-roundtrip",
		Seq:      42,
		TS:       time.Unix(1700000000, 0).UTC(),
		Counter:  5,
		Kind:     sqlstore.KindTaskCreated,
		Payload:  []byte(`{"id":"T42","title":"Roundtrip"}`),
		Hash:     "abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234",
		Sig:      sigBytes,
		PubKey:   id.PublicKey(),
		DeviceID: id.ID(),
		Alg:      identity.AlgECDSAP256,
	}

	wire := syncer.ToWire([]sqlstore.Op{orig})
	if len(wire) != 1 {
		t.Fatalf("expected 1 wire op, got %d", len(wire))
	}
	w := wire[0]
	if w.Sig != base64.RawURLEncoding.EncodeToString(sigBytes) {
		t.Errorf("wire sig mismatch: got %s", w.Sig)
	}
	if w.PubKey != orig.PubKey {
		t.Errorf("wire pubkey mismatch: got %s, want %s", w.PubKey, orig.PubKey)
	}
	if w.DeviceID != orig.DeviceID {
		t.Errorf("wire device ID mismatch: got %s, want %s", w.DeviceID, orig.DeviceID)
	}
	if w.Alg != orig.Alg {
		t.Errorf("wire alg mismatch: got %s, want %s", w.Alg, orig.Alg)
	}

	roundtrip := syncer.FromWire(wire)
	if len(roundtrip) != 1 {
		t.Fatalf("expected 1 reconstructed op, got %d", len(roundtrip))
	}
	r := roundtrip[0]
	if string(r.Sig) != string(orig.Sig) {
		t.Errorf("roundtrip sig mismatch: got %x, want %x", r.Sig, orig.Sig)
	}
	if r.PubKey != orig.PubKey {
		t.Errorf("roundtrip pubkey mismatch: got %s, want %s", r.PubKey, orig.PubKey)
	}
	if r.DeviceID != orig.DeviceID {
		t.Errorf("roundtrip device ID mismatch: got %s, want %s", r.DeviceID, orig.DeviceID)
	}
	if r.Alg != orig.Alg {
		t.Errorf("roundtrip alg mismatch: got %s, want %s", r.Alg, orig.Alg)
	}
}
