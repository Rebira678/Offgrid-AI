package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// Day 40: certificates built from device keys.
//
// A device certificate is self-signed with the device key. There is no CA and no
// chain. A peer does not trust a certificate because someone signed it; it
// trusts it because its public key is the one the membership log names for a
// device (pinning). The certificate is only the envelope TLS needs to carry the
// key. Its validity period is short and cheap to renew, and it is not what
// decides who is a member: the log is.

// MaxCertValidity is the longest validity a device certificate may have.
const MaxCertValidity = 366 * 24 * time.Hour

// Certificate makes a TLS credential for this device, valid from now (backdated
// five minutes, for clocks that disagree) for validity.
func (i *Identity) Certificate(now time.Time, validity time.Duration) (tls.Certificate, error) {
	if validity <= 0 || validity > MaxCertValidity {
		return tls.Certificate{}, errors.New("identity: certificate validity must be between 1 ns and a year")
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		return tls.Certificate{}, err
	}
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: i.id},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &i.key.PublicKey, i.key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("identity: certificate: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: i.key}, nil
}

// DeviceOfCertificate returns the device ID and tagged public key of a device
// certificate, or an error if it is not one: it must be valid now, a non-CA
// certificate for an ECDSA P-256 key, and signed by that same key.
func DeviceOfCertificate(c *x509.Certificate, now time.Time) (id, tagged string, err error) {
	switch {
	case now.Before(c.NotBefore):
		return "", "", errors.New("identity: the certificate is not valid yet")
	case now.After(c.NotAfter):
		return "", "", errors.New("identity: the certificate has expired")
	case c.IsCA:
		return "", "", errors.New("identity: a CA certificate is not a device certificate")
	}
	pk, ok := c.PublicKey.(*ecdsa.PublicKey)
	if !ok || pk.Curve != elliptic.P256() {
		return "", "", errors.New("identity: the certificate's key is not ECDSA P-256")
	}
	if err := c.CheckSignature(c.SignatureAlgorithm, c.RawTBSCertificate, c.Signature); err != nil {
		return "", "", errors.New("identity: the certificate is not signed by its own key")
	}
	e, err := pk.ECDH()
	if err != nil {
		return "", "", err
	}
	raw := e.Bytes()
	i := &Identity{pub: raw, id: deviceID(raw)}
	return i.id, i.PublicKey(), nil
}

// Fingerprint is the full SHA-256 of the 65-byte public key, in hex: 64
// characters, of which the device ID is the first 32. It is what an invite
// carries so a new device can pin the node it enrolls with.
func Fingerprint(tagged string) (string, error) {
	_, raw, err := parsePublic(tagged)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:]), nil
}
