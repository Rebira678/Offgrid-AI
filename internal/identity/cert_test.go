package identity

import (
	"crypto"
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

var epoch = time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)

func TestACertificateCarriesTheDeviceKeyAndYieldsTheDeviceID(t *testing.T) {
	id, _ := Generate()
	c, err := id.Certificate(epoch, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	gotID, gotKey, err := DeviceOfCertificate(leaf, epoch.Add(time.Hour))
	if err != nil || gotID != id.ID() || gotKey != id.PublicKey() {
		t.Fatalf("%q %q %v", gotID, gotKey, err)
	}
	if leaf.Subject.CommonName != id.ID() {
		t.Fatalf("CN = %q", leaf.Subject.CommonName)
	}
	// It is usable as a TLS credential: the key in the handshake is the device key.
	if _, ok := c.PrivateKey.(crypto.Signer); !ok {
		t.Fatalf("private key is %T", c.PrivateKey)
	}
}

func TestFingerprintIsTheFullHashOfWhichTheDeviceIDIsAPrefix(t *testing.T) {
	id, _ := Generate()
	fp, err := Fingerprint(id.PublicKey())
	if err != nil || len(fp) != 64 || !strings.HasPrefix(fp, strings.TrimPrefix(id.ID(), "dev_")) {
		t.Fatalf("%q %v", fp, err)
	}
	if _, err := Fingerprint("nonsense"); err == nil {
		t.Fatal("accepted")
	}
}

func TestACertificateIsRefusedWhenItIsNotAnHonestDeviceCertificate(t *testing.T) {
	id, _ := Generate()
	good, _ := id.Certificate(epoch, time.Hour)
	leaf, _ := x509.ParseCertificate(good.Certificate[0])
	if _, _, err := DeviceOfCertificate(leaf, epoch.Add(2*time.Hour)); err == nil {
		t.Error("an expired certificate was accepted")
	}
	if _, _, err := DeviceOfCertificate(leaf, epoch.Add(-time.Hour)); err == nil {
		t.Error("a certificate from the future was accepted")
	}
	// Not self-signed by its own key: someone else's key signed it (a CA-style chain is not part of this design).
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: id.ID()}, NotBefore: epoch, NotAfter: epoch.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, tpl, &id.key.PublicKey, other)
	forged, _ := x509.ParseCertificate(der)
	if _, _, err := DeviceOfCertificate(forged, epoch); err == nil {
		t.Error("a certificate not signed by its own key was accepted")
	}
	// A certificate for an RSA or other-curve key is not a device certificate.
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	der, _ = x509.CreateCertificate(rand.Reader, tpl, tpl, &p384.PublicKey, p384)
	odd, _ := x509.ParseCertificate(der)
	if _, _, err := DeviceOfCertificate(odd, epoch); err == nil {
		t.Error("a P-384 certificate was accepted")
	}
	// A CA certificate is not a device certificate.
	ca := *tpl
	ca.IsCA, ca.BasicConstraintsValid = true, true
	der, _ = x509.CreateCertificate(rand.Reader, &ca, &ca, &id.key.PublicKey, id.key)
	caCert, _ := x509.ParseCertificate(der)
	if _, _, err := DeviceOfCertificate(caCert, epoch); err == nil {
		t.Error("a CA certificate was accepted")
	}
	if _, err := id.Certificate(epoch, 0); err == nil {
		t.Error("a certificate with no validity")
	}
	if _, err := id.Certificate(epoch, 400*24*time.Hour); err == nil {
		t.Error("a certificate valid for more than a year")
	}
}
