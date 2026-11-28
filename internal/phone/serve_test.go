package phone

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"offgrid/internal/pki"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestTheSessionKeyIsMadePrivateAndReused(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "k")
	a, err := LoadOrCreateSecret(p)
	if err != nil || len(a) != 32 {
		t.Fatalf("%v %d", err, len(a))
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode %o", fi.Mode().Perm())
		}
		if fi, _ := os.Stat(filepath.Dir(p)); fi.Mode().Perm() != 0o700 {
			t.Fatalf("folder mode %o", fi.Mode().Perm())
		}
	}
	b, err := LoadOrCreateSecret(p)
	if err != nil || !bytes.Equal(a, b) {
		t.Fatalf("a second load must return the same key: %v", err)
	}
}

func TestAKeyOthersCanReadOrThatIsTooShortIsRefused(t *testing.T) {
	d := t.TempDir()
	open := filepath.Join(d, "open")
	os.WriteFile(open, bytes.Repeat([]byte{1}, 32), 0o644)
	os.Chmod(open, 0o644)
	if runtime.GOOS != "windows" {
		if _, err := LoadOrCreateSecret(open); err == nil || !strings.Contains(err.Error(), "chmod 600") {
			t.Errorf("readable key: %v", err)
		}
	}
	short := filepath.Join(d, "short")
	os.WriteFile(short, bytes.Repeat([]byte{1}, 31), 0o600)
	if _, err := LoadOrCreateSecret(short); err == nil || !strings.Contains(err.Error(), "at least 32") {
		t.Errorf("short key: %v", err)
	}
	exact := filepath.Join(d, "exact")
	os.WriteFile(exact, bytes.Repeat([]byte{1}, 32), 0o600)
	if _, err := LoadOrCreateSecret(exact); err != nil {
		t.Errorf("exactly 32: %v", err)
	}
}

func TestRotatingTheKeyChangesItAndKeepsItPrivate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k")
	a, _ := LoadOrCreateSecret(p)
	if err := RotateSecret(p); err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreateSecret(p)
	if err != nil || bytes.Equal(a, b) {
		t.Fatalf("not rotated: %v", err)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode %o", fi.Mode().Perm())
		}
	}
	// a session made with the old key no longer verifies
	if _, err := (signer{key: b}).verify(signer{key: a}.issue("dev_a", t0, time.Hour), t0); err == nil {
		t.Fatal("an old session survived the rotation")
	}
}

func TestPlainHTTPIsOnlyForThisMachine(t *testing.T) {
	r := newRig(t, nil)
	cfg := Config{Store: r.st, Secret: r.secret}
	for _, addr := range []string{"0.0.0.0:0", ":0", "192.168.1.20:0", "example.com:80", "noport"} {
		if err := Serve(context.Background(), addr, cfg, nil, nil); err == nil {
			t.Errorf("served plain HTTP on %s", addr)
		}
	}
}

func TestServeAnswersAndStopsWhenItsContextEnds(t *testing.T) {
	r := newRig(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	addr := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, "127.0.0.1:0", Config{Store: r.st, Secret: r.secret}, nil, func(a net.Addr) { addr <- a })
	}()
	a := <-addr
	cl := &http.Client{Timeout: 5 * time.Second}
	defer cl.CloseIdleConnections()
	res, err := cl.Get("http://" + a.String() + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("%d", res.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("did not stop")
	}
	if err := Serve(context.Background(), "127.0.0.1:0", Config{Secret: r.secret}, nil, nil); err == nil {
		t.Fatal("served with no store")
	}
}

func TestTLSServesTheAppAndRefusesOldProtocols(t *testing.T) {
	r := newRig(t, nil)
	ca, err := pki.InitCA(filepath.Join(t.TempDir(), "ca"), pki.CAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.Issue(pki.LeafOptions{Names: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(leaf.CertPEM, leaf.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	addr := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, "127.0.0.1:0", Config{Store: r.st, Secret: r.secret}, &cert, func(a net.Addr) { addr <- a })
	}()
	a := <-addr
	defer func() { cancel(); <-done }()
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	get := func(max uint16) error {
		tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS10, MaxVersion: max}}
		cl := &http.Client{Transport: tr, Timeout: 5 * time.Second}
		defer tr.CloseIdleConnections()
		res, err := cl.Get("https://" + a.String() + "/")
		if err == nil {
			res.Body.Close()
		}
		return err
	}
	if err := get(tls.VersionTLS13); err != nil {
		t.Fatalf("a client that trusts the CA: %v", err)
	}
	if err := get(tls.VersionTLS11); err == nil {
		t.Fatal("TLS 1.1 was accepted")
	}
	if res, err := http.Get("http://" + a.String() + "/"); err == nil { // Go answers 400 to plain HTTP on a TLS port
		res.Body.Close()
		if res.StatusCode == 200 {
			t.Fatal("plain HTTP got the app on the TLS port")
		}
	}
}
