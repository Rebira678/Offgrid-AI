package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

func newCA(t *testing.T, o CAOptions) (*CA, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ca")
	o.Now = now
	ca, err := InitCA(dir, o)
	if err != nil {
		t.Fatal(err)
	}
	return ca, dir
}

func pool(ca *CA) *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.Cert)
	return p
}

// verify is what a phone's browser does: chain to the installed CA, valid at that moment, for that name.
func verify(c *x509.Certificate, ca *CA, name string, at time.Time) error {
	_, err := c.Verify(x509.VerifyOptions{Roots: pool(ca), CurrentTime: at, DNSName: name, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	return err
}

func TestInitCAMakesAConstrainedPrivateCA(t *testing.T) {
	ca, dir := newCA(t, CAOptions{})
	c := ca.Cert
	if !c.IsCA || !c.BasicConstraintsValid || c.MaxPathLen != 0 || !c.MaxPathLenZero {
		t.Errorf("CA flags: IsCA=%v pathlen=%d zero=%v", c.IsCA, c.MaxPathLen, c.MaxPathLenZero)
	}
	if c.KeyUsage != x509.KeyUsageCertSign|x509.KeyUsageCRLSign {
		t.Errorf("key usage %v", c.KeyUsage)
	}
	if k, ok := c.PublicKey.(*ecdsa.PublicKey); !ok || k.Curve != elliptic.P256() {
		t.Errorf("CA key is not ECDSA P-256")
	}
	if !c.PermittedDNSDomainsCritical || len(c.PermittedDNSDomains) == 0 || len(c.PermittedIPRanges) == 0 {
		t.Errorf("name constraints missing or not critical: %v %v critical=%v", c.PermittedDNSDomains, c.PermittedIPRanges, c.PermittedDNSDomainsCritical)
	}
	if !c.NotBefore.Before(now) || !c.NotAfter.After(now.AddDate(9, 0, 0)) {
		t.Errorf("validity %v to %v", c.NotBefore, c.NotAfter)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(filepath.Join(dir, CAKeyFile))
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("ca.key mode %o, want 600", fi.Mode().Perm())
		}
		di, _ := os.Stat(dir)
		if di.Mode().Perm()&0o077 != 0 {
			t.Errorf("CA folder mode %o lets others in", di.Mode().Perm())
		}
	}
	if _, err := LoadCA(dir, now); err != nil {
		t.Errorf("reload: %v", err)
	}
}

func TestInitCANeverOverwritesAnExistingCA(t *testing.T) {
	_, dir := newCA(t, CAOptions{})
	before, _ := os.ReadFile(filepath.Join(dir, CAKeyFile))
	_, err := InitCA(dir, CAOptions{Now: now})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error = %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, CAKeyFile))
	if string(before) != string(after) {
		t.Error("the CA key was overwritten")
	}
}

func TestIssuedCertificateVerifiesLikeABrowserWould(t *testing.T) {
	ca, _ := newCA(t, CAOptions{})
	leaf, err := ca.Issue(LeafOptions{Names: []string{"10.240.72.88", "offgrid.local", "FieldNode.LAN"}, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	c := leaf.Cert
	for _, name := range []string{"offgrid.local", "fieldnode.lan", "10.240.72.88"} {
		if err := verify(c, ca, name, now); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := verify(c, ca, "other.local", now); err == nil {
		t.Error("valid for a name it was not issued for")
	}
	if c.IsCA {
		t.Error("a node certificate must not be a CA")
	}
	if len(c.ExtKeyUsage) != 1 || c.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth || c.KeyUsage != x509.KeyUsageDigitalSignature {
		t.Errorf("usages: %v %v", c.ExtKeyUsage, c.KeyUsage)
	}
	if days := c.NotAfter.Sub(now).Hours() / 24; days < 364 || days > 366 {
		t.Errorf("valid for %.1f more days, want about %d", days, DefaultLeafDays)
	}
	if k, ok := c.PublicKey.(*ecdsa.PublicKey); !ok || k.Curve != elliptic.P256() {
		t.Error("leaf key is not ECDSA P-256")
	}
}

func TestACertificateForAPublicSiteIsRefused(t *testing.T) {
	ca, _ := newCA(t, CAOptions{})
	for _, name := range []string{"example.com", "mail.google.com", "8.8.8.8", "1.1.1.1", "evil.local.example.com"} {
		if _, err := ca.Issue(LeafOptions{Names: []string{name}, Now: now}); err == nil {
			t.Errorf("issued a certificate for %q", name)
		}
	}
	// And a browser would refuse one even if someone forged it with a stolen CA key.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	forged, _ := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "bank.example"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"bank.example"},
	}, ca.Cert, &key.PublicKey, ca.Key)
	fc, err := x509.ParseCertificate(forged)
	if err != nil {
		t.Fatal(err)
	}
	if err := verify(fc, ca, "bank.example", now); err == nil {
		t.Error("a certificate for bank.example verified under the site CA: the name constraints do not bite")
	}
}

func TestANodeCertificateCannotSignOtherCertificates(t *testing.T) {
	ca, _ := newCA(t, CAOptions{})
	leaf, _ := ca.Issue(LeafOptions{Names: []string{"a.local"}, Now: now})
	leafKey, err := decodeKey(leaf.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(9), Subject: pkix.Name{CommonName: "b.local"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: []string{"b.local"},
	}, leaf.Cert, &key.PublicKey, leafKey)
	if err != nil {
		t.Fatal(err)
	}
	child, _ := x509.ParseCertificate(der)
	inter := x509.NewCertPool()
	inter.AddCert(leaf.Cert)
	if _, err := child.Verify(x509.VerifyOptions{Roots: pool(ca), Intermediates: inter, CurrentTime: now, DNSName: "b.local"}); err == nil {
		t.Error("a node certificate acted as a CA")
	}
}

func TestExpiryAndClockGrace(t *testing.T) {
	ca, _ := newCA(t, CAOptions{})
	leaf, _ := ca.Issue(LeafOptions{Names: []string{"a.local"}, Days: 30, Now: now})
	if err := verify(leaf.Cert, ca, "a.local", now.AddDate(0, 0, 29)); err != nil {
		t.Errorf("29 days in: %v", err)
	}
	if err := verify(leaf.Cert, ca, "a.local", now.AddDate(0, 0, 32)); err == nil {
		t.Error("valid after it expired")
	}
	// A phone whose clock is a few hours behind must still accept it; two days behind must not be silently fine either.
	if err := verify(leaf.Cert, ca, "a.local", now.Add(-6*time.Hour)); err != nil {
		t.Errorf("a clock 6 hours behind: %v", err)
	}
	if err := verify(leaf.Cert, ca, "a.local", now.Add(-48*time.Hour)); err == nil {
		t.Error("valid two days before it was issued")
	}
}

func TestIssueRejectsBadRequests(t *testing.T) {
	ca, _ := newCA(t, CAOptions{})
	for name, o := range map[string]LeafOptions{
		"no names":        {Now: now},
		"too long":        {Names: []string{"a.local"}, Days: MaxLeafDays + 1, Now: now},
		"a wildcard":      {Names: []string{"*.local"}, Now: now},
		"a space":         {Names: []string{"my node.local"}, Now: now},
		"an empty name":   {Names: []string{" "}, Now: now},
		"a URL, not host": {Names: []string{"https://a.local"}, Now: now},
	} {
		if _, err := ca.Issue(o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := (&CA{Cert: ca.Cert}).Issue(LeafOptions{Names: []string{"a.local"}}); err == nil || !strings.Contains(err.Error(), "without its key") {
		t.Errorf("a CA without a key signed: %v", err)
	}
}

func TestSerialNumbersDiffer(t *testing.T) {
	ca, _ := newCA(t, CAOptions{})
	seen := map[string]bool{ca.Cert.SerialNumber.String(): true}
	for i := 0; i < 20; i++ {
		l, err := ca.Issue(LeafOptions{Names: []string{"a.local"}, Now: now})
		if err != nil {
			t.Fatal(err)
		}
		s := l.Cert.SerialNumber.String()
		if seen[s] || l.Cert.SerialNumber.Sign() <= 0 {
			t.Fatalf("serial %s repeated or not positive", s)
		}
		seen[s] = true
	}
}

func TestNarrowerAndWiderCAs(t *testing.T) {
	narrow, _ := newCA(t, CAOptions{Only: []string{"192.168.50.0/24", "site.local"}})
	if _, err := narrow.Issue(LeafOptions{Names: []string{"192.168.50.7", "site.local"}, Now: now}); err != nil {
		t.Errorf("inside the narrow CA: %v", err)
	}
	if _, err := narrow.Issue(LeafOptions{Names: []string{"192.168.1.5"}, Now: now}); err == nil {
		t.Error("a narrow CA issued outside its range")
	}
	if _, err := narrow.Issue(LeafOptions{Names: []string{"other.local"}, Now: now}); err == nil {
		t.Error("a narrow CA issued outside its names")
	}
	wide, _ := newCA(t, CAOptions{Permit: []string{"10.99.0.0/16", "farm.example"}})
	if _, err := wide.Issue(LeafOptions{Names: []string{"gate.farm.example"}, Now: now}); err != nil {
		t.Errorf("an added domain: %v", err)
	}
	if _, err := wide.Issue(LeafOptions{Names: []string{"x.local"}, Now: now}); err != nil {
		t.Errorf("the defaults are kept when adding: %v", err)
	}
	for _, bad := range []string{"", "a b", "https://x", "*.x", "10.0.0.0/99"} {
		if _, err := InitCA(filepath.Join(t.TempDir(), "x"), CAOptions{Permit: []string{bad}, Now: now}); err == nil {
			t.Errorf("accepted the permit %q", bad)
		}
	}
}

func TestLoadCARefusesUnsafeOrWrongKeys(t *testing.T) {
	_, dir := newCA(t, CAOptions{})
	keyPath := filepath.Join(dir, CAKeyFile)

	if runtime.GOOS != "windows" {
		if err := os.Chmod(keyPath, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadCA(dir, now); err == nil || !strings.Contains(err.Error(), "chmod 600") {
			t.Errorf("a world-readable CA key loaded: %v", err)
		}
		os.Chmod(keyPath, 0o600)
	}

	other, otherDir := newCA(t, CAOptions{})
	_ = other
	otherKey, _ := os.ReadFile(filepath.Join(otherDir, CAKeyFile))
	os.WriteFile(keyPath, otherKey, 0o600)
	if _, err := LoadCA(dir, now); err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Errorf("a mismatched key loaded: %v", err)
	}

	os.Remove(keyPath)
	if _, err := LoadCA(dir, now); err == nil || !strings.Contains(err.Error(), "offline") {
		t.Errorf("a missing key should say the key is kept offline: %v", err)
	}
	// The public half alone still loads, and cannot sign.
	pub, err := LoadCACert(filepath.Join(dir, CACertFile))
	if err != nil || pub.Key != nil {
		t.Fatalf("public load: %v", err)
	}
}

func TestLoadCARefusesAnExpiredCA(t *testing.T) {
	_, dir := newCA(t, CAOptions{Years: 1})
	if _, err := LoadCA(dir, now.AddDate(2, 0, 0)); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("an expired CA loaded: %v", err)
	}
}

func TestLeafWriteAndLoadKeyPair(t *testing.T) {
	ca, _ := newCA(t, CAOptions{})
	leaf, _ := ca.Issue(LeafOptions{Names: []string{"a.local"}, Now: now})
	dir := filepath.Join(t.TempDir(), "node")
	if err := leaf.Write(dir); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(filepath.Join(dir, "node.key"))
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("node.key mode %o", fi.Mode().Perm())
		}
	}
	c, k, err := LoadKeyPair(filepath.Join(dir, "node.crt"), filepath.Join(dir, "node.key"))
	if err != nil || c == nil || k == nil {
		t.Fatalf("load: %v", err)
	}
	if err := leaf.Write(dir); err == nil || !strings.Contains(err.Error(), "not overwritten") {
		t.Errorf("a second write overwrote: %v", err)
	}
	// A key from another certificate is caught.
	other, _ := ca.Issue(LeafOptions{Names: []string{"b.local"}, Now: now})
	odir := filepath.Join(t.TempDir(), "other")
	other.Write(odir)
	if _, _, err := LoadKeyPair(filepath.Join(dir, "node.crt"), filepath.Join(odir, "node.key")); err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Errorf("mismatched pair loaded: %v", err)
	}
	if runtime.GOOS != "windows" {
		os.Chmod(filepath.Join(dir, "node.key"), 0o640)
		if _, _, err := LoadKeyPair(filepath.Join(dir, "node.crt"), filepath.Join(dir, "node.key")); err == nil {
			t.Error("a group-readable node key loaded")
		}
	}
}

func TestFingerprintAndDescribe(t *testing.T) {
	ca, _ := newCA(t, CAOptions{Name: "Depot CA"})
	sum := sha256.Sum256(ca.Cert.Raw)
	want := strings.ToUpper(hex.EncodeToString(sum[:2]))
	if got := Fingerprint(ca.Cert); !strings.HasPrefix(got, want[:2]+":"+want[2:4]) || strings.Count(got, ":") != 31 {
		t.Errorf("fingerprint %q", got)
	}
	d := Describe(ca.Cert, now)
	for _, w := range []string{"certificate authority: Depot CA", "days left", "may issue", ".local", "192.168.0.0/16", Fingerprint(ca.Cert)} {
		if !strings.Contains(d, w) {
			t.Errorf("Describe lacks %q:\n%s", w, d)
		}
	}
	leaf, _ := ca.Issue(LeafOptions{Names: []string{"10.1.2.3", "a.local"}, Days: 10, Now: now})
	d = Describe(leaf.Cert, now.AddDate(0, 0, 40))
	if !strings.Contains(d, "EXPIRED") || !strings.Contains(d, "10.1.2.3") || !strings.Contains(d, "a.local") {
		t.Errorf("leaf description:\n%s", d)
	}
	// A CA with no constraints says so loudly.
	free := &x509.Certificate{IsCA: true, Subject: pkix.Name{CommonName: "x"}, NotAfter: now.AddDate(1, 0, 0)}
	if !strings.Contains(Describe(free, now), "no name constraints") {
		t.Error("an unconstrained CA is not flagged")
	}
}

func TestPermittedIPRangesAreWhatWeSay(t *testing.T) {
	ca, _ := newCA(t, CAOptions{})
	for _, ip := range []string{"10.1.2.3", "172.16.5.5", "192.168.1.1", "169.254.1.1", "127.0.0.1", "fd12:3456::1", "fe80::1", "::1"} {
		if _, err := ca.Issue(LeafOptions{Names: []string{ip}, Now: now}); err != nil {
			t.Errorf("%s should be allowed: %v", ip, err)
		}
	}
	for _, ip := range []string{"172.32.0.1", "11.0.0.1", "192.169.0.1", "2001:db8::1"} {
		if _, err := ca.Issue(LeafOptions{Names: []string{ip}, Now: now}); err == nil {
			t.Errorf("%s should be refused", ip)
		}
	}
	_ = net.IPv4len
}
