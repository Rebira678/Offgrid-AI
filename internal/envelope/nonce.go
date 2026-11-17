// Package envelope holds the one piece of the Day 42 design that can be tested before anything is built: how
// nonces are derived, so that no (key, nonce) pair is ever used twice. The rest of ADR 009 (wrapping keys to
// members, the AEAD itself) is not implemented here, and nothing in Offgrid calls this package yet.
package envelope

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
)

// NonceSize is the size of a ChaCha20-Poly1305 nonce (12 bytes / 96 bits).
const NonceSize = 12

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
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.next[p]
	if c >= s.limit {
		return [NonceSize]byte{}, ErrExhausted
	}
	s.next[p] = c + 1
	return Nonce(p, c), nil
}
