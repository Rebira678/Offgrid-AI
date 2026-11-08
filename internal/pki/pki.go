// Package pki makes the small private certificate authority a site needs to serve
// HTTPS to phones with no internet.
//
// Why a private CA at all. A phone's browser only runs a service worker (offline
// pages), WebCrypto (device keys) and the rest of the offline toolkit on a "secure
// context": HTTPS, or localhost. A public certificate needs the internet to be issued
// and renewed, and a site node has none. So the site owner makes a CA once, installs
// its certificate on each phone once, and the node serves certificates signed by it.
//
// What keeps that safe. A CA certificate installed on a phone is trusted for every
// website, so a stolen CA key could impersonate a bank. Two things limit that here:
//
//   - The CA is constrained (x509 name constraints) to private addresses and to the
//     names .local, .lan and .offgrid. A certificate it signs for example.com is
//     refused by the browser, and Issue refuses to create one.
//   - The CA key is written to its own directory with mode 0600, is never needed by
//     the node (the node needs only its own certificate and key), and is meant to
//     live on a USB stick, not on the machine that serves the phones.
//
// Only the standard library is used (crypto/x509, crypto/ecdsa, crypto/rand).
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// File names inside a CA directory.
const (
	CACertFile = "ca.crt"
	CAKeyFile  = "ca.key"
)

const (
	// DefaultCAYears: long on purpose. Replacing a CA means touching every phone again.
	DefaultCAYears = 10
	// DefaultLeafDays: a year. Apple refuses server certificates valid for more than
	// 825 days even from a user-installed CA; Chrome has no such limit for private CAs
	// but a short life limits a leaked key.
	DefaultLeafDays = 365
	// MaxLeafDays is Apple's limit for the leaf certificate.
	MaxLeafDays = 825

	// backdate: a phone or a Pi that lost its clock is often hours or days behind, and a
	// certificate "not valid yet" looks exactly like a broken one. A day of grace.
	backdate = 24 * time.Hour
)

// ErrCAExists is returned by InitCA when the directory already holds a CA. A CA is
// never overwritten: every phone that trusts the old one would stop working.
var ErrCAExists = errors.New("a certificate authority already exists in that folder")

// DefaultPermitDNS and DefaultPermitCIDR are the only names the CA may issue for.
var (
	DefaultPermitDNS  = []string{"local", "lan", "offgrid"}
	DefaultPermitCIDR = []string{
		"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", // private IPv4
		"169.254.0.0/16", "127.0.0.0/8", // link-local, loopback
		"fc00::/7", "fe80::/10", "::1/128", // private, link-local, loopback IPv6
	}
)

// CA is a loaded certificate authority. Key is nil when only the public certificate
// was loaded (LoadCACert); such a value cannot Issue.
type CA struct {
	Cert    *x509.Certificate
	Key     *ecdsa.PrivateKey
	CertPEM []byte
}

// CAOptions configures InitCA. The zero value gives a ten-year CA limited to private
// addresses and the default names.
type CAOptions struct {
	Name   string    // common name shown in the phone's certificate list
	Years  int       // validity; DefaultCAYears when zero
	Permit []string  // extra names or CIDRs the CA may issue for, beyond the defaults
	Only   []string  // if set, REPLACES the defaults (names and CIDRs): a narrower CA
	Now    time.Time // for tests; zero means now
}

// InitCA creates a CA in dir (created if missing, mode 0700) and returns it. It
// refuses to overwrite an existing one.
func InitCA(dir string, o CAOptions) (*CA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("pki: %w", err)
	}
	for _, f := range []string{CACertFile, CAKeyFile} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			return nil, fmt.Errorf("%w (%s)", ErrCAExists, dir)
		}
	}
	if o.Name == "" {
		o.Name = "Offgrid site CA"
	}
	if o.Years <= 0 {
		o.Years = DefaultCAYears
	}
	now := o.Now
	if now.IsZero() {
		now = time.Now()
	}
	dns, nets, err := permitted(o)
	if err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("pki: %w", err)
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: o.Name, Organization: []string{"Offgrid"}},
		NotBefore:             now.Add(-backdate),
		NotAfter:              now.AddDate(o.Years, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLen:            0, // it signs node certificates, never another CA
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		PermittedDNSDomains:   dns,
		PermittedIPRanges:     nets,
		// Critical: a client that does not understand the constraint must refuse the
		// certificate, not ignore the limit.
		PermittedDNSDomainsCritical: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("pki: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("pki: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM, err := encodeKey(key)
	if err != nil {
		return nil, err
	}
	// The key first, exclusively and private; the certificate (public) second.
	if err := writeNew(filepath.Join(dir, CAKeyFile), keyPEM, 0o600); err != nil {
		return nil, err
	}
	if err := writeNew(filepath.Join(dir, CACertFile), certPEM, 0o644); err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key, CertPEM: certPEM}, nil
}

func permitted(o CAOptions) (dns []string, nets []*net.IPNet, err error) {
	names, cidrs := DefaultPermitDNS, DefaultPermitCIDR
	extra := o.Permit
	if len(o.Only) > 0 {
		names, cidrs, extra = nil, nil, o.Only
	}
	dns = append(dns, names...)
	all := append([]string{}, cidrs...)
	for _, e := range extra {
		if _, _, perr := net.ParseCIDR(e); perr == nil {
			all = append(all, e)
			continue
		}
		e = strings.TrimSpace(strings.ToLower(e))
		if e == "" || strings.ContainsAny(e, " /@:*") {
			return nil, nil, fmt.Errorf("pki: %q is neither a CIDR (192.168.1.0/24) nor a domain suffix (site.example)", e)
		}
		dns = append(dns, e)
	}
	for _, c := range all {
		_, n, perr := net.ParseCIDR(c)
		if perr != nil {
			return nil, nil, fmt.Errorf("pki: %w", perr)
		}
		nets = append(nets, n)
	}
	return dns, nets, nil
}

// LoadCA loads the CA from dir, with its key. It refuses a key file that other users
// can read, a key that does not belong to the certificate, and a CA that has expired.
func LoadCA(dir string, now time.Time) (*CA, error) {
	ca, err := LoadCACert(filepath.Join(dir, CACertFile))
	if err != nil {
		return nil, err
	}
	keyPath := filepath.Join(dir, CAKeyFile)
	fi, err := os.Stat(keyPath)
	if err != nil {
		return nil, fmt.Errorf("pki: the CA key is not here (%w). It is meant to be kept offline: plug in the stick that holds %s", err, CAKeyFile)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("pki: %s is readable by other users (mode %o); run: chmod 600 %s", keyPath, fi.Mode().Perm(), keyPath)
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("pki: %w", err)
	}
	key, err := decodeKey(raw)
	if err != nil {
		return nil, fmt.Errorf("pki: %s: %w", keyPath, err)
	}
	pub, ok := ca.Cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return nil, fmt.Errorf("pki: %s does not belong to %s", CAKeyFile, CACertFile)
	}
	if !now.IsZero() && now.After(ca.Cert.NotAfter) {
		return nil, fmt.Errorf("pki: the CA expired on %s; a new CA means installing it on every phone again", ca.Cert.NotAfter.Format("2006-01-02"))
	}
	ca.Key = key
	return ca, nil
}

// LoadCACert loads only the public certificate (what a node and a phone need).
func LoadCACert(path string) (*CA, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("pki: %w", err)
	}
	cert, err := parseCertPEM(raw)
	if err != nil {
		return nil, fmt.Errorf("pki: %s: %w", path, err)
	}
	if !cert.IsCA {
		return nil, fmt.Errorf("pki: %s is not a CA certificate", path)
	}
	return &CA{Cert: cert, CertPEM: raw}, nil
}

// LeafOptions configures Issue.
type LeafOptions struct {
	Names []string  // DNS names and IP addresses the certificate is valid for; at least one
	Days  int       // validity; DefaultLeafDays when zero; at most MaxLeafDays
	Now   time.Time // for tests; zero means now
}

// Leaf is a node's certificate and key, as PEM.
type Leaf struct {
	Cert    *x509.Certificate
	CertPEM []byte
	KeyPEM  []byte
}

// Issue signs a certificate for a node. It checks its own work: the new certificate
// must verify against this CA for every requested name, so a request outside the CA's
// constraints fails here, with the reason, instead of failing later in a phone's
// browser with a generic warning.
func (ca *CA) Issue(o LeafOptions) (*Leaf, error) {
	if ca.Key == nil {
		return nil, errors.New("pki: this CA was loaded without its key and cannot sign")
	}
	if len(o.Names) == 0 {
		return nil, errors.New("pki: at least one name or IP address is required")
	}
	if o.Days <= 0 {
		o.Days = DefaultLeafDays
	}
	if o.Days > MaxLeafDays {
		return nil, fmt.Errorf("pki: %d days is too long: iPhones refuse server certificates valid for more than %d days", o.Days, MaxLeafDays)
	}
	now := o.Now
	if now.IsZero() {
		now = time.Now()
	}
	var dnsNames []string
	var ips []net.IP
	for _, n := range o.Names {
		n = strings.TrimSpace(n)
		if ip, err := netip.ParseAddr(n); err == nil {
			ips = append(ips, net.IP(ip.AsSlice()))
			continue
		}
		if n == "" || strings.ContainsAny(n, " /@:*") {
			return nil, fmt.Errorf("pki: %q is not a valid host name or IP address (wildcards are not issued)", n)
		}
		dnsNames = append(dnsNames, strings.ToLower(n))
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("pki: %w", err)
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	cn := o.Names[0]
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"Offgrid"}},
		NotBefore:             now.Add(-backdate),
		NotAfter:              now.AddDate(0, 0, o.Days),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, // IsCA false: this certificate cannot sign others
		DNSNames:              dnsNames,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return nil, fmt.Errorf("pki: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("pki: %w", err)
	}
	if err := ca.verifyFor(cert, o.Names, now); err != nil {
		return nil, err
	}
	keyPEM, err := encodeKey(key)
	if err != nil {
		return nil, err
	}
	return &Leaf{Cert: cert, CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), KeyPEM: keyPEM}, nil
}

// verifyFor reports whether cert is valid for each name under this CA at time now.
func (ca *CA) verifyFor(cert *x509.Certificate, names []string, now time.Time) error {
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	opts := x509.VerifyOptions{Roots: pool, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if _, err := cert.Verify(opts); err != nil {
		return fmt.Errorf("pki: this CA cannot issue that certificate: %w (it may only issue for private addresses and %s)", err, "."+strings.Join(ca.Cert.PermittedDNSDomains, ", ."))
	}
	for _, n := range names {
		if err := cert.VerifyHostname(strings.TrimSpace(n)); err != nil {
			return fmt.Errorf("pki: %w", err)
		}
	}
	return nil
}

// Write stores the leaf in dir as node.crt (mode 0644) and node.key (mode 0600). It
// does not overwrite: pass a fresh folder, or remove the old files first.
func (l *Leaf) Write(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("pki: %w", err)
	}
	if err := writeNew(filepath.Join(dir, "node.key"), l.KeyPEM, 0o600); err != nil {
		return err
	}
	return writeNew(filepath.Join(dir, "node.crt"), l.CertPEM, 0o644)
}

// Fingerprint is the SHA-256 of the certificate, as colon-separated hex: what to read
// aloud or compare when installing the CA on a phone.
func Fingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	parts := make([]string, 0, len(h)/2)
	for i := 0; i < len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":")
}

// Describe is a short, human-readable summary of a certificate.
func Describe(c *x509.Certificate, now time.Time) string {
	var b strings.Builder
	kind := "node certificate"
	if c.IsCA {
		kind = "certificate authority"
	}
	fmt.Fprintf(&b, "%s: %s\n", kind, c.Subject.CommonName)
	fmt.Fprintf(&b, "  valid        %s to %s (%s)\n", c.NotBefore.Format("2006-01-02"), c.NotAfter.Format("2006-01-02"), remaining(c.NotAfter, now))
	if names := sans(c); len(names) > 0 {
		fmt.Fprintf(&b, "  names        %s\n", strings.Join(names, ", "))
	}
	if c.IsCA {
		fmt.Fprintf(&b, "  may issue    %s\n", strings.Join(constraints(c), ", "))
	}
	fmt.Fprintf(&b, "  fingerprint  %s", Fingerprint(c))
	return b.String()
}

func sans(c *x509.Certificate) []string {
	out := append([]string{}, c.DNSNames...)
	for _, ip := range c.IPAddresses {
		out = append(out, ip.String())
	}
	return out
}

func constraints(c *x509.Certificate) []string {
	var out []string
	for _, d := range c.PermittedDNSDomains {
		out = append(out, "."+d)
	}
	for _, n := range c.PermittedIPRanges {
		out = append(out, n.String())
	}
	if len(out) == 0 {
		out = []string{"ANYTHING (no name constraints)"}
	}
	return out
}

func remaining(notAfter, now time.Time) string {
	if now.IsZero() {
		now = time.Now()
	}
	d := notAfter.Sub(now)
	if d < 0 {
		return fmt.Sprintf("EXPIRED %d days ago", int(-d.Hours()/24))
	}
	return fmt.Sprintf("%d days left", int(d.Hours()/24))
}

// ---- helpers ----------------------------------------------------------------

func newSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 127) // positive, 127 random bits
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("pki: %w", err)
	}
	return n.Add(n, big.NewInt(1)), nil
}

func encodeKey(k *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, fmt.Errorf("pki: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func decodeKey(raw []byte) (*ecdsa.PrivateKey, error) {
	blk, _ := pem.Decode(raw)
	if blk == nil || blk.Type != "PRIVATE KEY" {
		return nil, errors.New("not a PEM private key")
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	ec, ok := k.(*ecdsa.PrivateKey)
	if !ok || ec.Curve != elliptic.P256() {
		return nil, errors.New("not an ECDSA P-256 key")
	}
	return ec, nil
}

// LoadKeyPair is for the node: it reads node.crt and node.key, and refuses a key
// that other users can read.
func LoadKeyPair(certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	fi, err := os.Stat(keyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("pki: %w", err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return nil, nil, fmt.Errorf("pki: %s is readable by other users (mode %o); run: chmod 600 %s", keyPath, fi.Mode().Perm(), keyPath)
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("pki: %w", err)
	}
	key, err := decodeKey(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("pki: %s: %w", keyPath, err)
	}
	cpem, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, fmt.Errorf("pki: %w", err)
	}
	cert, err := parseCertPEM(cpem)
	if err != nil {
		return nil, nil, fmt.Errorf("pki: %s: %w", certPath, err)
	}
	if pub, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok || !pub.Equal(&key.PublicKey) {
		return nil, nil, fmt.Errorf("pki: %s does not belong to %s", keyPath, certPath)
	}
	return cert, key, nil
}

// ParseCertFile reads one PEM certificate.
func ParseCertFile(path string) (*x509.Certificate, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("pki: %w", err)
	}
	c, err := parseCertPEM(raw)
	if err != nil {
		return nil, fmt.Errorf("pki: %s: %w", path, err)
	}
	return c, nil
}

func parseCertPEM(raw []byte) (*x509.Certificate, error) {
	blk, _ := pem.Decode(raw)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, errors.New("not a PEM certificate")
	}
	return x509.ParseCertificate(blk.Bytes)
}

// writeNew creates a file that must not exist, with the given mode, and syncs it.
func writeNew(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("pki: %s already exists and is not overwritten", path)
		}
		return fmt.Errorf("pki: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("pki: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("pki: %w", err)
	}
	return f.Close()
}
