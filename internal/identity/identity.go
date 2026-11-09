// Package identity is a device's cryptographic identity: one ECDSA P-256 key
// per device (not per person), and a device ID that is a hash of its public
// key. Attribution is to a device; a human label comes from enrollment (later).
// A lost phone means revoking its key.
//
// Two rules shape the file format:
//
//   - Everything that leaves this package carries an algorithm tag
//     ("ecdsa-p256:..."), so changing the algorithm later is a new tag, not a
//     break with old data. A file or key with a tag this build does not know is
//     ErrUnsupported ("probably from a newer Offgrid"), never "corrupt".
//   - The key is never replaced silently. A missing file may be created; a file
//     that is wrongly protected, corrupt, or from a newer version is an error,
//     because making a new key under it would change the device's identity.
//
// Standard library only (crypto/ecdsa, crypto/ecdh, crypto/rand, crypto/sha256).
// Nothing here logs, and nothing printed or encoded from an *Identity contains
// the private key.
package identity

import (
	"errors"
)

// AlgECDSAP256 is the only algorithm this build can make.
const AlgECDSAP256 = "ecdsa-p256"

const (
	fileMagic   = "offgrid-key"
	fileVersion = "v1"
	maxFileSize = 8 << 10
	idPrefix    = "dev_"
	idHexChars  = 32 // 128 bits of the SHA-256
)

var (
	// ErrNotFound: there is no key file at that path (nothing was created).
	ErrNotFound = errors.New("no identity key")
	// ErrExists: Create will not overwrite an existing key.
	ErrExists = errors.New("an identity key already exists")
	// ErrPermissions: the file can be read by someone other than its owner.
	ErrPermissions = errors.New("identity key file is too open")
	// ErrCorrupt: the file is not a key this format could have written.
	ErrCorrupt = errors.New("identity key file is corrupt")
	// ErrUnsupported: a well-formed file or key with a version or algorithm this build does not know.
	ErrUnsupported = errors.New("identity key uses an unsupported version or algorithm")
)
