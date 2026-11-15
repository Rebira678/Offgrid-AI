package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"strings"
	"testing"
	"time"
)

func TestCertEdgeValidityBoundaryTimestamps(t *testing.T) {
	id, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	validity := 24 * time.Hour

	cert, err := id.Certificate(now, validity)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}

	// 1. Exactly at NotBefore (now - 5m) should be valid
	if _, _, err := DeviceOfCertificate(leaf, leaf.NotBefore); err != nil {
		t.Errorf("expected valid at exactly NotBefore: %v", err)
	}

	// 2. 1 millisecond before NotBefore must fail
	if _, _, err := DeviceOfCertificate(leaf, leaf.NotBefore.Add(-time.Millisecond)); err == nil {
		t.Error("accepted timestamp before NotBefore")
	}

	// 3. Exactly at NotAfter should be valid
	if _, _, err := DeviceOfCertificate(leaf, leaf.NotAfter); err != nil {
		t.Errorf("expected valid at exactly NotAfter: %v", err)
	}

	// 4. 1 millisecond after NotAfter must fail
	if _, _, err := DeviceOfCertificate(leaf, leaf.NotAfter.Add(time.Millisecond)); err == nil {
		t.Error("accepted timestamp after NotAfter")
	}
}

func TestCertEdgeTamperedSignatureFailsVerification(t *testing.T) {
	id, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

	cert, err := id.Certificate(now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}

	// Tamper with signature bytes
	if len(leaf.Signature) > 5 {
		leaf.Signature[4] ^= 0xff
		if _, _, err := DeviceOfCertificate(leaf, now); err == nil || !strings.Contains(err.Error(), "not signed by its own key") {
			t.Errorf("expected signature verification failure for tampered cert, got: %v", err)
		}
	}
}

func TestCertEdgeSerialNumberUniqueness(t *testing.T) {
	id, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	serials := make(map[string]bool)

	for i := 0; i < 50; i++ {
		cert, err := id.Certificate(now, time.Hour)
		if err != nil {
			t.Fatalf("mint certificate %d: %v", i, err)
		}
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			t.Fatalf("parse certificate %d: %v", i, err)
		}
		s := leaf.SerialNumber.String()
		if serials[s] {
			t.Fatalf("duplicate serial number generated: %s", s)
		}
		serials[s] = true
	}
}

func TestCertEdgeNonP256CurvesRejected(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

	// Curve P-521
	p521, err := ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(42),
		Subject:      pkix.Name{CommonName: "dev_fake"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &p521.PublicKey, p521)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := DeviceOfCertificate(parsed, now); err == nil || !strings.Contains(err.Error(), "P-256") {
		t.Errorf("expected P-256 error for P-521 cert, got: %v", err)
	}
}

func TestFingerprintEdgeCases(t *testing.T) {
	testCases := []struct {
		name    string
		pubKey  string
		wantErr bool
	}{
		{"Empty string", "", true},
		{"Invalid base64 characters", "invalid!!!base64", true},
		{"Valid base64 but wrong length", "AAAA", true},
		{"Valid base64 but 32 bytes (raw seed, untagged)", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", true},
		{"Truncated string", "og-pk-1-short", true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fp, err := Fingerprint(tc.pubKey)
			if (err != nil) != tc.wantErr {
				t.Errorf("Fingerprint(%q) err = %v, wantErr = %v (fp=%s)", tc.pubKey, err, tc.wantErr, fp)
			}
		})
	}
}
