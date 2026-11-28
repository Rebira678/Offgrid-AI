package phone

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	ClientHeader      = "X-Offgrid-Client"
	ActionHeader      = "X-Offgrid-Action"
	SessionCookieName = "offgrid_phone_session"

	// Strict CSP following Day 26 principles with PWA enhancements (manifest and blob previews)
	StrictCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data: blob:; manifest-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

	MaxJSONBodyBytes  = 1 << 20  // 1 MiB
	MaxPhotoBodyBytes = 32 << 20 // 32 MiB for photos
)

type contextKey string

const sessionContextKey contextKey = "phone_session"

// SessionFromContext returns the authenticated session from request context.
func SessionFromContext(ctx context.Context) (*Session, bool) {
	s, ok := ctx.Value(sessionContextKey).(*Session)
	return s, ok && s != nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// SecurityMiddleware applies strict CSP, standard headers, and CSRF protection.
func SecurityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", StrictCSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")

		// Enforce custom header and origin checks on state-changing requests
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			// Safe read methods
		case http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
			// Check custom header
			hasHeader := r.Header.Get(ClientHeader) == "phone" || r.Header.Get(ActionHeader) == "1"
			if !hasHeader {
				writeError(w, http.StatusForbidden, "missing custom security header ("+ClientHeader+": phone or "+ActionHeader+": 1)")
				return
			}

			// Validate Origin if present
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || !strings.EqualFold(u.Host, r.Host) {
					writeError(w, http.StatusForbidden, "cross-origin request refused")
					return
				}
			}

			// Validate Sec-Fetch-Site if present
			if sf := r.Header.Get("Sec-Fetch-Site"); sf != "" && sf != "same-origin" && sf != "none" {
				writeError(w, http.StatusForbidden, "cross-site request refused")
				return
			}

			// Cap request body size
			maxBytes := int64(MaxJSONBodyBytes)
			if strings.Contains(r.Header.Get("Content-Type"), "multipart/form-data") ||
				strings.HasSuffix(r.URL.Path, "/attachments") {
				maxBytes = MaxPhotoBodyBytes
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		default:
			h.Set("Allow", "GET, HEAD, POST, DELETE, OPTIONS")
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// AuthMiddleware validates phone session tokens from cookies or Authorization Bearer header.
func AuthMiddleware(sessions *SessionStore, secureCookie bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Public paths: root page, static assets, and pair endpoint
		path := r.URL.Path
		if path == "/" ||
			path == "/index.html" ||
			path == "/app.js" ||
			path == "/app.css" ||
			path == "/sw.js" ||
			path == "/manifest.webmanifest" ||
			path == "/icon.svg" ||
			path == "/api/phone/pair" {
			next.ServeHTTP(w, r)
			return
		}

		// Protected paths (all /api/phone/... routes except /pair)
		token := extractToken(r)
		if token == "" {
			writeError(w, http.StatusUnauthorized, "unpaired: enter a pairing code from the node")
			return
		}

		sess, err := sessions.ValidateSession(r.Context(), token)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unpaired: session invalid, expired, or revoked")
			return
		}

		// Inject authenticated session into context
		ctx := context.WithValue(r.Context(), sessionContextKey, sess)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func extractToken(r *http.Request) string {
	// 1. Check HttpOnly cookie
	if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	// 2. Check Authorization: Bearer <token>
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	// 3. Check X-Phone-Token header (for testing / programmatic API)
	if tok := r.Header.Get("X-Phone-Token"); tok != "" {
		return tok
	}
	return ""
}

// SetSessionCookie sets the HttpOnly session cookie on ResponseWriter.
func SetSessionCookie(w http.ResponseWriter, token string, secure bool) {
	cookie := &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   30 * 24 * 3600, // 30 days
	}
	http.SetCookie(w, cookie)
}

// ClearSessionCookie clears the session cookie.
func ClearSessionCookie(w http.ResponseWriter) {
	cookie := &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
	}
	http.SetCookie(w, cookie)
}
