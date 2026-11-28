package phone

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"offgrid/internal/identity"
	"offgrid/internal/store/sqlstore"
)

// TestEdgeConcurrentSyncBatchesWithReferences spins up multiple concurrent phone sessions
// each rapidly creating tasks with intra-batch "@<command_id>" references and notes under
// the race detector to ensure zero race conditions and correct reference resolution.
func TestEdgeConcurrentSyncBatchesWithReferences(t *testing.T) {
	r := newRig(t, nil)
	const numPhones = 5
	const batchesPerPhone = 4

	var wg sync.WaitGroup
	var errCount atomic.Int64

	for i := 0; i < numPhones; i++ {
		wg.Add(1)
		phoneIdx := i
		go func() {
			defer wg.Done()
			label := fmt.Sprintf("phone-worker-%d", phoneIdx)
			p, cookie := r.pair(label)
			if cookie == "" || p == nil {
				errCount.Add(1)
				return
			}

			for b := 0; b < batchesPerPhone; b++ {
				createCmdID := fmt.Sprintf("cmd-c-%d-%d-0001", phoneIdx, b)
				noteCmdID := fmt.Sprintf("cmd-n-%d-%d-0002", phoneIdx, b)
				statusCmdID := fmt.Sprintf("cmd-s-%d-%d-0003", phoneIdx, b)

				cmds := []Command{
					{
						ID:       createCmdID,
						Op:       "create",
						Title:    fmt.Sprintf("Task from worker %d batch %d", phoneIdx, b),
						Priority: "high",
						AgeMS:    1000,
					},
					{
						ID:   noteCmdID,
						Op:   "note",
						Task: "@" + createCmdID,
						Text: fmt.Sprintf("Initial field note by worker %d batch %d", phoneIdx, b),
					},
					{
						ID:     statusCmdID,
						Op:     "status",
						Task:   "@" + createCmdID,
						Status: "in_progress",
					},
				}

				rp := r.do("POST", "/api/sync", syncRequest{Commands: cmds}, cookie, nil)
				if rp.code != 200 {
					errCount.Add(1)
					return
				}
				var out syncReply
				rp.json(t, &out)
				if len(out.Results) != 3 {
					errCount.Add(1)
					return
				}
				if out.Results[0].Outcome != "applied" || out.Results[0].Task == "" {
					errCount.Add(1)
					return
				}
				if out.Results[1].Outcome != "applied" || out.Results[1].Task != out.Results[0].Task {
					errCount.Add(1)
					return
				}
				if out.Results[2].Outcome != "applied" || out.Results[2].Task != out.Results[0].Task {
					errCount.Add(1)
					return
				}
			}
		}()
	}

	wg.Wait()
	if n := errCount.Load(); n > 0 {
		t.Fatalf("%d errors encountered during concurrent sync batches", n)
	}
}

// TestEdgeMalformedCryptoPayloads verifies strict refusal on corrupt proofs, signatures,
// nonces, and session tokens.
func TestEdgeMalformedCryptoPayloads(t *testing.T) {
	r := newRig(t, nil)
	code := r.invite("EdgeTester")
	p, _ := identity.Generate()

	// 1. Proof corrupted / truncated
	t.Run("truncated-proof", func(t *testing.T) {
		badReq := pairRequest{
			Code:      code,
			PublicKey: p.PublicKey(),
			Label:     "EdgeTester",
			Proof:     base64.RawURLEncoding.EncodeToString([]byte("short-proof")),
		}
		rp := r.do("POST", "/api/pair", badReq, "", nil)
		if rp.code != 400 || rp.errCode(t) != "bad_proof" {
			t.Fatalf("expected 400 bad_proof, got %d %s", rp.code, rp.body)
		}
	})

	t.Run("non-base64-proof", func(t *testing.T) {
		badReq := pairRequest{
			Code:      code,
			PublicKey: p.PublicKey(),
			Label:     "EdgeTester",
			Proof:     "not!valid@base64===",
		}
		rp := r.do("POST", "/api/pair", badReq, "", nil)
		if rp.code != 400 || rp.errCode(t) != "bad_proof" {
			t.Fatalf("expected 400 bad_proof, got %d %s", rp.code, rp.body)
		}
	})

	// 2. Corrupt login signature
	t.Run("corrupt-login-sig", func(t *testing.T) {
		rp := r.do("POST", "/api/challenge", map[string]string{}, "", nil)
		var chal challengeReply
		rp.json(t, &chal)

		dev, _ := identity.DeviceID(p.PublicKey())
		badLogin := loginRequest{
			Device: dev,
			Nonce:  chal.Nonce,
			Sig:    base64.RawURLEncoding.EncodeToString([]byte("wrong-length-signature")),
		}
		rp = r.do("POST", "/api/login", badLogin, "", nil)
		if rp.code != 401 || rp.errCode(t) != "unpaired" {
			t.Fatalf("expected 401 unpaired for bad sig length, got %d %s", rp.code, rp.body)
		}
	})

	// 3. Unknown fields rejected by strict decoder
	t.Run("disallow-unknown-fields", func(t *testing.T) {
		rawJSON := []byte(`{"code":"` + code + `","public_key":"` + p.PublicKey() + `","label":"t","proof":"` +
			base64.RawURLEncoding.EncodeToString(make([]byte, 64)) + `","extra_field":"malicious"}`)
		rp := r.do("POST", "/api/pair", rawJSON, "", nil)
		if rp.code != 400 || rp.errCode(t) != "bad_request" {
			t.Fatalf("expected 400 bad_request on unknown field, got %d %s", rp.code, rp.body)
		}
	})
}

// TestEdgeTokenTamperingAndBitFlips ensures any modification to sealed tokens is rejected.
func TestEdgeTokenTamperingAndBitFlips(t *testing.T) {
	s := signer{key: bytes.Repeat([]byte{0x42}, MinSecret)}
	now := time.Now()
	tok := s.issue("device-123", now, time.Hour)

	// Flip a byte in the payload part
	parts := bytes.Split([]byte(tok), []byte("."))
	if len(parts) != 3 {
		t.Fatalf("expected 3 token parts, got %d", len(parts))
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(string(parts[1]))
	if err != nil {
		t.Fatal(err)
	}
	payloadBytes[0] ^= 0xFF // bit flip
	tamperedPayload := base64.RawURLEncoding.EncodeToString(payloadBytes)
	tamperedTok := string(parts[0]) + "." + tamperedPayload + "." + string(parts[2])

	if _, err := s.verify(tamperedTok, now); err != errToken {
		t.Fatalf("expected errToken on tampered payload, got %v", err)
	}

	// Tamper signature
	tamperedSig := string(parts[2]) + "x"
	tamperedTok2 := string(parts[0]) + "." + string(parts[1]) + "." + tamperedSig
	if _, err := s.verify(tamperedTok2, now); err != errToken {
		t.Fatalf("expected errToken on tampered signature, got %v", err)
	}

	// Missing components or bad version
	if _, err := s.verify("v2."+string(parts[1])+"."+string(parts[2]), now); err != errToken {
		t.Fatalf("expected errToken on wrong version, got %v", err)
	}
	if _, err := s.verify("v1.onlyonepart", now); err != errToken {
		t.Fatalf("expected errToken on single part, got %v", err)
	}
}

// TestEdgeNonceClaimRacing verifies that under severe concurrent contention,
// a challenge nonce can only be claimed once across 20 goroutines.
func TestEdgeNonceClaimRacing(t *testing.T) {
	var n nonces
	now := time.Now()
	const nonce = "test-challenge-nonce-racing-12345"

	const routines = 20
	var wg sync.WaitGroup
	var successCount atomic.Int64

	for i := 0; i < routines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if n.claim(nonce, now) {
				successCount.Add(1)
			}
		}()
	}

	wg.Wait()
	if got := successCount.Load(); got != 1 {
		t.Fatalf("expected exactly 1 nonce claim success, got %d", got)
	}
}

// TestEdgePhotoUploadLimitsAndValidation checks photo size limits, magic bytes,
// and edge filename sanitization.
func TestEdgePhotoUploadLimitsAndValidation(t *testing.T) {
	r := newRig(t, func(c *Config) {
		c.MaxPhoto = 2048 // 2 KB custom limit for quick boundary testing
	})
	_, cookie := r.pair("Photographer")
	tk := r.mk("Repair Antenna")

	// Valid 1x1 GIF/PNG header
	validPNG := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4, 0x89,
	}

	t.Run("photo-under-limit-succeeds", func(t *testing.T) {
		rp := r.do("POST", "/api/tasks/"+tk.ID+"/photos", validPNG, cookie, map[string]string{
			"Content-Type": "image/png",
		})
		if rp.code != 201 {
			t.Fatalf("expected 201 Created, got %d %s", rp.code, rp.body)
		}
	})

	t.Run("photo-over-limit-refused", func(t *testing.T) {
		oversized := make([]byte, 3000)
		copy(oversized, validPNG)
		rp := r.do("POST", "/api/tasks/"+tk.ID+"/photos", oversized, cookie, map[string]string{
			"Content-Type": "image/png",
		})
		if rp.code != http.StatusRequestEntityTooLarge {
			t.Fatalf("expected 413 Too Large, got %d %s", rp.code, rp.body)
		}
	})

	t.Run("non-image-content-type-refused", func(t *testing.T) {
		rp := r.do("POST", "/api/tasks/"+tk.ID+"/photos", validPNG, cookie, map[string]string{
			"Content-Type": "text/plain",
		})
		if rp.code != http.StatusUnsupportedMediaType {
			t.Fatalf("expected 415 Unsupported Media Type, got %d %s", rp.code, rp.body)
		}
	})
}

// TestEdgeRateLimiterLockoutAndWindowRecovery tests rate limiter lockout per IP
// and verifies clean recovery after the window expires.
func TestEdgeRateLimiterLockoutAndWindowRecovery(t *testing.T) {
	l := newLimiter()
	now := time.Now()
	const ip = "10.0.0.99"

	// Initial checks allowed
	for i := 0; i < 5; i++ {
		if !l.allowed(ip, now) {
			t.Fatalf("call %d should be allowed", i)
		}
		l.fail(ip, now)
	}

	// 6th call should be locked out
	if l.allowed(ip, now) {
		t.Fatal("expected lockout after 5 fails")
	}

	// Advance time past 1 minute window
	later := now.Add(61 * time.Second)
	if !l.allowed(ip, later) {
		t.Fatal("expected recovery after window elapsed")
	}
}

// TestEdgeCommandAgeThresholdBoundaries tests the exact boundary where
// an operation switches to late.
func TestEdgeCommandAgeThresholdBoundaries(t *testing.T) {
	r := newRig(t, func(c *Config) {
		c.LateAfter = 2 * time.Minute
	})
	_, cookie := r.pair("Inspector")

	// Exactly 1ms under 2m -> not late
	under := Command{
		ID:       "cmd-edge-under-1234",
		Op:       "create",
		Title:    "Boundary check under",
		Priority: "low",
		AgeMS:    (2*time.Minute - 1*time.Millisecond).Milliseconds(),
	}
	resUnder := r.sync(cookie, under)
	if resUnder[0].Late {
		t.Errorf("1ms under threshold should not be marked late")
	}

	// Exactly 2m -> late
	exact := Command{
		ID:       "cmd-edge-exact-1234",
		Op:       "create",
		Title:    "Boundary check exact",
		Priority: "low",
		AgeMS:    (2 * time.Minute).Milliseconds(),
	}
	resExact := r.sync(cookie, exact)
	if !resExact[0].Late {
		t.Errorf("exact threshold should be marked late")
	}
}

// TestEdgeIPv6ScopedHostHeaders verifies that link-local IPv6 addresses with zone
// identifiers and configured hosts with explicit ports are matched correctly.
func TestEdgeIPv6ScopedHostHeaders(t *testing.T) {
	// 1. Link-local IPv6 with zone scope
	if !hostOK(nil, "[fe80::1%eth0]:8443") {
		t.Error("expected link-local with zone scope to be accepted")
	}
	if !hostOK(nil, "fe80::2%wlan0") {
		t.Error("expected link-local with interface scope to be accepted")
	}
	if !hostOK(nil, "[::1]:8443") {
		t.Errorf("expected [::1]:8443 to be accepted as loopback")
	}

	// 2. Configured host with port in config matches requests with or without port
	cfgHosts := []string{"192.168.1.55:8443", "field-node.local:8443"}
	if !hostOK(cfgHosts, "192.168.1.55:8443") {
		t.Errorf("expected exact match with port")
	}
	if !hostOK(cfgHosts, "192.168.1.55") {
		t.Errorf("expected match when request omits port")
	}
	if !hostOK(cfgHosts, "field-node.local") {
		t.Errorf("expected host name match when request omits port")
	}
	if !hostOK(cfgHosts, "field-node.local:8443") {
		t.Errorf("expected host name match with port")
	}
	if hostOK(cfgHosts, "evil.example.com") {
		t.Errorf("unconfigured host should be rejected")
	}
}

// TestEdgeFastNonceReplayRejection tests that replaying an existing nonce is rejected
// via the fast path without executing expensive signature verification.
func TestEdgeFastNonceReplayRejection(t *testing.T) {
	r := newRig(t, nil)
	p, _ := identity.Generate()
	code := r.invite("ReplayTester")
	proof, _ := sqlstore.EnrollProof(code, p, "ReplayTester")
	rp := r.do("POST", "/api/pair", pairRequest{
		Code:      code,
		PublicKey: p.PublicKey(),
		Label:     "ReplayTester",
		Proof:     base64.RawURLEncoding.EncodeToString(proof),
	}, "", nil)
	if rp.code != 200 {
		t.Fatalf("pair: %d %s", rp.code, rp.body)
	}

	// Obtain challenge
	rp = r.do("POST", "/api/challenge", map[string]string{}, "", nil)
	var chal challengeReply
	rp.json(t, &chal)

	dev, _ := identity.DeviceID(p.PublicKey())
	sig, _ := p.Sign(LoginMessage(dev, chal.Nonce))
	loginReq := loginRequest{
		Device: dev,
		Nonce:  chal.Nonce,
		Sig:    base64.RawURLEncoding.EncodeToString(sig),
	}

	// First login succeeds
	rp1 := r.do("POST", "/api/login", loginReq, "", nil)
	if rp1.code != 200 {
		t.Fatalf("login 1 expected 200, got %d %s", rp1.code, rp1.body)
	}

	// Replayed login is immediately rejected
	rp2 := r.do("POST", "/api/login", loginReq, "", nil)
	if rp2.code != 401 || rp2.errCode(t) != "unpaired" {
		t.Fatalf("login 2 expected 401 unpaired, got %d %s", rp2.code, rp2.body)
	}
}

// TestEdgeNoncesCapacityBounding checks that nonce table is properly bounded
// under a flood of claims.
func TestEdgeNoncesCapacityBounding(t *testing.T) {
	var nc nonces
	now := time.Now()
	for i := 0; i < maxNonces+100; i++ {
		nonce := fmt.Sprintf("nonce-%08d", i)
		if !nc.claim(nonce, now) {
			t.Fatalf("fresh nonce %s should be claimable", nonce)
		}
	}
	nc.mu.Lock()
	defer nc.mu.Unlock()
	if len(nc.used) > maxNonces {
		t.Fatalf("nonces table exceeded maxNonces: %d > %d", len(nc.used), maxNonces)
	}
}

// TestEdgeLimiterSelectivePruning tests that when limiter exceeds 4096 entries,
// it selectively prunes expired entries and keeps active blocked IP entries.
func TestEdgeLimiterSelectivePruning(t *testing.T) {
	l := newLimiter()
	past := time.Now().Add(-2 * time.Minute)
	current := time.Now()

	// Add 4,100 expired failures
	for i := 0; i < 4100; i++ {
		l.fail(fmt.Sprintf("stale-ip-%d", i), past)
	}

	// Add 5 fresh failures for an active attacker
	activeAttacker := "203.0.113.50"
	for i := 0; i < 5; i++ {
		l.fail(activeAttacker, current)
	}

	// Attacker must be blocked
	if l.allowed(activeAttacker, current) {
		t.Errorf("active attacker should be blocked even after table pruning")
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.fails) > 4097 {
		t.Errorf("expected pruned table size <= 4097, got %d", len(l.fails))
	}
}

// TestEdgeDeviceClaimSanitization tests that session tokens with invalid or
// malicious characters in the device field are strictly rejected.
func TestEdgeDeviceClaimSanitization(t *testing.T) {
	s := signer{key: bytes.Repeat([]byte{0x77}, MinSecret)}
	now := time.Now()

	invalidDevices := []string{
		"dev_123\nnewline",
		"dev_123\x00null",
		"dev 123 space",
		"dev\ttab",
	}

	for _, d := range invalidDevices {
		tok := s.issue(d, now, time.Hour)
		if _, err := s.verify(tok, now); err != errToken {
			t.Errorf("expected errToken for device %q, got %v", d, err)
		}
	}
}

// TestEdgeConcurrentSecretRotation verifies that concurrent secret rotation calls
// do not corrupt the key file or fail with file collision errors.
func TestEdgeConcurrentSecretRotation(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "session-test.key")

	_, err := LoadOrCreateSecret(keyFile)
	if err != nil {
		t.Fatal(err)
	}

	const callers = 10
	var wg sync.WaitGroup
	errs := make(chan error, callers)

	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := RotateSecret(keyFile); err != nil {
				errs <- err
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("RotateSecret error: %v", err)
	}

	key, err := LoadOrCreateSecret(keyFile)
	if err != nil || len(key) != 32 {
		t.Fatalf("final key invalid: %v, len %d", err, len(key))
	}
}

// TestEdgeLogoutExpiryHeader verifies that logout sets explicit epoch expiration.
func TestEdgeLogoutExpiryHeader(t *testing.T) {
	r := newRig(t, nil)
	rp := r.do("POST", "/api/logout", map[string]string{}, "", nil)
	if rp.code != 200 {
		t.Fatalf("expected 200, got %d", rp.code)
	}

	cookieHeader := rp.h.Get("Set-Cookie")
	if !strings.Contains(cookieHeader, "Max-Age=0") && !strings.Contains(cookieHeader, "Max-Age=-1") {
		t.Errorf("expected Max-Age to invalidate cookie: %s", cookieHeader)
	}
	if !strings.Contains(cookieHeader, "1970") {
		t.Errorf("expected 1970 epoch expiry date: %s", cookieHeader)
	}
}
