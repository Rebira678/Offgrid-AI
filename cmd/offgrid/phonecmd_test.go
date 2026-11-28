package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"offgrid/internal/identity"
	"offgrid/internal/phone"
	"offgrid/internal/pki"
	"offgrid/internal/store/sqlstore"
)

// A real run: a founded node, `phone pair`, `phone serve` over TLS with a certificate from the Day 33 CA, and a
// phone (here, Go code holding a device key the way the browser does) that pairs, reads, syncs a late command and
// signs in again without a code.
func TestPhonePairAndServeEndToEnd(t *testing.T) {
	useKey(t, "site")
	dir := t.TempDir()
	db := filepath.Join(dir, "site.db")
	runCLI(t, "", "exec", "-db", db, `{"intent":"list_tasks"}`) // makes the database
	if code, out, errOut := runCLI(t, "", "store", "-db", db, "found", "-label", "site"); code != 0 {
		t.Fatalf("found: %d %s%s", code, out, errOut)
	}
	st, err := openExistingStore(context.Background(), db, sqlstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	ca, err := pki.InitCA(filepath.Join(dir, "ca"), pki.CAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.Issue(pki.LeafOptions{Names: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := leaf.Write(dir); err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	cl := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}, Timeout: 10 * time.Second}
	t.Cleanup(cl.CloseIdleConnections)

	code, out, errOut := runCLI(t, "", "phone", "pair", "-db", db, "-label", "Ana's phone")
	if code != 0 {
		t.Fatalf("pair: %d %s%s", code, out, errOut)
	}
	m := regexp.MustCompile(`\n\s+([A-Z2-7]{4}(?:-[A-Z2-7]{4}){3,7})\n`).FindStringSubmatch(out)
	if m == nil || !strings.Contains(out, "works once") {
		t.Fatalf("no code in %q", out)
	}
	code4 := m[1]

	ctx, cancel := context.WithCancel(context.Background())
	stdout, stderr := &syncBuf{}, &syncBuf{}
	done := make(chan int, 1)
	go func() {
		done <- phoneServeContext(ctx, []string{"-db", db, "-addr", "127.0.0.1:0", "-cert", filepath.Join(dir, "node.crt"), "-key", filepath.Join(dir, "node.key"), "-host", "127.0.0.1"}, stdout, stderr)
	}()
	mm := waitMatch(t, stdout, regexp.MustCompile(`app on (https://\S+)`))
	base := mm[1]
	defer func() {
		cancel()
		select {
		case c := <-done:
			if c != 0 {
				t.Errorf("phone serve exited %d: %s", c, stderr.String())
			}
		case <-time.After(10 * time.Second):
			t.Error("phone serve did not stop when its context ended")
		}
	}()

	post := func(path string, body any, cookie string) (*http.Response, []byte) {
		j, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", base+path, bytes.NewReader(j))
		req.Header.Set(phone.ActionHeader, "1")
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: phone.CookieName, Value: cookie})
		}
		res, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var b bytes.Buffer
		b.ReadFrom(res.Body)
		return res, b.Bytes()
	}
	get := func(path, cookie string) (*http.Response, []byte) {
		req, _ := http.NewRequest("GET", base+path, nil)
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: phone.CookieName, Value: cookie})
		}
		res, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var b bytes.Buffer
		b.ReadFrom(res.Body)
		return res, b.Bytes()
	}

	if res, _ := get("/api/state", ""); res.StatusCode != 401 {
		t.Fatalf("unpaired over real TLS: %d", res.StatusCode)
	}
	if res, body := get("/", ""); res.StatusCode != 200 || !strings.Contains(string(body), "/app.js") {
		t.Fatalf("the app: %d", res.StatusCode)
	}

	// the phone pairs
	ph, _ := identity.Generate()
	proof, err := sqlstore.EnrollProof(code4, ph, "Ana's phone")
	if err != nil {
		t.Fatal(err)
	}
	res, body := post("/api/pair", map[string]string{"code": code4, "public_key": ph.PublicKey(), "label": "Ana's phone", "proof": base64.RawURLEncoding.EncodeToString(proof)}, "")
	if res.StatusCode != 200 {
		t.Fatalf("pair: %d %s", res.StatusCode, body)
	}
	var ck string
	for _, c := range res.Cookies() {
		if c.Name == phone.CookieName {
			ck = c.Value
		}
	}
	if ck == "" {
		t.Fatal("no cookie over TLS")
	}
	if res, body := get("/api/state", ck); res.StatusCode != 200 {
		t.Fatalf("state: %d %s", res.StatusCode, body)
	}
	// a late command
	res, body = post("/api/sync", map[string]any{"commands": []map[string]any{{"id": "e2e-cmd-0001", "op": "create", "title": "check the gate", "age_ms": 3600000}}}, ck)
	if res.StatusCode != 200 || !strings.Contains(string(body), `"outcome":"applied"`) || !strings.Contains(string(body), `"late":true`) {
		t.Fatalf("sync: %d %s", res.StatusCode, body)
	}
	// the same code again is refused
	if res, _ := post("/api/pair", map[string]string{"code": code4, "public_key": ph.PublicKey(), "label": "x", "proof": base64.RawURLEncoding.EncodeToString(proof)}, ""); res.StatusCode == 200 {
		t.Fatal("a pairing code worked twice")
	}
	// signing in again with the key
	_, body = post("/api/challenge", map[string]string{}, "")
	var chal struct{ Nonce string }
	json.Unmarshal(body, &chal)
	dev, _ := identity.DeviceID(ph.PublicKey())
	sig, _ := ph.Sign(phone.LoginMessage(dev, chal.Nonce))
	res, body = post("/api/login", map[string]string{"device": dev, "nonce": chal.Nonce, "sig": base64.RawURLEncoding.EncodeToString(sig)}, "")
	if res.StatusCode != 200 {
		t.Fatalf("login: %d %s", res.StatusCode, body)
	}
}

func TestPhoneServeRefusals(t *testing.T) {
	useKey(t, "site")
	dir := t.TempDir()
	db := filepath.Join(dir, "site.db")
	if code, _, _ := runCLI(t, "", "phone", "serve"); code != 2 {
		t.Errorf("no -db: %d", code)
	}
	if code, _, e := runCLI(t, "", "phone", "serve", "-db", db, "-cert", "x"); code != 2 || !strings.Contains(e, "both -cert and -key") {
		t.Errorf("half a pair: %d %s", code, e)
	}
	if code, _, e := runCLI(t, "", "phone", "serve", "-db", db); code == 0 {
		t.Errorf("a missing database was served: %s", e)
	}
	if code, out, _ := runCLI(t, "", "phone", "help"); code != 0 || !strings.Contains(out, "reset-sessions") {
		t.Errorf("help: %d", code)
	}
	if code, _, _ := runCLI(t, "", "phone", "bogus"); code != 2 {
		t.Errorf("unknown: %d", code)
	}
	if code, _, e := runCLI(t, "", "phone", "pair"); code != 2 {
		t.Errorf("pair without -db: %d %s", code, e)
	}
	runCLI(t, "", "exec", "-db", db, `{"intent":"list_tasks"}`)
	// founded, no certificate, not loopback: refused (a phone needs HTTPS)
	if code, _, e := runCLI(t, "", "store", "-db", db, "found", "-label", "site"); code != 0 {
		t.Fatalf("found: %s", e)
	}
	code, _, e := runCLI(t, "", "phone", "serve", "-db", db, "-addr", "0.0.0.0:0")
	if code == 0 || !strings.Contains(e, "plain HTTP") {
		t.Errorf("plain HTTP on every interface: %d %s", code, e)
	}
	// a membership-less database cannot serve phones
	db2 := filepath.Join(dir, "empty.db")
	runCLI(t, "", "exec", "-db", db2, `{"intent":"list_tasks"}`)
	if code, _, e := runCLI(t, "", "phone", "serve", "-db", db2, "-addr", "127.0.0.1:0"); code == 0 || !strings.Contains(e, "no membership") {
		t.Errorf("no membership: %d %s", code, e)
	}
}

func TestPhoneResetSessionsRotatesTheKey(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "k")
	a, err := phone.LoadOrCreateSecret(p)
	if err != nil || len(a) != 32 {
		t.Fatal(err)
	}
	if code, out, e := runCLI(t, "", "phone", "reset-sessions", "-secret", p); code != 0 || !strings.Contains(out, "signs in again") {
		t.Fatalf("%d %s%s", code, out, e)
	}
	b, _ := phone.LoadOrCreateSecret(p)
	if bytes.Equal(a, b) {
		t.Fatal("the key did not change")
	}
	if code, _, _ := runCLI(t, "", "phone", "reset-sessions"); code != 2 {
		t.Fatalf("no target: %d", code)
	}
}

func TestGroupTokenGroupsByFourWhateverItIsGivenAndKeepsTheCodeValid(t *testing.T) {
	for in, want := range map[string]string{
		"ABCDEFGHIJKLMNOPQRST":     "ABCD-EFGH-IJKL-MNOP-QRST",
		"ABCD-EFGH-IJKL-MNOP-QRST": "ABCD-EFGH-IJKL-MNOP-QRST", // already grouped by the store: it was printed as "ABCD--EFG-H-IJ-..."
		"abcd efgh":                "ABCD-EFGH",
		"ABCDE":                    "ABCD-E",
	} {
		if got := groupToken(in); got != want {
			t.Errorf("%q: %q want %q", in, got, want)
		}
	}
}
