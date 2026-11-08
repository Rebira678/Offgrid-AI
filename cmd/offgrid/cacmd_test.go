package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"offgrid/internal/pki"
)

func TestCAInitIssueShow(t *testing.T) {
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca")
	node := filepath.Join(dir, "node")

	code, out, errOut := runCLI(t, "", "ca", "init", "-dir", ca, "-name", "Depot CA")
	if code != 0 || !strings.Contains(out, "Depot CA") || !strings.Contains(out, "ca.key (SECRET)") || !strings.Contains(out, "USB stick") {
		t.Fatalf("init: %d\n%s\n%s", code, out, errOut)
	}
	if code, _, e := runCLI(t, "", "ca", "init", "-dir", ca); code != 1 || !strings.Contains(e, "already exists") {
		t.Errorf("a second init: exit %d %q", code, e)
	}

	code, out, errOut = runCLI(t, "", "ca", "issue", "-dir", ca, "-out", node, "-name", "10.20.30.40", "-name", "farm.local")
	if code != 0 || !strings.Contains(out, "10.20.30.40") || !strings.Contains(out, "does NOT need ca.key") {
		t.Fatalf("issue: %d\n%s\n%s", code, out, errOut)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(filepath.Join(node, "node.key")); fi.Mode().Perm() != 0o600 {
			t.Errorf("node.key mode %o", fi.Mode().Perm())
		}
	}
	code, out, _ = runCLI(t, "", "ca", "show", filepath.Join(ca, "ca.crt"))
	if code != 0 || !strings.Contains(out, "may issue") || !strings.Contains(out, "days left") {
		t.Errorf("show CA: %d\n%s", code, out)
	}
	code, out, _ = runCLI(t, "", "ca", "show", filepath.Join(node, "node.crt"))
	if code != 0 || !strings.Contains(out, "farm.local") || strings.Contains(out, "may issue") {
		t.Errorf("show node: %d\n%s", code, out)
	}
}

func TestCAIssueRefusesWhatTheCAMayNotSign(t *testing.T) {
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca")
	runCLI(t, "", "ca", "init", "-dir", ca)
	node := filepath.Join(dir, "node")
	for _, name := range []string{"example.com", "8.8.8.8"} {
		code, _, e := runCLI(t, "", "ca", "issue", "-dir", ca, "-out", node, "-name", name)
		if code != 1 || !strings.Contains(e, "cannot issue") {
			t.Errorf("%s: exit %d %q", name, code, e)
		}
		if _, err := os.Stat(node); err == nil {
			t.Errorf("%s: a folder was created for a refused certificate", name)
		}
	}
	// The CA key is kept offline: with it gone, issuing says so.
	os.Remove(filepath.Join(ca, "ca.key"))
	if code, _, e := runCLI(t, "", "ca", "issue", "-dir", ca, "-out", node, "-name", "a.local"); code != 1 || !strings.Contains(e, "offline") {
		t.Errorf("no key: exit %d %q", code, e)
	}
}

func TestCAUsage(t *testing.T) {
	for _, args := range [][]string{
		{"ca"}, {"ca", "nope"}, {"ca", "init"}, {"ca", "issue", "-dir", "x"}, {"ca", "issue", "-dir", "x", "-out", "y"},
		{"ca", "show"}, {"ca", "spike"}, {"ca", "init", "-dir", "x", "extra"},
	} {
		if code, _, _ := runCLI(t, "", args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if code, _, e := runCLI(t, "", "ca", "help"); code != 0 || !strings.Contains(e, "offgrid ca init") {
		t.Errorf("help: %d %q", code, e)
	}
	if _, out, _ := runCLI(t, "", "help"); !strings.Contains(out, "offgrid ca") {
		t.Error("main usage does not list ca")
	}
	if code, _, e := runCLI(t, "", "ca", "init", "-dir", filepath.Join(t.TempDir(), "x"), "-permit", "not a thing"); code != 1 || !strings.Contains(e, "neither a CIDR") {
		t.Errorf("a bad -permit: %d %q", code, e)
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func TestCASpikeServesAPhoneThatTrustsTheCA(t *testing.T) {
	dir := t.TempDir()
	ca, node := filepath.Join(dir, "ca"), filepath.Join(dir, "node")
	runCLI(t, "", "ca", "init", "-dir", ca)
	if code, _, e := runCLI(t, "", "ca", "issue", "-dir", ca, "-out", node, "-name", "127.0.0.1"); code != 0 {
		t.Fatal(e)
	}
	httpsAddr, httpAddr := freePort(t), freePort(t)
	var out, errOut bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- cmdCASpike(ctx, []string{"-cert", filepath.Join(node, "node.crt"), "-key", filepath.Join(node, "node.key"), "-ca", filepath.Join(ca, "ca.crt"),
			"-addr", httpsAddr, "-http-addr", httpAddr, "-results", filepath.Join(dir, "r.jsonl"), "-for", "1m"}, &out, &errOut)
	}()
	pem, _ := os.ReadFile(filepath.Join(ca, "ca.crt"))
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)
	c := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	var body string
	for i := 0; i < 50; i++ {
		resp, err := c.Get("https://" + httpsAddr + "/")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body = string(b)
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(body, "Offgrid phone check") {
		cancel()
		t.Fatalf("no page. stderr: %s", errOut.String())
	}
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Get("http://" + httpAddr + "/ca.crt")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != string(pem) {
		t.Error("/ca.crt is not the CA certificate")
	}
	// Old TLS is refused: the page is served with TLS 1.2 or newer only.
	old := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11}}}
	if _, err := old.Get("https://" + httpsAddr + "/"); err == nil {
		t.Error("a TLS 1.1 client connected")
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit %d: %s", code, errOut.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("did not stop when asked")
	}
	if !strings.Contains(out.String(), "CA fingerprint") || !strings.Contains(out.String(), "stopped") {
		t.Errorf("output:\n%s", out.String())
	}
	// ca.key sits next to ca.crt in this setup, and the command says to move it.
	if !strings.Contains(errOut.String(), "USB stick") {
		t.Errorf("no warning about ca.key on the node: %q", errOut.String())
	}
}

func TestCASpikeRefusesBadInputs(t *testing.T) {
	dir := t.TempDir()
	ca, node := filepath.Join(dir, "ca"), filepath.Join(dir, "node")
	runCLI(t, "", "ca", "init", "-dir", ca)
	runCLI(t, "", "ca", "issue", "-dir", ca, "-out", node, "-name", "127.0.0.1")
	base := []string{"ca", "spike", "-cert", filepath.Join(node, "node.crt"), "-key", filepath.Join(node, "node.key"), "-ca", filepath.Join(ca, "ca.crt"), "-addr", "127.0.0.1:0", "-http-addr", "", "-for", "1s"}

	// ca.key given instead of ca.crt
	bad := append([]string{}, base...)
	bad[7] = filepath.Join(ca, "ca.key")
	if code, _, e := runCLI(t, "", bad...); code != 1 || !strings.Contains(e, "error:") {
		t.Errorf("a key as the CA certificate: %d %q", code, e)
	}
	// a node key other users can read
	if runtime.GOOS != "windows" {
		os.Chmod(filepath.Join(node, "node.key"), 0o644)
		if code, _, e := runCLI(t, "", base...); code != 1 || !strings.Contains(e, "chmod 600") {
			t.Errorf("a world-readable node key: %d %q", code, e)
		}
		os.Chmod(filepath.Join(node, "node.key"), 0o600)
	}
}

func TestCASpikeRefusesAnExpiredNodeCertificate(t *testing.T) {
	dir := t.TempDir()
	caDir, node := filepath.Join(dir, "ca"), filepath.Join(dir, "node")
	// A CA that already existed two years ago, and a 30-day node certificate issued then.
	then := time.Now().AddDate(-2, 0, 0)
	ca, err := pki.InitCA(caDir, pki.CAOptions{Now: then})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.Issue(pki.LeafOptions{Names: []string{"127.0.0.1"}, Days: 30, Now: then})
	if err != nil {
		t.Fatal(err)
	}
	if err := leaf.Write(node); err != nil {
		t.Fatal(err)
	}
	code, _, e := runCLI(t, "", "ca", "spike", "-cert", filepath.Join(node, "node.crt"), "-key", filepath.Join(node, "node.key"), "-ca", filepath.Join(caDir, "ca.crt"), "-addr", "127.0.0.1:0", "-http-addr", "", "-for", "1s")
	if code != 1 || !strings.Contains(e, "expired") {
		t.Errorf("an expired node certificate was served: exit %d %q", code, e)
	}
}
