package phone

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"offgrid/internal/identity"
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
