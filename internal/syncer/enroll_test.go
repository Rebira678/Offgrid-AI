package syncer

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"offgrid/internal/identity"
	"offgrid/internal/store/sqlstore"
)

// enrollServer is a founded admin node serving the sync and enroll endpoints.
type enrollEnv struct {
	admin *sqlstore.Store
	ida   *identity.Identity
	srv   *httptest.Server
	clk   *fakeClock
}

func newEnrollEnv(t *testing.T) *enrollEnv {
	t.Helper()
	clk := &fakeClock{t0}
	a, ida := newSignedStore(t, "laptop", clk)
	if err := a.Found(context.Background(), "founder"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(Handler(a, ServerOptions{Key: key, Enroller: a, Now: clk.now}))
	t.Cleanup(srv.Close)
	return &enrollEnv{a, ida, srv, clk}
}

func (e *enrollEnv) invite(t *testing.T, role sqlstore.Role) (sqlstore.Invite, sqlstore.InviteLink) {
	t.Helper()
	inv, err := e.admin.CreateInvite(context.Background(), role, "x", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	link, err := sqlstore.ParseInviteLink(inv.Link(""))
	if err != nil {
		t.Fatal(err)
	}
	return inv, link
}

func newKeyT(t *testing.T) *identity.Identity {
	t.Helper()
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func request(t *testing.T, token string, id *identity.Identity, label string) EnrollRequest {
	t.Helper()
	proof, err := sqlstore.EnrollProof(token, id, label)
	if err != nil {
		t.Fatal(err)
	}
	return EnrollRequest{Token: token, PublicKey: id.PublicKey(), Label: label, Proof: proof}
}

func TestANewDeviceEnrollsOverHTTPAndThenSyncsTheMembership(t *testing.T) {
	ctx := context.Background()
	e := newEnrollEnv(t)
	inv, link := e.invite(t, sqlstore.RoleMember)
	idb := newKeyT(t)
	resp, err := Enroll(ctx, nil, e.srv.URL, request(t, inv.Token, idb, "Pi"))
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckEnrollment(resp, link, idb, "Pi"); err != nil {
		t.Fatalf("the device must accept an honest enrollment: %v", err)
	}
	// Now it is a member: it syncs and sees the membership, including itself.
	b := newStore(t, "pi", e.clk)
	cl := &Client{Peer: e.srv.URL, Key: key, Node: "pi", Now: e.clk.now}
	if _, err := cl.Once(ctx, b); err != nil {
		t.Fatal(err)
	}
	rep, err := b.Members(ctx)
	if err != nil || len(rep.Members) != 2 || rep.Founder() != e.ida.ID() {
		t.Fatalf("%+v %v", rep, err)
	}
}

func TestEnrollNeedsNoSharedKeyButSyncStillDoes(t *testing.T) {
	e := newEnrollEnv(t)
	inv, _ := e.invite(t, sqlstore.RoleMember)
	idb := newKeyT(t)
	if _, err := Enroll(context.Background(), nil, e.srv.URL, request(t, inv.Token, idb, "x")); err != nil {
		t.Fatalf("a new device has no shared key yet and must still be able to present its invite: %v", err)
	}
	resp, err := http.Get(e.srv.URL + PathSummary)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the sync endpoints must stay authenticated: %d", resp.StatusCode)
	}
}

func TestEveryKindOfBadInviteGetsTheSameAnswer(t *testing.T) {
	ctx := context.Background()
	e := newEnrollEnv(t)
	idb := newKeyT(t)
	used, _ := e.invite(t, sqlstore.RoleMember)
	if _, err := Enroll(ctx, nil, e.srv.URL, request(t, used.Token, idb, "x")); err != nil {
		t.Fatal(err)
	}
	expired, _ := e.invite(t, sqlstore.RoleMember)
	e.clk.t = e.clk.t.Add(2 * time.Hour)
	fresh, _ := e.invite(t, sqlstore.RoleMember)
	wrongSecret := fresh.Token[:len(fresh.Token)-1] + "B"
	if strings.HasSuffix(fresh.Token, "B") {
		wrongSecret = fresh.Token[:len(fresh.Token)-1] + "C"
	}
	idc := newKeyT(t)
	var bodies []string
	for name, tok := range map[string]string{"used": used.Token, "expired": expired.Token, "wrong secret": wrongSecret, "unknown": "AAAAAAAAAAAAAAAAAAAAAAAA", "garbage": "nonsense"} {
		_, err := Enroll(ctx, nil, e.srv.URL, request2(t, tok, idc, "y"))
		var re *RemoteError
		if !errors.As(err, &re) || re.Status != http.StatusForbidden || re.Code != CodeInviteInvalid {
			t.Fatalf("%s: err = %v, want 403 %s", name, err, CodeInviteInvalid)
		}
		bodies = append(bodies, re.Msg)
	}
	for _, b := range bodies[1:] {
		if b != bodies[0] {
			t.Fatalf("the answers differ, so they tell a guesser which invites exist: %q vs %q", bodies[0], b)
		}
	}
}

// request2 builds a request even when the token is not well formed (the proof is then garbage).
func request2(t *testing.T, token string, id *identity.Identity, label string) EnrollRequest {
	proof, err := sqlstore.EnrollProof(token, id, label)
	if err != nil {
		proof = []byte("garbage")
	}
	return EnrollRequest{Token: token, PublicKey: id.PublicKey(), Label: label, Proof: proof}
}

func TestGuessingIsRateLimited(t *testing.T) {
	ctx := context.Background()
	e := newEnrollEnv(t)
	good, _ := e.invite(t, sqlstore.RoleMember)
	idb := newKeyT(t)
	for i := 0; i < MaxEnrollFailures; i++ {
		if _, err := Enroll(ctx, nil, e.srv.URL, request2(t, "AAAAAAAAAAAAAAAAAAAAAAAA", idb, "x")); err == nil {
			t.Fatal("a bad invite worked")
		}
	}
	// Past the limit even the RIGHT invite waits: a guesser must not get unlimited tries.
	_, err := Enroll(ctx, nil, e.srv.URL, request(t, good.Token, idb, "x"))
	var re *RemoteError
	if !errors.As(err, &re) || re.Status != http.StatusTooManyRequests || re.Code != CodeTooManyAttempts {
		t.Fatalf("err = %v, want 429 %s", err, CodeTooManyAttempts)
	}
	// After the window it works again.
	e.clk.t = e.clk.t.Add(EnrollFailureWindow + time.Second)
	if _, err := Enroll(ctx, nil, e.srv.URL, request(t, good.Token, idb, "x")); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

func TestANodeWithoutAnEnrollerDoesNotAnswerEnrollRequests(t *testing.T) {
	clk := &fakeClock{t0}
	s := newStore(t, "pi", clk)
	srv := httptest.NewServer(Handler(s, ServerOptions{Key: key, Now: clk.now}))
	defer srv.Close()
	idb := newKeyT(t)
	_, err := Enroll(context.Background(), nil, srv.URL, request2(t, "AAAAAAAAAAAAAAAAAAAAAAAA", idb, "x"))
	var re *RemoteError
	if !errors.As(err, &re) || re.Status != http.StatusNotFound {
		t.Fatalf("err = %v, want 404", err)
	}
}

func TestTheDeviceRefusesAnEnrollmentFromTheWrongAdmin(t *testing.T) {
	// The invite pinned admin A. A node that answers as admin M (Mallory) must be refused,
	// even though M's receipt is a valid signature by M.
	e := newEnrollEnv(t)
	inv, link := e.invite(t, sqlstore.RoleMember)
	idb := newKeyT(t)
	resp, err := Enroll(context.Background(), nil, e.srv.URL, request(t, inv.Token, idb, "x"))
	if err != nil {
		t.Fatal(err)
	}
	mallory := newKeyT(t)
	forged := resp
	forged.AdminDevice, forged.AdminKey = mallory.ID(), mallory.PublicKey()
	forged.Receipt, _ = mallory.Sign(sqlstore.ReceiptMessage(resp.InviteID, resp.Device, sqlstore.Role(resp.Role), resp.Label))
	if err := CheckEnrollment(forged, link, idb, "x"); !errors.Is(err, ErrWrongAdmin) {
		t.Fatalf("err = %v, want ErrWrongAdmin", err)
	}
	// The pinned admin's key with someone else's receipt.
	bad := resp
	bad.Receipt = append([]byte(nil), resp.Receipt...)
	bad.Receipt[5] ^= 1
	if err := CheckEnrollment(bad, link, idb, "x"); !errors.Is(err, ErrWrongAdmin) {
		t.Fatalf("a bad receipt: err = %v", err)
	}
	// A receipt for a different device (the admin enrolled someone else).
	other := resp
	other.Device = newKeyT(t).ID()
	if err := CheckEnrollment(other, link, idb, "x"); !errors.Is(err, ErrWrongAdmin) {
		t.Fatalf("a receipt for another device: err = %v", err)
	}
	// A perfectly valid answer, but for ANOTHER device (someone replays it to us).
	inv2, link2 := e.invite(t, sqlstore.RoleMember)
	idc := newKeyT(t)
	resp2, err := Enroll(context.Background(), nil, e.srv.URL, request(t, inv2.Token, idc, "x"))
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckEnrollment(resp2, link2, idb, "x"); !errors.Is(err, ErrWrongAdmin) {
		t.Fatalf("a valid answer for another device was accepted: %v", err)
	}
	// The admin field alone disagreeing with the pinned device.
	lying := resp
	lying.AdminDevice = mallory.ID()
	if err := CheckEnrollment(lying, link, idb, "x"); !errors.Is(err, ErrWrongAdmin) {
		t.Fatalf("a mismatched admin field was accepted: %v", err)
	}
	if err := CheckEnrollment(resp, link, idb, "x"); err != nil {
		t.Fatalf("the honest answer must pass: %v", err)
	}
}

func TestAnOversizedEnrollRequestIsRefused(t *testing.T) {
	e := newEnrollEnv(t)
	big := bytes.Repeat([]byte("a"), 64<<10)
	resp, err := http.Post(e.srv.URL+PathEnroll, "application/json", bytes.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestAStrangersOpsAreRefusedOverTheWireAndARevokedDevicesAreKeptAsVoid(t *testing.T) {
	ctx := context.Background()
	e := newEnrollEnv(t)
	inv, _ := e.invite(t, sqlstore.RoleMember)
	idb := newKeyT(t)
	if _, err := Enroll(ctx, nil, e.srv.URL, request(t, inv.Token, idb, "Pi")); err != nil {
		t.Fatal(err)
	}
	// A stranger with the shared key but no enrollment.
	stranger, _ := newSignedStoreWith(t, "mallory", e.clk, newKeyT(t))
	add(t, stranger, "hostile")
	cl := &Client{Peer: e.srv.URL, Key: key, Node: "mallory", Now: e.clk.now}
	_, err := cl.Once(ctx, stranger)
	var re *RemoteError
	if !errors.As(err, &re) || re.Status != http.StatusForbidden || re.Code != CodeNotAuthorized {
		t.Fatalf("a stranger's push: %v, want 403 %s", err, CodeNotAuthorized)
	}
	// The enrolled Pi is revoked; what it writes afterwards is accepted into the log and does not count.
	pi, _ := newSignedStoreWith(t, "pi", e.clk, idb)
	cp := &Client{Peer: e.srv.URL, Key: key, Node: "pi", Now: e.clk.now}
	if _, err := cp.Once(ctx, pi); err != nil {
		t.Fatal(err)
	}
	if _, err := e.admin.Revoke(ctx, idb.ID()); err != nil {
		t.Fatal(err)
	}
	add(t, pi, "after")
	if _, err := cp.Once(ctx, pi); err != nil {
		t.Fatalf("a revoked device's sync is not an error, its work is void: %v", err)
	}
	v, err := e.admin.VoidOps(ctx)
	if err != nil || len(v) == 0 {
		t.Fatalf("%v %v", v, err)
	}
}

func newSignedStoreWith(t testing.TB, node string, clk *fakeClock, id *identity.Identity) (*sqlstore.Store, *identity.Identity) {
	t.Helper()
	s, err := sqlstore.Open(context.Background(), filepath.Join(t.TempDir(), "x.db"), sqlstore.Options{NodeID: node, Now: clk.now, Signer: id})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, id
}
