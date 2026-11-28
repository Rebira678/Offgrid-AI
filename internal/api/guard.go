package api

import (
	"net/http"
	"net/url"
	"strings"

	"offgrid/internal/web"
)

// ActionHeader must be on every POST and PATCH, as on the dashboard and the phone server.
const ActionHeader = web.ActionHeader

// LocalGuard wraps the API for `offgrid api serve`: the machine's own program, no login. It applies the
// dashboard's rules: the Host must name this machine (so a page that re-points its own name here cannot
// read it), a write needs the custom header (a page on another site cannot add one without a CORS
// preflight, which is never granted), and a request that says it came from another origin is refused.
func LocalGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; sandbox")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		if !web.LocalHost(r.Host) {
			Fail(w, http.StatusForbidden, "bad_host", "this API answers to this machine only")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get(ActionHeader) != "1" {
				Fail(w, http.StatusForbidden, "missing_header", "send the header "+ActionHeader+": 1 with every write")
				return
			}
		}
		if o := r.Header.Get("Origin"); o != "" {
			u, err := url.Parse(o)
			if err != nil || !strings.EqualFold(u.Host, r.Host) {
				Fail(w, http.StatusForbidden, "cross_origin", "cross-origin request refused")
				return
			}
		}
		if sf := r.Header.Get("Sec-Fetch-Site"); sf != "" && sf != "same-origin" && sf != "none" {
			Fail(w, http.StatusForbidden, "cross_site", "cross-site request refused")
			return
		}
		next.ServeHTTP(w, r)
	})
}
