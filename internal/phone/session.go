package phone

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"
)

var (
	ErrCodeInvalid    = errors.New("pairing code is invalid")
	ErrCodeExpired    = errors.New("pairing code has expired")
	ErrCodeUsed       = errors.New("pairing code has already been redeemed")
	ErrSessionInvalid = errors.New("session is invalid or has been revoked")
)

const (
	DefaultPairingTTL = 15 * time.Minute
	MaxPairingTTL     = 24 * time.Hour
)

// SessionStore manages pairing codes and authenticated phone sessions in SQLite.
type SessionStore struct {
	db  *sql.DB
	now func() time.Time
	mu  sync.RWMutex
}

// NewSessionStore creates or initializes the phone pairing and session tables.
func NewSessionStore(db *sql.DB, now func() time.Time) (*SessionStore, error) {
	if db == nil {
		return nil, errors.New("phone: nil database handle")
	}
	if now == nil {
		now = time.Now
	}

	schema := `
	CREATE TABLE IF NOT EXISTS phone_pairings (
		code TEXT PRIMARY KEY,
		label TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL,
		redeemed_at INTEGER
	);
	CREATE TABLE IF NOT EXISTS phone_sessions (
		token TEXT PRIMARY KEY,
		label TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		last_seen_at INTEGER NOT NULL,
		revoked INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_phone_sessions_revoked ON phone_sessions(revoked);
	`
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("phone: init session tables: %w", err)
	}

	return &SessionStore{db: db, now: now}, nil
}

// NormalizeCode standardizes pairing codes by removing spaces and dashes.
func NormalizeCode(code string) string {
	code = strings.TrimSpace(code)
	code = strings.ReplaceAll(code, "-", "")
	code = strings.ReplaceAll(code, " ", "")
	return code
}

// FormatCode returns the human-friendly 6-digit hyphenated format (123-456).
func FormatCode(code string) string {
	raw := NormalizeCode(code)
	if len(raw) == 6 {
		return raw[:3] + "-" + raw[3:]
	}
	return raw
}

// CreatePairingCode generates a human-friendly 6-digit code with TTL.
func (s *SessionStore) CreatePairingCode(ctx context.Context, label string, ttl time.Duration) (*PairingCode, error) {
	if ttl <= 0 {
		ttl = DefaultPairingTTL
	}
	if ttl > MaxPairingTTL {
		ttl = MaxPairingTTL
	}
	if label == "" {
		label = "Field Phone"
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now().UTC()
	expires := now.Add(ttl)

	var codeRaw string
	var insertErr error
	for attempt := 0; attempt < 5; attempt++ {
		n, err := rand.Int(rand.Reader, big.NewInt(1000000))
		if err != nil {
			return nil, fmt.Errorf("phone: generate pairing code: %w", err)
		}
		codeRaw = fmt.Sprintf("%06d", n.Int64())

		_, insertErr = s.db.ExecContext(ctx, `
			INSERT INTO phone_pairings (code, label, created_at, expires_at, redeemed_at)
			VALUES (?, ?, ?, ?, NULL)
		`, codeRaw, label, now.UnixNano(), expires.UnixNano())
		if insertErr == nil {
			break
		}
	}
	if insertErr != nil {
		return nil, fmt.Errorf("phone: insert pairing code: %w", insertErr)
	}

	codeFmt := FormatCode(codeRaw)

	return &PairingCode{
		Code:      codeFmt,
		Label:     label,
		CreatedAt: now,
		ExpiresAt: expires,
	}, nil
}

// RedeemPairingCode redeems a pairing code and returns a newly minted session token.
func (s *SessionStore) RedeemPairingCode(ctx context.Context, rawCode, clientLabel string) (*Session, error) {
	code := NormalizeCode(rawCode)
	if len(code) != 6 {
		return nil, ErrCodeInvalid
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now().UTC()
	nowNano := now.UnixNano()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("phone: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		label      string
		expiresAt  int64
		redeemedAt sql.NullInt64
	)
	err = tx.QueryRowContext(ctx, `
		SELECT label, expires_at, redeemed_at FROM phone_pairings WHERE code = ?
	`, code).Scan(&label, &expiresAt, &redeemedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrCodeInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("phone: query pairing: %w", err)
	}

	if redeemedAt.Valid {
		return nil, ErrCodeUsed
	}
	if nowNano > expiresAt {
		return nil, ErrCodeExpired
	}

	// Mark pairing code redeemed
	if _, err := tx.ExecContext(ctx, `UPDATE phone_pairings SET redeemed_at = ? WHERE code = ?`, nowNano, code); err != nil {
		return nil, fmt.Errorf("phone: mark code redeemed: %w", err)
	}

	// Generate 32 bytes (256 bits) cryptographically secure random session token
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, fmt.Errorf("phone: generate session token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)

	effectiveLabel := label
	if clientLabel != "" {
		effectiveLabel = fmt.Sprintf("%s (%s)", label, clientLabel)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO phone_sessions (token, label, created_at, last_seen_at, revoked)
		VALUES (?, ?, ?, ?, 0)
	`, token, effectiveLabel, nowNano, nowNano)
	if err != nil {
		return nil, fmt.Errorf("phone: save session: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("phone: commit pairing redemption: %w", err)
	}

	return &Session{
		Token:      token,
		Label:      effectiveLabel,
		CreatedAt:  now,
		LastSeenAt: now,
		Revoked:    false,
	}, nil
}

// ValidateSession verifies if a session token is active and unrevoked.
func (s *SessionStore) ValidateSession(ctx context.Context, token string) (*Session, error) {
	if token == "" {
		return nil, ErrSessionInvalid
	}

	s.mu.RLock()
	var (
		label      string
		createdAt  int64
		lastSeenAt int64
		revoked    int
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT label, created_at, last_seen_at, revoked FROM phone_sessions WHERE token = ?
	`, token).Scan(&label, &createdAt, &lastSeenAt, &revoked)
	s.mu.RUnlock()

	if errors.Is(err, sql.ErrNoRows) || revoked != 0 {
		return nil, ErrSessionInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("phone: validate session: %w", err)
	}

	now := s.now().UTC()
	// Update last_seen_at asynchronously or best-effort
	go func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		_, _ = s.db.Exec(`UPDATE phone_sessions SET last_seen_at = ? WHERE token = ?`, now.UnixNano(), token)
	}()

	return &Session{
		Token:      token,
		Label:      label,
		CreatedAt:  time.Unix(0, createdAt).UTC(),
		LastSeenAt: now,
		Revoked:    false,
	}, nil
}

// RevokeSession revokes a session by full token or prefix.
func (s *SessionStore) RevokeSession(ctx context.Context, tokenOrPrefix string) (int64, error) {
	tokenOrPrefix = strings.TrimSpace(tokenOrPrefix)
	if tokenOrPrefix == "" {
		return 0, errors.New("phone: empty token for revocation")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var res sql.Result
	var err error
	if len(tokenOrPrefix) == 64 {
		res, err = s.db.ExecContext(ctx, `UPDATE phone_sessions SET revoked = 1 WHERE token = ? AND revoked = 0`, tokenOrPrefix)
	} else {
		prefix := tokenOrPrefix + "%"
		res, err = s.db.ExecContext(ctx, `UPDATE phone_sessions SET revoked = 1 WHERE token LIKE ? AND revoked = 0`, prefix)
	}
	if err != nil {
		return 0, fmt.Errorf("phone: revoke session: %w", err)
	}
	return res.RowsAffected()
}

// ListSessions lists all sessions in the store.
func (s *SessionStore) ListSessions(ctx context.Context) ([]*Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT token, label, created_at, last_seen_at, revoked FROM phone_sessions ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("phone: list sessions: %w", err)
	}
	defer rows.Close()

	var out []*Session
	for rows.Next() {
		var (
			token      string
			label      string
			createdAt  int64
			lastSeenAt int64
			revoked    int
		)
		if err := rows.Scan(&token, &label, &createdAt, &lastSeenAt, &revoked); err != nil {
			return nil, err
		}
		out = append(out, &Session{
			Token:      token,
			Label:      label,
			CreatedAt:  time.Unix(0, createdAt).UTC(),
			LastSeenAt: time.Unix(0, lastSeenAt).UTC(),
			Revoked:    revoked != 0,
		})
	}
	return out, rows.Err()
}

// CleanExpired removes pairing codes that expired without redemption.
func (s *SessionStore) CleanExpired(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	nowNano := s.now().UTC().UnixNano()
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM phone_pairings WHERE redeemed_at IS NULL AND expires_at < ?
	`, nowNano)
	if err != nil {
		return 0, fmt.Errorf("phone: clean expired pairings: %w", err)
	}
	return res.RowsAffected()
}
