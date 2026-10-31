package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"offgrid/internal/task"
)

//go:embed static
var staticFiles embed.FS

// ActionHeader must be present on every POST. A web page on another site
// cannot send a custom header without the browser first asking this server for
// permission (a CORS preflight), and this server never grants it.
const ActionHeader = "X-Offgrid-Action"

// Handler returns the dashboard: GET / (the page), GET /api/state, and
// POST /api/suggestions/{id}/accept or /dismiss.
func Handler(c Config) http.Handler {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err) // the embedded folder is part of the program
	}
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(sub))
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		st, err := Build(r.Context(), c)
		if err != nil {
			if r.Context().Err() != nil {
				return // the page went away
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "cannot read the task log: " + err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, st)
	})
	mux.HandleFunc("POST /api/suggestions/{id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		code, res := resolve(r.Context(), c, r.PathValue("id"), r.PathValue("action"))
		writeJSON(w, code, res)
	})
	return guard(mux)
}

// resolve is the only write the dashboard can make.
func resolve(ctx context.Context, c Config, id, action string) (int, any) {
	fail := func(code int, msg string) (int, any) { return code, map[string]string{"error": msg} }
	if c.Store == nil {
		return fail(http.StatusInternalServerError, "no store configured")
	}
	if c.ReadOnly {
		return fail(http.StatusForbidden, errReadOnly.Error())
	}
	var accept bool
	switch action {
	case "accept":
		accept = true
	case "dismiss":
	default:
		return fail(http.StatusNotFound, "unknown action "+action+" (use accept or dismiss)")
	}
	t, err := c.Store.Resolve(ctx, id, accept)
	switch {
	case err == nil:
		if accept {
			return http.StatusOK, Resolved{OK: true, Message: fmt.Sprintf("accepted: %s is now %s, %s", task.ShortID(t.ID), t.Status, t.Priority)}
		}
		return http.StatusOK, Resolved{OK: true, Message: fmt.Sprintf("dismissed: %s is unchanged", task.ShortID(t.ID))}
	case errors.Is(err, task.ErrStale):
		return fail(http.StatusConflict, "refused: the task was edited after the AI proposed this, so applying it could undo that edit. Dismiss it instead.")
	case errors.Is(err, task.ErrSuggestionResolved):
		return fail(http.StatusConflict, err.Error())
	case errors.Is(err, task.ErrSuggestionNotFound), errors.Is(err, task.ErrNotFound):
		return fail(http.StatusNotFound, err.Error())
	}
	return fail(http.StatusInternalServerError, "could not apply that: "+err.Error())
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// csp lets the page load only its own script and style, talk only to its own
// server, and nothing else: no inline script, no framing, no outside network.
const csp = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// guard is what makes a login-less, local-only server reasonable.
//
//   - Host must be this machine (localhost, 127.0.0.1, ::1). A page on
//     evil.example that re-points its own name at 127.0.0.1 ("DNS rebinding")
//     arrives with Host: evil.example and is refused.
//   - Only GET, HEAD and POST. A POST needs the ActionHeader and, when the
//     browser sends them, a same-origin Origin and Sec-Fetch-Site.
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-cache, no-store, must-revalidate")
		deny := func(code int, msg string) { writeJSON(w, code, map[string]string{"error": msg}) }

		if !LocalHost(r.Host) {
			deny(http.StatusForbidden, "this dashboard only answers to localhost")
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead:
		case http.MethodPost:
			if r.Header.Get(ActionHeader) != "1" {
				deny(http.StatusForbidden, "missing "+ActionHeader+" header")
				return
			}
			if o := r.Header.Get("Origin"); o != "" {
				u, err := url.Parse(o)
				if err != nil || !strings.EqualFold(u.Host, r.Host) {
					deny(http.StatusForbidden, "cross-origin request refused")
					return
				}
			}
			if sf := r.Header.Get("Sec-Fetch-Site"); sf != "" && sf != "same-origin" && sf != "none" {
				deny(http.StatusForbidden, "cross-site request refused")
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
		default:
			h.Set("Allow", "GET, HEAD, POST")
			deny(http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// LocalHost reports whether a Host header names this machine.
func LocalHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// RequireLoopback refuses an address the dashboard must not listen on. The
// dashboard has no login and no encryption, so it is for this machine only; a
// phone or another laptop should use `offgrid sync`, which is signed.
func RequireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("bad address %q: %v (use host:port, for example 127.0.0.1:8787)", addr, err)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("refusing to listen on %q: the dashboard has no login and no encryption, so it only listens on this machine (127.0.0.1, ::1 or localhost)", addr)
}

// Serve listens on addr until ctx ends. onListen is called with the real
// address (so port 0 works) once the socket is open.
func Serve(ctx context.Context, addr string, c Config, onListen func(net.Addr)) error {
	if err := RequireLoopback(addr); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           Handler(c),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	if onListen != nil {
		onListen(ln.Addr())
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
