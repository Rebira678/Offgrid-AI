package spike

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
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

var fixed = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

type rig struct {
	https   *http.Server
	httpURL string
	url     string
	ca      *pki.CA
	client  *http.Client // trusts only the site CA
	results string
	log     *bytes.Buffer
}

func newRig(t *testing.T) *rig {
	t.Helper()
	dir := t.TempDir()
	ca, err := pki.InitCA(filepath.Join(dir, "ca"), pki.CAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.Issue(pki.LeafOptions{Names: []string{"127.0.0.1", "node.local"}})
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{ca: ca, results: filepath.Join(dir, "results.jsonl"), log: &bytes.Buffer{}}
	srv, err := New(Options{CA: ca.Cert, CAPEM: ca.CertPEM, ResultsPath: r.results, Log: r.log, Now: func() time.Time { return fixed }})
	if err != nil {
		t.Fatal(err)
	}
	key, _ := leafKey(t, leaf)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.Cert.Raw}, PrivateKey: key}}})
	if err != nil {
		t.Fatal(err)
	}
	r.https = &http.Server{Handler: srv.HTTPS()}
	go r.https.Serve(ln)
	t.Cleanup(func() { r.https.Close() })
	r.url = "https://" + ln.Addr().String()

	hln, _ := net.Listen("tcp", "127.0.0.1:0")
	hs := &http.Server{Handler: srv.HTTP()}
	go hs.Serve(hln)
	t.Cleanup(func() { hs.Close() })
	r.httpURL = "http://" + hln.Addr().String()

	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	r.client = &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	return r
}

func get(t *testing.T, c *http.Client, url string) (*http.Response, string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestAPhoneThatTrustsTheCASeesThePageAndOneThatDoesNotIsRefused(t *testing.T) {
	r := newRig(t)
	resp, body := get(t, r.client, r.url+"/")
	if resp.StatusCode != 200 || !strings.Contains(body, "Offgrid phone check") {
		t.Fatalf("page: %d %q", resp.StatusCode, body)
	}
	// The same server, from a client that has not installed the CA: the browser's "your connection is not private".
	plain := &http.Client{Timeout: 5 * time.Second}
	if _, err := plain.Get(r.url + "/"); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("a client without the CA connected: %v", err)
	}
	// And a client that trusts the CA but asks for a different name is refused too.
	bad := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: r.client.Transport.(*http.Transport).TLSClientConfig.RootCAs, ServerName: "other.local"}}}
	if _, err := bad.Get(r.url + "/"); err == nil {
		t.Error("connected under a name the certificate is not valid for")
	}
}

func TestEveryFileIsServedWithTheRightTypeAndHeaders(t *testing.T) {
	r := newRig(t)
	for path, ct := range map[string]string{
		"/": "text/html", "/app.js": "text/javascript", "/sw.js": "text/javascript",
		"/app.css": "text/css", "/manifest.webmanifest": "application/manifest+json",
	} {
		resp, body := get(t, r.client, r.url+path)
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), ct) || len(body) < 20 {
			t.Errorf("%s: %d %q (%d bytes)", path, resp.StatusCode, resp.Header.Get("Content-Type"), len(body))
		}
		if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "script-src 'self'") || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: missing security headers", path)
		}
		if resp.Header.Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: Cache-Control %q", path, resp.Header.Get("Cache-Control"))
		}
	}
	resp, _ := get(t, r.client, r.url+"/sw.js")
	if resp.Header.Get("Service-Worker-Allowed") != "/" {
		t.Error("sw.js lacks Service-Worker-Allowed")
	}
	if resp, _ := get(t, r.client, r.url+"/nope"); resp.StatusCode != 404 {
		t.Errorf("unknown path: %d", resp.StatusCode)
	}
	if resp, _ := get(t, r.client, r.url+"/static/app.js"); resp.StatusCode != 404 {
		t.Errorf("the embed folder is reachable: %d", resp.StatusCode)
	}
	// no inline script anywhere: the CSP would block it, so the page must not need it.
	_, html := get(t, r.client, r.url+"/")
	if strings.Contains(html, "<script>") || strings.Contains(html, "onclick=") {
		t.Error("index.html has inline script")
	}
	_, ping := get(t, r.client, r.url+"/ping")
	var j map[string]string
	if err := json.Unmarshal([]byte(ping), &j); err != nil || j["now"] != fixed.Format(time.RFC3339Nano) {
		t.Errorf("ping %q", ping)
	}
}

func post(t *testing.T, r *rig, body string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", r.url+"/report", strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

var ok = map[string]string{"Content-Type": "application/json", "X-Offgrid-Spike": "1"}

func TestAPostedResultIsStoredAndSummarized(t *testing.T) {
	r := newRig(t)
	body := `{"ua":"Mozilla/5.0 (Linux; Android 14; Pixel 7) Chrome/130","results":[{"name":"a","ok":true},{"name":"b","ok":false},{"name":"c","ok":null}],"offline_loads":[{"controlled":true},{"controlled":false}]}`
	if resp := post(t, r, body, ok); resp.StatusCode != 204 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	raw, err := os.ReadFile(r.results)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(raw), &obj); err != nil {
		t.Fatalf("not one JSON line: %v\n%s", err, raw)
	}
	if obj["received_at"] != fixed.Format(time.RFC3339) || obj["from"] != "127.0.0.1" {
		t.Errorf("server fields: %v %v", obj["received_at"], obj["from"])
	}
	out := r.log.String()
	for _, w := range []string{"1 passed, 1 failed, 1 notes", "offline loads served by the service worker: 1", "Pixel 7"} {
		if !strings.Contains(out, w) {
			t.Errorf("log lacks %q:\n%s", w, out)
		}
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(r.results)
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("results file mode %o", fi.Mode().Perm())
		}
	}
	post(t, r, body, ok)
	raw, _ = os.ReadFile(r.results)
	if n := strings.Count(strings.TrimSpace(string(raw)), "\n") + 1; n != 2 {
		t.Errorf("%d lines after two posts", n)
	}
}

func TestReportRefusesWhatIsNotThePage(t *testing.T) {
	r := newRig(t)
	for name, c := range map[string]struct {
		body string
		hdr  map[string]string
		want int
	}{
		"no marker header": {`{"a":1}`, map[string]string{"Content-Type": "application/json"}, 400},
		"not JSON type":    {`{"a":1}`, map[string]string{"Content-Type": "text/plain", "X-Offgrid-Spike": "1"}, 400},
		"not an object":    {`[1,2]`, ok, 400},
		"garbage":          {`{{{`, ok, 400},
		"null":             {`null`, ok, 400},
		"too large":        {`{"x":"` + strings.Repeat("a", MaxReportBytes) + `"}`, ok, 413},
	} {
		if resp := post(t, r, c.body, c.hdr); resp.StatusCode != c.want {
			t.Errorf("%s: status %d, want %d", name, resp.StatusCode, c.want)
		}
	}
	if resp, _ := get(t, r.client, r.url+"/report"); resp.StatusCode != 405 {
		t.Errorf("GET /report: %d", resp.StatusCode)
	}
	if _, err := os.Stat(r.results); err == nil {
		t.Error("a refused post left a results file")
	}
}

func TestThePlainHTTPPageOnlyHandsOutThePublicCA(t *testing.T) {
	r := newRig(t)
	plain := &http.Client{Timeout: 5 * time.Second}
	resp, body := get(t, plain, r.httpURL+"/ca.crt")
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/x-x509-ca-cert") {
		t.Fatalf("ca.crt: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if body != string(r.ca.CertPEM) {
		t.Error("ca.crt is not the CA certificate")
	}
	if strings.Contains(body, "PRIVATE KEY") {
		t.Fatal("the CA key is being served")
	}
	_, page := get(t, plain, r.httpURL+"/")
	if !strings.Contains(page, pki.Fingerprint(r.ca.Cert)) || !strings.Contains(page, "/ca.crt") {
		t.Error("the install page lacks the link or the fingerprint")
	}
	for _, p := range []string{"/app.js", "/sw.js", "/report", "/ping", "/ca.key", "/../ca.key"} {
		if resp, _ := get(t, plain, r.httpURL+p); resp.StatusCode != 404 {
			t.Errorf("plain HTTP serves %s: %d", p, resp.StatusCode)
		}
	}
}

func TestNewRequiresACA(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("a server without a CA was made")
	}
}

func TestSummarizeToleratesAnyShape(t *testing.T) {
	for _, in := range []map[string]any{
		{}, {"results": "x"}, {"results": []any{1, "a", nil}}, {"offline_loads": 5}, {"ua": strings.Repeat("u", 500)},
	} {
		if s := Summarize(in); s == "" || len(s) > 400 {
			t.Errorf("Summarize(%v) = %q", in, s)
		}
	}
}

var _ = context.Background

func leafKey(t *testing.T, l *pki.Leaf) (any, error) {
	t.Helper()
	dir := t.TempDir()
	if err := l.Write(filepath.Join(dir, "n")); err != nil {
		t.Fatal(err)
	}
	_, k, err := pki.LoadKeyPair(filepath.Join(dir, "n", "node.crt"), filepath.Join(dir, "n", "node.key"))
	return k, err
}
