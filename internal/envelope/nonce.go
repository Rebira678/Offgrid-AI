// Package envelope holds the one piece of the Day 42 design that can be tested before anything is built: how
// nonces are derived, so that no (key, nonce) pair is ever used twice. The rest of ADR 009 (wrapping keys to
// members, the AEAD itself) is not implemented here, and nothing in Offgrid calls this package yet.
package envelope

import (
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
)

// NonceSize is the size of a ChaCha20-Poly1305 nonce (12 bytes / 96 bits).
const NonceSize = 12

// NonceHexLen is the string length of a hex-encoded nonce (24 hex characters).
const NonceHexLen = NonceSize * 2

// Purpose says what a nonce is for. It takes the first byte, so the same counter under the same key for two
// purposes still gives two nonces.
type Purpose byte

const (
	// PurposeBody seals an op's payload under its one-time content key.
	PurposeBody Purpose = 1
	// PurposeWrap seals the content key for one member.
	PurposeWrap Purpose = 2
)

// Nonce is purpose (1 byte), three zero bytes, then the counter, big-endian. A counter, not random bytes:
// with 96-bit random nonces a collision is only improbable, and "improbable" is the wrong word for a rule
// that a broken device must not be able to turn into a key leak.
func Nonce(p Purpose, counter uint64) [NonceSize]byte {
	var n [NonceSize]byte
	n[0] = byte(p)
	binary.BigEndian.PutUint64(n[4:], counter)
	return n
}

// Valid reports whether the purpose is one of the recognized envelope purposes.
func (p Purpose) Valid() bool {
	return p == PurposeBody || p == PurposeWrap
}

// String returns a human-readable identifier for the purpose.
func (p Purpose) String() string {
	switch p {
	case PurposeBody:
		return "body"
	case PurposeWrap:
		return "wrap"
	default:
		return fmt.Sprintf("unknown(%d)", byte(p))
	}
}

// ErrExhausted means a key has used all the nonces it may. The caller must pick a new key.
var ErrExhausted = errors.New("envelope: this key has used all its nonces; use a new key")

// ErrInvalidPurpose indicates an unassigned or invalid purpose byte.
var ErrInvalidPurpose = errors.New("envelope: invalid or unassigned nonce purpose")

// ErrMalformedNonce indicates that the nonce padding bytes (indices 1..3) are non-zero.
var ErrMalformedNonce = errors.New("envelope: malformed nonce (padding bytes must be zero)")

// ErrInvalidNonceSize indicates a byte slice does not match NonceSize (12 bytes).
var ErrInvalidNonceSize = errors.New("envelope: invalid nonce byte slice length (expected 12 bytes)")

// ErrInvalidNonceHex indicates a hex string does not represent a valid 12-byte nonce.
var ErrInvalidNonceHex = errors.New("envelope: invalid nonce hex representation")

// ErrNilScope indicates an operation was attempted on a nil Scope pointer.
var ErrNilScope = errors.New("envelope: nil scope")

// DefaultLimit is how many nonces one key may use per purpose. A content key is one-time (limit 1 would do);
// the limit exists so a bug that reuses a scope fails loudly long before 2^32 messages.
const DefaultLimit = 1 << 32

// Scope hands out nonces for one key. It is safe for concurrent use. Each purpose counts separately and
// never goes backwards, and a Scope never hands out the same nonce twice.
type Scope struct {
	mu    sync.Mutex
	next  [256]uint64
	limit uint64
}

// NewScope starts a scope for a new key with DefaultLimit.
func NewScope() *Scope {
	return &Scope{limit: DefaultLimit}
}

// NewScopeWithLimit starts a scope for a new key with a custom maximum nonces limit per purpose.
func NewScopeWithLimit(limit uint64) *Scope {
	return &Scope{limit: limit}
}

// Count returns the number of nonces already issued for the given purpose.
func (s *Scope) Count(p Purpose) uint64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.next[p]
}

// Remaining returns how many nonces remain available for the purpose before hitting limit.
func (s *Scope) Remaining(p Purpose) uint64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.next[p]
	if c >= s.limit {
		return 0
	}
	return s.limit - c
}

// Next returns the next unused nonce for the purpose.
func (s *Scope) Next(p Purpose) ([NonceSize]byte, error) {
	if s == nil {
		return [NonceSize]byte{}, ErrNilScope
	}
	if !p.Valid() {
		return [NonceSize]byte{}, ErrInvalidPurpose
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.next[p]
	if c >= s.limit || c == ^uint64(0) {
		return [NonceSize]byte{}, ErrExhausted
	}
	s.next[p] = c + 1
	return Nonce(p, c), nil
}

// ParseNonce unpacks and validates a 12-byte nonce according to the ADR 009 layout.
// It verifies that bytes 1..3 are zero padding, that the purpose is recognized,
// and extracts the 64-bit big-endian counter.
func ParseNonce(n [NonceSize]byte) (Purpose, uint64, error) {
	p := Purpose(n[0])
	if !p.Valid() {
		return 0, 0, ErrInvalidPurpose
	}
	if n[1] != 0 || n[2] != 0 || n[3] != 0 {
		return 0, 0, ErrMalformedNonce
	}
	counter := binary.BigEndian.Uint64(n[4:])
	return p, counter, nil
}

// NonceFromBytes copies and validates a 12-byte slice into a Nonce array.
func NonceFromBytes(b []byte) ([NonceSize]byte, error) {
	if len(b) != NonceSize {
		return [NonceSize]byte{}, ErrInvalidNonceSize
	}
	var n [NonceSize]byte
	copy(n[:], b)
	if _, _, err := ParseNonce(n); err != nil {
		return [NonceSize]byte{}, err
	}
	return n, nil
}

// NonceFromHex decodes and validates a 24-character hex string into a Nonce array.
func NonceFromHex(s string) ([NonceSize]byte, error) {
	if len(s) != NonceHexLen {
		return [NonceSize]byte{}, ErrInvalidNonceHex
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return [NonceSize]byte{}, ErrInvalidNonceHex
	}
	return NonceFromBytes(b)
}

// NonceHex returns the 24-character lowercase hex string representation of the nonce.
func NonceHex(n [NonceSize]byte) string {
	return hex.EncodeToString(n[:])
}

// Equal returns true if two nonces are byte-for-byte identical in constant time.
func Equal(a, b [NonceSize]byte) bool {
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}
