package phone

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", "file::memory:?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestSessionStorePairingAndRedemption(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 28, 5, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	db := newTestDB(t)
	store, err := NewSessionStore(db, clock)
	if err != nil {
		t.Fatalf("new session store: %v", err)
	}

	// 1. Create pairing code
	code, err := store.CreatePairingCode(ctx, "Field Tech Alice", 15*time.Minute)
	if err != nil {
		t.Fatalf("create pairing code: %v", err)
	}
	if len(code.Code) != 7 || code.Code[3] != '-' {
		t.Fatalf("pairing code format mismatch: got %q, want XXX-XXX", code.Code)
	}
	if code.Label != "Field Tech Alice" {
		t.Errorf("label mismatch: got %q", code.Label)
	}

	// 2. Validate unredeemed token fails
	if _, err := store.ValidateSession(ctx, "nonexistent-token"); err != ErrSessionInvalid {
		t.Errorf("expected ErrSessionInvalid, got %v", err)
	}

	// 3. Redeem pairing code (with or without dashes)
	sess, err := store.RedeemPairingCode(ctx, code.Code, "Mobile Chrome")
	if err != nil {
		t.Fatalf("redeem pairing code: %v", err)
	}
	if len(sess.Token) != 64 {
		t.Errorf("expected 64-hex char token, got length %d (%q)", len(sess.Token), sess.Token)
	}
	if sess.Revoked {
		t.Errorf("new session should not be revoked")
	}

	// 4. Redeeming again fails with ErrCodeUsed
	if _, err := store.RedeemPairingCode(ctx, code.Code, "Another Device"); err != ErrCodeUsed {
		t.Errorf("expected ErrCodeUsed on second redemption, got %v", err)
	}

	// 5. Validate redeemed session succeeds
	vSess, err := store.ValidateSession(ctx, sess.Token)
	if err != nil {
		t.Fatalf("validate session failed: %v", err)
	}
	if vSess.Token != sess.Token {
		t.Errorf("token mismatch: got %q, want %q", vSess.Token, sess.Token)
	}

	// 6. List sessions
	sessions, err := store.ListSessions(ctx)
	if err != nil {
		t.Fatalf("list sessions failed: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].Token != sess.Token {
		t.Errorf("session token mismatch")
	}

	// 7. Revoke session
	n, err := store.RevokeSession(ctx, sess.Token[:12])
	if err != nil {
		t.Fatalf("revoke session failed: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 revoked row, got %d", n)
	}

	// 8. Validating revoked session returns ErrSessionInvalid
	if _, err := store.ValidateSession(ctx, sess.Token); err != ErrSessionInvalid {
		t.Errorf("expected ErrSessionInvalid for revoked session, got %v", err)
	}
}

func TestSessionStoreExpiryAndCleanup(t *testing.T) {
	ctx := context.Background()
	current := time.Date(2026, 11, 28, 5, 0, 0, 0, time.UTC)
	clock := func() time.Time { return current }

	db := newTestDB(t)
	store, err := NewSessionStore(db, clock)
	if err != nil {
		t.Fatalf("new session store: %v", err)
	}

	// Create pairing code with 5m TTL
	code, err := store.CreatePairingCode(ctx, "Test Phone", 5*time.Minute)
	if err != nil {
		t.Fatalf("create pairing code: %v", err)
	}

	// Advance time by 6 minutes
	current = current.Add(6 * time.Minute)

	// Redemption should fail with ErrCodeExpired
	if _, err := store.RedeemPairingCode(ctx, code.Code, "Late Phone"); err != ErrCodeExpired {
		t.Errorf("expected ErrCodeExpired, got %v", err)
	}

	// Clean expired codes
	cleaned, err := store.CleanExpired(ctx)
	if err != nil {
		t.Fatalf("clean expired failed: %v", err)
	}
	if cleaned != 1 {
		t.Errorf("expected 1 cleaned pairing code, got %d", cleaned)
	}
}

func TestSessionStoreConcurrency(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	store, err := NewSessionStore(db, time.Now)
	if err != nil {
		t.Fatalf("new session store: %v", err)
	}

	const n = 30
	var wg sync.WaitGroup
	codes := make([]*PairingCode, n)

	// Create pairing codes concurrently
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			c, err := store.CreatePairingCode(ctx, fmt.Sprintf("phone-%d", idx), 10*time.Minute)
			if err != nil {
				t.Errorf("concurrency create %d: %v", idx, err)
				return
			}
			codes[idx] = c
		}(i)
	}
	wg.Wait()

	// Redeem pairing codes concurrently
	sessions := make([]*Session, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			s, err := store.RedeemPairingCode(ctx, codes[idx].Code, fmt.Sprintf("client-%d", idx))
			if err != nil {
				t.Errorf("concurrency redeem %d: %v", idx, err)
				return
			}
			sessions[idx] = s
		}(i)
	}
	wg.Wait()

	// Validate sessions concurrently
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			v, err := store.ValidateSession(ctx, sessions[idx].Token)
			if err != nil || v == nil {
				t.Errorf("concurrency validate %d: %v", idx, err)
			}
		}(i)
	}
	wg.Wait()
}
