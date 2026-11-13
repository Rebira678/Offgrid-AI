package sqlstore

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"offgrid/internal/identity"
	"offgrid/internal/task"
)

func newTask(title string) task.NewTask {
	return task.NewTask{Title: title, Priority: task.PriorityMedium}
}

func newKey(t *testing.T) *identity.Identity {
	t.Helper()
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// signedReplica opens a replica whose ops are signed by id.
func signedReplica(t *testing.T, node string, id *identity.Identity) *Store {
	t.Helper()
	clk := &clock{t: epoch}
	return open(t, newDB(t), Options{NodeID: node, Now: clk.now, Signer: id})
}

func opsOf(t *testing.T, s *Store) []Op {
	t.Helper()
	ops, _, err := s.OpsSince(context.Background(), Summary{}, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	return ops
}

func summaryNow(t *testing.T, s *Store) Summary {
	t.Helper()
	sum, err := s.Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

// sigsOf returns the signed ops of a replica with three tasks written.
func threeSigned(t *testing.T) (*Store, *identity.Identity, []Op) {
	t.Helper()
	id := newKey(t)
	a := signedReplica(t, "laptop", id)
	for _, title := range []string{"one", "two", "three"} {
		create(t, a, title)
	}
	return a, id, opsOf(t, a)
}

func TestSignedOpCarriesVerifiableSignature(t *testing.T) {
	_, id, ops := threeSigned(t)
	if len(ops) != 3 {
		t.Fatalf("ops = %d, want 3", len(ops))
	}
	for i, op := range ops {
		if op.Sig == nil {
			t.Fatalf("op %s/%d has no signature", op.NodeID, op.Seq)
		}
		if op.Sig.Alg != identity.AlgECDSAP256 || op.Sig.Device != id.ID() || len(op.Sig.Sig) != 64 {
			t.Fatalf("op %d: bad signature envelope %+v", op.Seq, op.Sig)
		}
		// The signature is over the chain hash, under the domain-separation prefix.
		if err := identity.Verify(id.PublicKey(), sigMessage(op.Hash), op.Sig.Sig); err != nil {
			t.Fatalf("op %d: signature does not verify against its own chain hash: %v", op.Seq, err)
		}
		// The public key travels once per device per batch, with the first op.
		if (i == 0) != (op.Sig.Key != "") {
			t.Fatalf("op %d: Key = %q (want it on the first op of the batch only)", op.Seq, op.Sig.Key)
		}
	}
}

func TestUnsignedStoreHasNoSignatures(t *testing.T) {
	s, _ := freshStore(t)
	create(t, s, "legacy style")
	for _, op := range opsOf(t, s) {
		if op.Sig != nil {
			t.Fatalf("a store with no key signed an op: %+v", op.Sig)
		}
	}
	rep, err := s.VerifySignatures(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Signed != 0 || rep.Unsigned != 1 {
		t.Fatalf("report = %+v, want 0 signed, 1 unsigned", rep)
	}
}

func TestSignedOpsSyncAndVerifyOnTheOtherSide(t *testing.T) {
	a, id, _ := threeSigned(t)
	bk := newKey(t)
	b := signedReplica(t, "pi", bk)
	create(t, b, "pi task")
	pull(t, b, a, 1) // one op per batch: the key must be found again from the store
	pull(t, a, b, 2)
	for _, s := range []*Store{a, b} {
		rep, err := s.VerifySignatures(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", s.node, err)
		}
		if rep.Signed != 4 || rep.Unsigned != 0 || len(rep.Devices) != 2 {
			t.Fatalf("%s: report = %+v, want 4 signed by 2 devices", s.node, rep)
		}
		mustVerify(t, s)
	}
	if hashOf(t, a) != hashOf(t, b) {
		t.Fatal("views differ after a signed sync")
	}
	_ = id
}

func applyFresh(t *testing.T, ops []Op) (*Store, error) {
	t.Helper()
	b, _ := freshStore(t)
	_, err := b.ApplyRemote(context.Background(), ops)
	return b, err
}

func cloneOps(ops []Op) []Op {
	out := make([]Op, len(ops))
	for i, op := range ops {
		out[i] = op
		out[i].Payload = append(json.RawMessage(nil), op.Payload...)
		if op.Sig != nil {
			c := *op.Sig
			c.Sig = append([]byte(nil), op.Sig.Sig...)
			out[i].Sig = &c
		}
	}
	return out
}

func TestForgedSignatureIsRefusedAndNamesTheOp(t *testing.T) {
	_, _, ops := threeSigned(t)
	bad := cloneOps(ops)
	bad[1].Sig.Sig[10] ^= 0xff
	b, err := applyFresh(t, bad)
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("err = %v, want ErrBadSignature", err)
	}
	if !strings.Contains(err.Error(), "laptop/2") {
		t.Fatalf("the error must name the op laptop/2: %v", err)
	}
	if len(summaryNow(t, b)) != 0 {
		t.Fatal("a refused batch changed the store")
	}
}

func TestEditedOpIsRefusedAndNamesTheOp(t *testing.T) {
	_, _, ops := threeSigned(t)
	bad := cloneOps(ops)
	bad[2].Payload = json.RawMessage(strings.Replace(string(bad[2].Payload), "three", "THREE", 1))
	bad[2].Hash = "" // a relay that also strips the claimed hash
	_, err := applyFresh(t, bad)
	if !errors.Is(err, ErrBadSignature) || !strings.Contains(err.Error(), "laptop/3") {
		t.Fatalf("err = %v, want ErrBadSignature naming laptop/3", err)
	}
	// Keeping the claimed hash does not help: the signature is checked against the hash the receiver computes.
	bad = cloneOps(ops)
	bad[2].Payload = json.RawMessage(strings.Replace(string(bad[2].Payload), "three", "THREE", 1))
	_, err = applyFresh(t, bad)
	if err == nil || !(errors.Is(err, ErrBadSignature) || errors.Is(err, ErrChain)) || !strings.Contains(err.Error(), "laptop/3") {
		t.Fatalf("err = %v, want a refusal naming laptop/3", err)
	}
}

func TestFirstBadSignatureInTheBatchIsTheOneNamed(t *testing.T) {
	a := signedReplica(t, "laptop", newKey(t))
	for i := 0; i < 12; i++ {
		create(t, a, "t"+string(rune('a'+i)))
	}
	bad := cloneOps(opsOf(t, a))
	bad[8].Sig.Sig[0] ^= 1
	bad[3].Sig.Sig[0] ^= 1
	_, err := applyFresh(t, bad)
	if !errors.Is(err, ErrBadSignature) || !strings.Contains(err.Error(), "laptop/4") {
		t.Fatalf("err = %v, want laptop/4 (the lowest bad one, whatever order the pool finished in)", err)
	}
}

func TestSignatureByAnotherKeyIsRefused(t *testing.T) {
	_, id, ops := threeSigned(t)
	mallory := newKey(t)
	// Mallory signs the real hash but claims to be the laptop's device.
	bad := cloneOps(ops)
	sig, err := mallory.Sign(sigMessage(bad[0].Hash))
	if err != nil {
		t.Fatal(err)
	}
	bad[0].Sig.Sig = sig
	if _, err := applyFresh(t, bad); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("a signature by another key under the laptop's device ID: err = %v", err)
	}
	// Or ships her own key under the laptop's device ID.
	bad = cloneOps(ops)
	bad[0].Sig.Key = mallory.PublicKey()
	if _, err := applyFresh(t, bad); !errors.Is(err, ErrBadSignature) || !strings.Contains(err.Error(), "device") {
		t.Fatalf("a key that does not hash to the device ID: err = %v", err)
	}
	// An algorithm tag that does not match the key is refused too.
	bad = cloneOps(ops)
	bad[0].Sig.Alg = "ed25519"
	if _, err := applyFresh(t, bad); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("a signature tagged with another algorithm: err = %v", err)
	}
	_ = id
}

func TestNodeCannotChangeItsKeyMidHistory(t *testing.T) {
	a, _, ops := threeSigned(t)
	_ = a
	mallory := newKey(t)
	b, _ := freshStore(t)
	if _, err := b.ApplyRemote(context.Background(), ops[:1]); err != nil {
		t.Fatal(err)
	}
	// op 2 re-signed, correctly, by a different key: valid signature, wrong device for this node.
	forged := cloneOps(ops[1:2])
	prev := ops[0].Hash
	h := OpHash(forged[0], prev)
	sig, err := mallory.Sign(sigMessage(h))
	if err != nil {
		t.Fatal(err)
	}
	forged[0].Hash = ""
	forged[0].Sig = &OpSig{Alg: identity.AlgECDSAP256, Device: mallory.ID(), Sig: sig, Key: mallory.PublicKey()}
	_, err = b.ApplyRemote(context.Background(), forged)
	if !errors.Is(err, ErrKeyChange) || !strings.Contains(err.Error(), "laptop/2") {
		t.Fatalf("err = %v, want ErrKeyChange naming laptop/2", err)
	}
	// Opening the database with a different key is refused for the same reason.
	path := newDB(t)
	first := newKey(t)
	s, err := Open(context.Background(), path, Options{NodeID: "laptop", Signer: first})
	if err != nil {
		t.Fatal(err)
	}
	create(t, s, "x")
	s.Close()
	if _, err := Open(context.Background(), path, Options{NodeID: "laptop", Signer: mallory}); !errors.Is(err, ErrKeyChange) {
		t.Fatalf("Open with another key: err = %v, want ErrKeyChange", err)
	}
}

func TestUnsignedAfterSignedIsRefused(t *testing.T) {
	_, _, ops := threeSigned(t)
	bad := cloneOps(ops)
	bad[2].Sig = nil
	_, err := applyFresh(t, bad)
	if !errors.Is(err, ErrUnsigned) || !strings.Contains(err.Error(), "laptop/3") {
		t.Fatalf("err = %v, want ErrUnsigned naming laptop/3", err)
	}
	// Locally too: a replica that has signed does not quietly start writing unsigned ops.
	path := newDB(t)
	s, err := Open(context.Background(), path, Options{NodeID: "laptop", Signer: newKey(t)})
	if err != nil {
		t.Fatal(err)
	}
	create(t, s, "x")
	s.Close()
	u, err := Open(context.Background(), path, Options{NodeID: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	if _, _, err := u.Create(context.Background(), newTask("y")); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("write without a key after signing: err = %v, want ErrUnsigned", err)
	}
}

func TestLegacyOpsThenSignedOpsSync(t *testing.T) {
	path := newDB(t)
	clk := &clock{t: epoch}
	old, err := Open(context.Background(), path, Options{NodeID: "laptop", Now: clk.now})
	if err != nil {
		t.Fatal(err)
	}
	create(t, old, "before keys")
	old.Close()
	id := newKey(t)
	s := open(t, path, Options{NodeID: "laptop", Now: clk.now, Signer: id})
	create(t, s, "after keys")
	ops := opsOf(t, s)
	if ops[0].Sig != nil || ops[1].Sig == nil {
		t.Fatalf("want legacy op unsigned and new op signed, got %+v / %+v", ops[0].Sig, ops[1].Sig)
	}
	b, _ := freshStore(t)
	pull(t, b, s, 1)
	rep, err := b.VerifySignatures(context.Background())
	if err != nil || rep.Signed != 1 || rep.Unsigned != 1 {
		t.Fatalf("receiver: %+v, %v", rep, err)
	}
}

func TestRequireSigned(t *testing.T) {
	if _, err := Open(context.Background(), newDB(t), Options{NodeID: "x", RequireSigned: true}); err == nil {
		t.Fatal("RequireSigned without a key must not open")
	}
	legacy, _ := freshStore(t)
	create(t, legacy, "unsigned")
	req := open(t, newDB(t), Options{NodeID: "strict", Signer: newKey(t), RequireSigned: true})
	_, err := req.ApplyRemote(context.Background(), opsOf(t, legacy))
	if !errors.Is(err, ErrUnsigned) {
		t.Fatalf("err = %v, want ErrUnsigned", err)
	}
	_, _, signed := threeSigned(t)
	if _, err := req.ApplyRemote(context.Background(), signed); err != nil {
		t.Fatalf("signed ops must be accepted: %v", err)
	}
}

func TestSignatureTablesAreAppendOnly(t *testing.T) {
	a, _, _ := threeSigned(t)
	ctx := context.Background()
	for _, q := range []string{
		`UPDATE op_sigs SET sig = x'00'`, `DELETE FROM op_sigs`,
		`UPDATE device_keys SET public_key = 'x'`, `DELETE FROM device_keys`,
		`UPDATE node_devices SET device_id = 'x'`, `DELETE FROM node_devices`,
	} {
		if _, err := a.db.ExecContext(ctx, q); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("%s: err = %v, want an append-only refusal", q, err)
		}
	}
}

func TestVerifySignaturesFindsAStoredSignatureThatWasEdited(t *testing.T) {
	a, _, _ := threeSigned(t)
	ctx := context.Background()
	// Someone with the file removes the guard and flips a bit.
	for _, tr := range []string{"op_sigs_append_only_update"} {
		if _, err := a.db.ExecContext(ctx, `DROP TRIGGER `+tr); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.db.ExecContext(ctx, `UPDATE op_sigs SET sig = zeroblob(64) WHERE node_id = 'laptop' AND seq = 2`); err != nil {
		t.Fatal(err)
	}
	_, err := a.VerifySignatures(ctx)
	var se *SigError
	if !errors.As(err, &se) || se.Node != "laptop" || se.Seq != 2 || !errors.Is(err, ErrBadSignature) {
		t.Fatalf("err = %v, want a SigError for laptop/2", err)
	}
	if _, err := a.Verify(ctx); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("store verify must report it too: %v", err)
	}
}

func TestAttestationCoversLegacyHistoryOnly(t *testing.T) {
	path := newDB(t)
	clk := &clock{t: epoch}
	old, err := Open(context.Background(), path, Options{NodeID: "laptop", Now: clk.now})
	if err != nil {
		t.Fatal(err)
	}
	create(t, old, "one")
	create(t, old, "two")
	old.Close()
	id := newKey(t)
	s := open(t, path, Options{NodeID: "laptop", Now: clk.now, Signer: id})
	ctx := context.Background()

	rep, _ := s.VerifySignatures(ctx)
	if rep.Unsigned != 2 || rep.Attested != 0 {
		t.Fatalf("before attesting: %+v", rep)
	}
	if _, err := s.Attest(ctx); err != nil {
		t.Fatal(err)
	}
	rep, err = s.VerifySignatures(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Attested != 2 || rep.Unsigned != 0 || rep.Signed != 1 || len(rep.Attestations) != 1 || rep.Attestations[0].By != id.ID() || rep.Attestations[0].Covers != 2 {
		t.Fatalf("after attesting: %+v", rep)
	}
	// Unsigned history that arrives afterwards is NOT covered by the earlier attestation.
	late := open(t, newDB(t), Options{NodeID: "pi", Now: clk.now})
	create(t, late, "late legacy")
	pull(t, s, late, 10)
	rep, _ = s.VerifySignatures(ctx)
	if rep.Unsigned != 1 || rep.Attested != 2 {
		t.Fatalf("a legacy op that arrived after the attestation must stay unattested: %+v", rep)
	}
}

func TestAttestationNeedsAKeyAndASignature(t *testing.T) {
	s, _ := freshStore(t)
	if _, err := s.Attest(context.Background()); err == nil {
		t.Fatal("a store with no key cannot attest")
	}
	_, _, ops := threeSigned(t)
	att := Op{NodeID: "laptop", Seq: 1, TS: epoch, Kind: KindHistoryAttested, Payload: json.RawMessage(`{"heads":[]}`)}
	_ = ops
	if err := validateRemote(att); !errors.Is(err, ErrBadOp) {
		t.Fatalf("an unsigned attestation must be refused: %v", err)
	}
}

func TestAttestationSurvivesSync(t *testing.T) {
	path := newDB(t)
	clk := &clock{t: epoch}
	old, _ := Open(context.Background(), path, Options{NodeID: "laptop", Now: clk.now})
	create(t, old, "legacy")
	old.Close()
	s := open(t, path, Options{NodeID: "laptop", Now: clk.now, Signer: newKey(t)})
	if _, err := s.Attest(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, _ := freshStore(t)
	pull(t, b, s, 1)
	rep, err := b.VerifySignatures(context.Background())
	if err != nil || rep.Attested != 1 || rep.Unsigned != 0 {
		t.Fatalf("receiver: %+v, %v", rep, err)
	}
	mustVerify(t, b)
}

// ---- migration: a database written by the Day 30 program -----------------------------------

type goldenSidecar struct {
	Node      string `json:"node"`
	Ops       int    `json:"ops"`
	StateHash string `json:"state_hash"`
	Heads     []Head `json:"heads"`
}

func copyGolden(t *testing.T) (string, goldenSidecar) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "day30-golden.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	side, err := os.ReadFile(filepath.Join("testdata", "day30-golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var g goldenSidecar
	if err := json.Unmarshal(side, &g); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "golden.db")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return p, g
}

func TestGoldenDay30DatabaseMigratesToSignedSchema(t *testing.T) {
	ctx := context.Background()
	p, g := copyGolden(t)
	s, err := Open(ctx, p, Options{MustExist: true})
	if err != nil {
		t.Fatalf("opening a Day 30 database: %v", err)
	}
	defer s.Close()
	if v, err := schemaVersion(ctx, s.db); err != nil || v != 6 {
		t.Fatalf("schema version = %d, %v; want 6", v, err)
	}
	if s.NodeID() != g.Node {
		t.Fatalf("node = %s", s.NodeID())
	}
	if h := hashOf(t, s); h != g.StateHash {
		t.Fatalf("the view changed in migration: %s != %s", h, g.StateHash)
	}
	heads, err := s.Heads(ctx)
	if err != nil || len(heads) != len(g.Heads) {
		t.Fatalf("heads = %v, %v", heads, err)
	}
	for i := range heads {
		if heads[i] != g.Heads[i] {
			t.Fatalf("head %d changed in migration: %+v != %+v", i, heads[i], g.Heads[i])
		}
	}
	if _, err := s.VerifyChain(ctx); err != nil {
		t.Fatal(err)
	}
	rep, err := s.VerifySignatures(ctx)
	if err != nil || rep.Signed != 0 || rep.Unsigned != g.Ops {
		t.Fatalf("a Day 30 database has no signatures: %+v, %v", rep, err)
	}
	mustVerify(t, s)
	// Writes without a key still work (signing is opt-in until Day 37) and stay unsigned.
	create(t, s, "after the migration")
	if rep, _ = s.VerifySignatures(ctx); rep.Unsigned != g.Ops+1 {
		t.Fatalf("%+v", rep)
	}
}

func TestGoldenDay30DatabaseThenSignedAndAttested(t *testing.T) {
	ctx := context.Background()
	p, g := copyGolden(t)
	id := newKey(t)
	late := &clock{t: epoch.Add(5 * 24 * time.Hour)} // after the golden database's own timestamps (12 Oct)
	s, err := Open(ctx, p, Options{MustExist: true, Signer: id, Now: late.now})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	create(t, s, "first signed op")
	if _, err := s.Attest(ctx); err != nil {
		t.Fatal(err)
	}
	rep, err := s.VerifySignatures(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Attested != g.Ops || rep.Unsigned != 0 || rep.Signed != 2 {
		t.Fatalf("legacy ops must be attested, new ops signed: %+v", rep)
	}
	// A fresh replica can take the whole mixed history, and ends in the same state.
	b := open(t, newDB(t), Options{NodeID: "fresh", Now: late.now})
	pull(t, b, s, 3)
	if hashOf(t, b) != hashOf(t, s) {
		t.Fatal("views differ")
	}
	if rb, err := b.VerifySignatures(ctx); err != nil || rb.Attested != g.Ops || rb.Signed != 2 {
		t.Fatalf("receiver: %+v, %v", rb, err)
	}
}

func TestSignedMessageIsDomainSeparated(t *testing.T) {
	_, id, ops := threeSigned(t)
	want := []byte("offgrid-opsig-v1\x00" + ops[0].Hash)
	if err := identity.Verify(id.PublicKey(), want, ops[0].Sig.Sig); err != nil {
		t.Fatalf("the signed bytes must be the fixed prefix plus the chain hash: %v", err)
	}
	if err := identity.Verify(id.PublicKey(), []byte(ops[0].Hash), ops[0].Sig.Sig); err == nil {
		t.Fatal("a signature over the bare hash must not be accepted: another use of the key could be replayed")
	}
}

func TestAttestationStopsCoveringHistoryThatNoLongerMatches(t *testing.T) {
	path := newDB(t)
	clk := &clock{t: epoch}
	old, _ := Open(context.Background(), path, Options{NodeID: "laptop", Now: clk.now})
	create(t, old, "one")
	create(t, old, "two")
	old.Close()
	s := open(t, path, Options{NodeID: "laptop", Now: clk.now, Signer: newKey(t)})
	ctx := context.Background()
	if _, err := s.Attest(ctx); err != nil {
		t.Fatal(err)
	}
	// Someone rewrites the stored head the attestation named.
	for _, q := range []string{`DROP TRIGGER op_chain_append_only_update`} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE op_chain SET hash = ? WHERE node_id = 'laptop' AND seq = 2`, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	rep, err := s.VerifySignatures(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Attested != 0 || rep.Unsigned != 2 {
		t.Fatalf("an attestation of a head that does not match must vouch for nothing: %+v", rep)
	}
}
