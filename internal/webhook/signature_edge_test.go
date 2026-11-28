package webhook

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWebhookSignatureAndToleranceEdges(t *testing.T) {
	secret := "test-secret-with-unicode-⚡-and-symbols!@#$"
	body := []byte(`{"event":"task.created","task":"T1.site"}`)
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	tsStr := strconv.FormatInt(now.Unix(), 10)
	sig := Sign(secret, now, body)

	// Valid exact signature and timestamp
	if err := Verify(secret, sig, tsStr, body, now); err != nil {
		t.Fatalf("valid signature failed verification: %v", err)
	}

	// Exact tolerance boundaries
	if err := Verify(secret, sig, tsStr, body, now.Add(Tolerance)); err != nil {
		t.Fatalf("exact positive tolerance failed: %v", err)
	}
	if err := Verify(secret, sig, tsStr, body, now.Add(-Tolerance)); err != nil {
		t.Fatalf("exact negative tolerance failed: %v", err)
	}

	// Just outside tolerance (1ms beyond 5m)
	if err := Verify(secret, sig, tsStr, body, now.Add(Tolerance+time.Second)); err == nil {
		t.Fatal("expected failure for timestamp past tolerance")
	}
	if err := Verify(secret, sig, tsStr, body, now.Add(-Tolerance-time.Second)); err == nil {
		t.Fatal("expected failure for timestamp before negative tolerance")
	}

	tamperedSig := sig[:len(sig)-1]
	if sig[len(sig)-1] == 'a' {
		tamperedSig += "b"
	} else {
		tamperedSig += "a"
	}

	tamperedCases := []struct {
		name string
		sig  string
	}{
		{"wrong prefix v0", "v0=" + sig[3:]},
		{"wrong prefix v2", "v2=" + sig[3:]},
		{"missing prefix", sig[3:]},
		{"empty string", ""},
		{"invalid hex characters", "v1=" + strings.Repeat("z", 64)},
		{"odd length hex", "v1=" + sig[3:66]}, // 63 chars instead of 64
		{"tampered trailing character", tamperedSig},
	}

	for _, tc := range tamperedCases {
		t.Run(tc.name, func(t *testing.T) {
			if err := Verify(secret, tc.sig, tsStr, body, now); err == nil {
				t.Fatalf("expected verification failure for %s", tc.name)
			}
		})
	}

	// Binary/raw byte secrets
	rawSecret := string([]byte{0x00, 0x01, 0xFE, 0xFF, 0x42})
	rawSig := Sign(rawSecret, now, body)
	if err := Verify(rawSecret, rawSig, tsStr, body, now); err != nil {
		t.Fatalf("binary secret verification failed: %v", err)
	}
}

func TestKeyLengthVariationsAndEmptyPayload(t *testing.T) {
	now := time.Now().UTC()
	ts := strconv.FormatInt(now.Unix(), 10)

	// 1. Empty payload
	emptyBody := []byte{}
	sig := Sign("secret", now, emptyBody)
	if err := Verify("secret", sig, ts, emptyBody, now); err != nil {
		t.Fatalf("empty body failed verification: %v", err)
	}

	// 2. Secret lengths: 1-byte, 1024-byte
	shortSecret := "x"
	sigShort := Sign(shortSecret, now, []byte("payload"))
	if err := Verify(shortSecret, sigShort, ts, []byte("payload"), now); err != nil {
		t.Fatalf("1-byte secret failed verification: %v", err)
	}

	longSecret := strings.Repeat("a", 1024)
	sigLong := Sign(longSecret, now, []byte("payload"))
	if err := Verify(longSecret, sigLong, ts, []byte("payload"), now); err != nil {
		t.Fatalf("1024-byte secret failed verification: %v", err)
	}
}
