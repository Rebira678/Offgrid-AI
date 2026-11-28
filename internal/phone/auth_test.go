package phone

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAuthAndSecurityHeaders(t *testing.T) {
	handler := SecurityMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	handler.ServeHTTP(rec, req)

	// Verify security headers
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("CSP missing default-src 'none': got %q", csp)
	}
	if nosniff := rec.Header().Get("X-Content-Type-Options"); nosniff != "nosniff" {
		t.Errorf("X-Content-Type-Options mismatch: got %q", nosniff)
	}
	if frame := rec.Header().Get("X-Frame-Options"); frame != "DENY" {
		t.Errorf("X-Frame-Options mismatch: got %q", frame)
	}
}

func TestUnpairedPhoneGetsNothing(t *testing.T) {
	db := newTestDB(t)
	sessions, err := NewSessionStore(db, time.Now)
	if err != nil {
		t.Fatalf("session store: %v", err)
	}

	protectedHit := false
	protectedHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protectedHit = true
		w.WriteHeader(http.StatusOK)
	})

	authHandler := AuthMiddleware(sessions, true, protectedHandler)

	endpoints := []string{
		"/api/phone/tasks",
		"/api/phone/suggestions",
		"/api/phone/sync/replay",
		"/api/phone/tasks/123/status",
	}

	for _, ep := range endpoints {
		protectedHit = false
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", ep, nil)
		authHandler.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expected 401 Unauthorized for unpaired client at %s, got %d", ep, rec.Code)
		}
		if protectedHit {
			t.Errorf("protected handler was called for unpaired request at %s", ep)
		}
	}
}

func TestPairedPhoneCookieAndBearer(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions, err := NewSessionStore(db, time.Now)
	if err != nil {
		t.Fatalf("session store: %v", err)
	}

	code, _ := sessions.CreatePairingCode(ctx, "Test Phone", 15*time.Minute)
	sess, err := sessions.RedeemPairingCode(ctx, code.Code, "Client #1")
	if err != nil {
		t.Fatalf("redeem failed: %v", err)
	}

	protectedHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, ok := SessionFromContext(r.Context())
		if !ok || s == nil {
			t.Errorf("session not found in context")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(s.Label))
	})

	authHandler := AuthMiddleware(sessions, true, protectedHandler)

	// 1. Authenticate via HttpOnly Cookie
	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest("GET", "/api/phone/tasks", nil)
	req1.AddCookie(&http.Cookie{
		Name:  SessionCookieName,
		Value: sess.Token,
	})
	authHandler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Errorf("cookie auth failed: got status %d", rec1.Code)
	}

	// 2. Authenticate via Authorization Bearer
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/api/phone/tasks", nil)
	req2.Header.Set("Authorization", "Bearer "+sess.Token)
	authHandler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Errorf("bearer auth failed: got status %d", rec2.Code)
	}
}

func TestCSRFProtection(t *testing.T) {
	handler := SecurityMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// 1. POST without custom header -> 403 Forbidden
	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest("POST", "/api/phone/tasks", strings.NewReader(`{}`))
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden for missing CSRF header, got %d", rec1.Code)
	}

	// 2. POST with mismatched Origin -> 403 Forbidden
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/api/phone/tasks", strings.NewReader(`{}`))
	req2.Header.Set("X-Offgrid-Client", "phone")
	req2.Header.Set("Origin", "http://evil.com")
	req2.Host = "192.168.1.100:8443"
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden for mismatched origin, got %d", rec2.Code)
	}

	// 3. POST with Sec-Fetch-Site cross-site -> 403 Forbidden
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("POST", "/api/phone/tasks", strings.NewReader(`{}`))
	req3.Header.Set("X-Offgrid-Client", "phone")
	req3.Header.Set("Sec-Fetch-Site", "cross-site")
	handler.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden for cross-site fetch, got %d", rec3.Code)
	}

	// 4. Valid POST with proper header and origin -> 200 OK
	rec4 := httptest.NewRecorder()
	req4 := httptest.NewRequest("POST", "/api/phone/tasks", strings.NewReader(`{}`))
	req4.Header.Set("X-Offgrid-Client", "phone")
	req4.Header.Set("Origin", "http://192.168.1.100:8443")
	req4.Host = "192.168.1.100:8443"
	handler.ServeHTTP(rec4, req4)
	if rec4.Code != http.StatusOK {
		t.Errorf("expected 200 OK for valid security headers, got %d", rec4.Code)
	}
}
