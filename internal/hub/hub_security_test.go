package hub

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"

	"offgrid/internal/syncer"
)

func TestHubHandshakeAuthorizerMultiTenantIsolation(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)

	founderA := newID(t)
	founderB := newID(t)
	stranger := newID(t)

	if err := e.hub.AddGroup(ctx, "tenant-a", founderA.ID()); err != nil {
		t.Fatal(err)
	}
	if err := e.hub.AddGroup(ctx, "tenant-b", founderB.ID()); err != nil {
		t.Fatal(err)
	}

	auth := e.hub.HandshakeAuthorizer()

	// 1. Founder A is authorized for handshake
	if err := auth(founderA.ID(), founderA.PublicKey()); err != nil {
		t.Fatalf("founder A handshake refused: %v", err)
	}

	// 2. Founder B is authorized for handshake
	if err := auth(founderB.ID(), founderB.PublicKey()); err != nil {
		t.Fatalf("founder B handshake refused: %v", err)
	}

	// 3. Stranger is refused at handshake
	if err := auth(stranger.ID(), stranger.PublicKey()); err != syncer.ErrNotEnrolled {
		t.Fatalf("stranger handshake error = %v, want ErrNotEnrolled", err)
	}

	// 4. Mismatched device ID and public key is refused
	if err := auth(founderA.ID(), stranger.PublicKey()); err == nil {
		t.Fatal("mismatched key and device ID was accepted")
	}
}

func TestHubEmptyOrInvalidCertificatesProduceUniformNotFound(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	founder := newID(t)
	if err := e.hub.AddGroup(ctx, "uniform-test", founder.ID()); err != nil {
		t.Fatal(err)
	}

	// Request with empty PeerCertificates
	rec := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/g/uniform-test/sync/summary", nil)
	req.TLS = &tls.ConnectionState{
		HandshakeComplete: true,
		PeerCertificates:  nil,
	}
	e.hub.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("empty peer certs got %d, want 404", rec.Code)
	}

	// Verify Cache-Control: no-store header on refusal
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
}

func TestHubCrossTenantAntiEnumeration(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)

	founderA := newID(t)
	founderB := newID(t)

	if err := e.hub.AddGroup(ctx, "secure-group", founderA.ID()); err != nil {
		t.Fatal(err)
	}

	// Founder B tries to probe for "secure-group" (exists, but caller is not in it)
	// versus "phantom-group" (does not exist at all)
	codeExisting, bodyExisting := rawGet(t, e, node{id: founderB}, Prefix+"secure-group"+syncer.PathSummary)
	codePhantom, bodyPhantom := rawGet(t, e, node{id: founderB}, Prefix+"phantom-group"+syncer.PathSummary)

	// In this test, founderB has not founded any group so the handshake itself drops it,
	// or if founderB is enrolled in another group, both return identical 404 responses.
	if err := e.hub.AddGroup(ctx, "other-group", founderB.ID()); err != nil {
		t.Fatal(err)
	}

	codeExisting, bodyExisting = rawGet(t, e, node{id: founderB}, Prefix+"secure-group"+syncer.PathSummary)
	codePhantom, bodyPhantom = rawGet(t, e, node{id: founderB}, Prefix+"phantom-group"+syncer.PathSummary)

	if codeExisting != http.StatusNotFound || codePhantom != http.StatusNotFound {
		t.Fatalf("expected 404 for both, got %d and %d", codeExisting, codePhantom)
	}
	if bodyExisting != bodyPhantom {
		t.Fatalf("anti-enumeration failure: response bodies differ:\nexisting: %s\nphantom: %s", bodyExisting, bodyPhantom)
	}
}
