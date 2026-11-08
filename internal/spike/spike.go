// Package spike serves the Day 33 phone check: one small HTTPS page that tests what a
// phone browser needs to run Offgrid offline (a trusted private certificate, a service
// worker, offline reload, IndexedDB, and a non-extractable ECDSA device key), plus a
// plain-HTTP page whose only job is to hand the phone the CA certificate to install.
//
// It is a spike: it exists to produce a written result for each phone, then be
// deleted or folded into the real phone client (Day 52). It stores nothing about people.
package spike

import (
	"bytes"
	"crypto/x509"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"offgrid/internal/pki"
)

//go:embed static
var static embed.FS

// MaxReportBytes bounds a posted result. A real one is about 3 KiB.
const MaxReportBytes = 32 << 10

// Options configures a Server.
type Options struct {
	CA          *x509.Certificate // the public CA certificate the phone must install
	CAPEM       []byte
	ResultsPath string           // JSON lines, one per posted result; empty keeps them in the log only
	Log         io.Writer        // a line per result
	Now         func() time.Time // for tests
}

// Server holds the two handlers.
type Server struct {
	o  Options
	mu sync.Mutex
}

// New returns a Server. CAPEM and CA are required.
func New(o Options) (*Server, error) {
	if o.CA == nil || len(o.CAPEM) == 0 {
		return nil, errors.New("spike: the CA certificate is required")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
	return &Server{o: o}, nil
}

const csp = "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

func secure(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
}

var types = map[string]string{
	"/":                     "text/html; charset=utf-8",
	"/app.js":               "text/javascript; charset=utf-8",
	"/sw.js":                "text/javascript; charset=utf-8",
	"/app.css":              "text/css; charset=utf-8",
	"/manifest.webmanifest": "application/manifest+json",
}

// HTTPS is the page itself.
func (s *Server) HTTPS() http.Handler {
	mux := http.NewServeMux()
	serve := func(w http.ResponseWriter, r *http.Request) {
		secure(w)
		name := r.URL.Path
		ct, ok := types[name]
		if !ok || r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		file := strings.TrimPrefix(name, "/")
		if name == "/" {
			file = "index.html"
		}
		b, err := fs.ReadFile(static, "static/"+file)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", ct)
		// no-cache, not no-store: the browser may keep it but must ask first. The service
		// worker, not the HTTP cache, is what makes the page work offline.
		w.Header().Set("Cache-Control", "no-cache")
		if name == "/sw.js" {
			w.Header().Set("Service-Worker-Allowed", "/")
		}
		_, _ = w.Write(b)
	}
	mux.HandleFunc("/", serve)
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		secure(w)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"now": s.o.Now().UTC().Format(time.RFC3339Nano)})
	})
	mux.HandleFunc("/report", s.report)
	return mux
}

// report accepts one posted result. Only a JSON object, only from this page (a custom
// header a cross-site form cannot send), at most MaxReportBytes.
func (s *Server) report(w http.ResponseWriter, r *http.Request) {
	secure(w)
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("X-Offgrid-Spike") != "1" || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		http.Error(w, "not from the phone check page", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxReportBytes))
	if err != nil {
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		return
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		http.Error(w, "not a JSON object", http.StatusBadRequest)
		return
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	obj["received_at"] = s.o.Now().UTC().Format(time.RFC3339)
	obj["from"] = host
	line, _ := json.Marshal(obj)
	if err := s.store(line); err != nil {
		http.Error(w, "could not store", http.StatusInternalServerError)
		fmt.Fprintf(s.o.Log, "could not store a result: %v\n", err)
		return
	}
	fmt.Fprintln(s.o.Log, Summarize(obj))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) store(line []byte) error {
	if s.o.ResultsPath == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.o.ResultsPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Summarize is the one-line log entry for a posted result: who, how many passed, and
// whether an offline load was seen. It reads only fields it knows and tolerates any shape.
func Summarize(obj map[string]any) string {
	pass, fail, note := 0, 0, 0
	if rs, ok := obj["results"].([]any); ok {
		for _, r := range rs {
			m, _ := r.(map[string]any)
			switch v := m["ok"].(type) {
			case bool:
				if v {
					pass++
				} else {
					fail++
				}
			default:
				note++
			}
		}
	}
	offline := 0
	if ol, ok := obj["offline_loads"].([]any); ok {
		for _, x := range ol {
			if m, _ := x.(map[string]any); m != nil && m["controlled"] == true {
				offline++
			}
		}
	}
	ua, _ := obj["ua"].(string)
	if len(ua) > 90 {
		ua = ua[:90] + "…"
	}
	return fmt.Sprintf("result from %v: %d passed, %d failed, %d notes, offline loads served by the service worker: %d\n  %s", obj["from"], pass, fail, note, offline, ua)
}

// HTTP is the plain page that hands out the CA certificate. It serves nothing else, and
// nothing on it is secret: the certificate is public, and the phone is shown its
// fingerprint to compare with the one printed on the node.
func (s *Server) HTTP() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ca.crt", func(w http.ResponseWriter, r *http.Request) {
		secure(w)
		w.Header().Set("Content-Type", "application/x-x509-ca-cert")
		w.Header().Set("Content-Disposition", `attachment; filename="offgrid-ca.crt"`)
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(s.o.CAPEM)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		secure(w)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		var b bytes.Buffer
		fmt.Fprintf(&b, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Offgrid: install the site certificate</title>
<body style="font:16px/1.5 system-ui,sans-serif;max-width:36rem;margin:1rem auto;padding:0 1rem">
<h1>Offgrid: install the site certificate</h1>
<p>1. <a href="/ca.crt">Download the site certificate</a>.</p>
<p>2. Install it as a <b>CA certificate</b>: Settings, Security, Encryption and credentials, Install a certificate, CA certificate. Android asks you to set a screen lock first if you have none.</p>
<p>3. To check you got the right certificate, open Settings, Security, Encryption and credentials, Trusted credentials, User, tap it, and compare its SHA-256 fingerprint with this one, also printed on the node:</p>
<p><code style="word-break:break-all">%s</code></p>
<p>4. Then open the HTTPS address printed on the node.</p>
<p style="color:#666">This certificate can only vouch for private addresses and names ending in .local, .lan or .offgrid. It cannot be used to impersonate a public website.</p>
`, pki.Fingerprint(s.o.CA))
		_, _ = w.Write(b.Bytes())
	})
	return mux
}
