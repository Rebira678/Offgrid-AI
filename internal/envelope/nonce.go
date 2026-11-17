// Package envelope holds the one piece of the Day 42 design that can be tested before anything is built: how
// nonces are derived, so that no (key, nonce) pair is ever used twice. The rest of ADR 009 (wrapping keys to
// members, the AEAD itself) is not implemented here, and nothing in Offgrid calls this package yet.
package envelope

import (
	"encoding/binary"
	"fmt"
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
