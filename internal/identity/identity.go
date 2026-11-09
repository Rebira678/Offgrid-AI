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
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// Identity is a device's key. The zero value is not usable.
type Identity struct {
	key *ecdsa.PrivateKey
	pub []byte // 65 bytes: 0x04 || X || Y
	id  string
}

func newIdentity(k *ecdsa.PrivateKey) (*Identity, error) {
	e, err := k.PublicKey.ECDH()
	if err != nil {
		return nil, err
	}
	raw := e.Bytes()
	return &Identity{key: k, pub: raw, id: deviceID(raw)}, nil
}

// Generate makes a new identity in memory. Nothing is written.
func Generate() (*Identity, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("identity: generate key: %w", err)
	}
	return newIdentity(k)
}

func deviceID(raw []byte) string {
	h := sha256.Sum256(raw)
	return idPrefix + hex.EncodeToString(h[:])[:idHexChars]
}

// ID is the device ID: "dev_" and 32 hex characters of the SHA-256 of the
// 65-byte public key. The phone check page (Day 33) derives it the same way.
func (i *Identity) ID() string {
	return i.id
}

// Algorithm is the tag of the key's algorithm.
func (i *Identity) Algorithm() string { return AlgECDSAP256 }

// PublicKey is the tagged public key: "ecdsa-p256:" and the base64url of the 65 raw bytes.
func (i *Identity) PublicKey() string {
	return AlgECDSAP256 + ":" + base64.RawURLEncoding.EncodeToString(i.pub)
}

// Sign signs msg (SHA-256 then ECDSA) and returns the 64-byte raw r||s form,
// which is what WebCrypto produces, so a phone and a node agree on the format.
func (i *Identity) Sign(msg []byte) ([]byte, error) {
	h := sha256.Sum256(msg)
	r, s, err := ecdsa.Sign(rand.Reader, i.key, h[:])
	if err != nil {
		return nil, fmt.Errorf("identity: sign: %w", err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return sig, nil
}

func parsePublic(tagged string) (*ecdsa.PublicKey, []byte, error) {
	alg, enc, ok := strings.Cut(tagged, ":")
	if !ok {
		return nil, nil, errors.New("public key has no algorithm tag (want \"ecdsa-p256:...\")")
	}
	if alg != AlgECDSAP256 {
		return nil, nil, fmt.Errorf("%w: public key algorithm %q", ErrUnsupported, trunc(alg))
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(enc)
	if err != nil || len(raw) != 65 {
		return nil, nil, errors.New("public key is not 65 bytes of base64url")
	}
	if _, err := ecdh.P256().NewPublicKey(raw); err != nil { // checks the point is on the curve
		return nil, nil, errors.New("public key is not a point on P-256")
	}
	return &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(raw[1:33]), Y: new(big.Int).SetBytes(raw[33:])}, raw, nil
}

func trunc(s string) string {
	if len(s) > 24 {
		return s[:24] + "..."
	}
	return s
}

// DeviceID derives the device ID from a tagged public key.
func DeviceID(tagged string) (string, error) {
	_, raw, err := parsePublic(tagged)
	if err != nil {
		return "", err
	}
	return deviceID(raw), nil
}

// Verify checks a 64-byte raw r||s signature over msg against a tagged public key.
func Verify(tagged string, msg, sig []byte) error {
	pub, _, err := parsePublic(tagged)
	if err != nil {
		return err
	}
	if len(sig) != 64 {
		return errors.New("signature is not 64 bytes")
	}
	h := sha256.Sum256(msg)
	if !ecdsa.Verify(pub, h[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return errors.New("signature does not verify")
	}
	return nil
}

// The file is one line: "offgrid-key v1 ecdsa-p256 <base64url PKCS#8, no padding>\n".
func (i *Identity) encode() []byte {
	der, err := x509.MarshalPKCS8PrivateKey(i.key)
	if err != nil { // cannot happen for a P-256 key
		panic("identity: " + err.Error())
	}
	return []byte(fileMagic + " " + fileVersion + " " + AlgECDSAP256 + " " + base64.RawURLEncoding.EncodeToString(der) + "\n")
}

func parseKeyFile(data []byte) (*Identity, error) {
	corrupt := func(why string) error { return fmt.Errorf("%w: %s", ErrCorrupt, why) }
	if len(data) > maxFileSize {
		return nil, corrupt("it is too large to be a key")
	}
	if len(data) == 0 || data[len(data)-1] != '\n' || strings.Count(string(data), "\n") != 1 {
		return nil, corrupt("it is not exactly one line")
	}
	f := strings.Split(string(data[:len(data)-1]), " ")
	if len(f) != 4 || f[0] != fileMagic {
		return nil, corrupt("it does not start with \"" + fileMagic + " <version> <algorithm> <key>\"")
	}
	if f[1] != fileVersion || f[2] != AlgECDSAP256 {
		return nil, fmt.Errorf("%w: version %q, algorithm %q (this build reads %s %s); the file may come from a newer Offgrid", ErrUnsupported, trunc(f[1]), trunc(f[2]), fileVersion, AlgECDSAP256)
	}
	der, err := base64.RawURLEncoding.Strict().DecodeString(f[3])
	if err != nil {
		return nil, corrupt("the key is not base64url")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, corrupt("the key is not a PKCS#8 private key")
	}
	k, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || k.Curve != elliptic.P256() {
		return nil, corrupt("the key is not an ECDSA P-256 key")
	}
	id, err := newIdentity(k)
	if err != nil {
		return nil, corrupt("the key is not usable")
	}
	return id, nil
}

// DefaultPath is where a device keeps its key unless told otherwise.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("identity: cannot find a config folder (%w); give the key path explicitly", err)
	}
	return filepath.Join(dir, "offgrid", "device.key"), nil
}

// Load reads the key at path. It refuses a file that anyone but its owner can
// read (the mode of the file that is actually opened counts, so a symlink to a
// readable file is refused too). It never creates or changes anything.
func Load(path string) (*Identity, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w at %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("identity: open %s: %w", path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("identity: stat %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrCorrupt, path)
	}
	// Windows reports 0666 or 0444 whatever the ACL says, so the bits mean nothing there.
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%w: %s has mode %04o, but it holds a private key and must be readable by its owner only; fix it with: chmod 600 %q", ErrPermissions, path, fi.Mode().Perm(), path)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("identity: read %s: %w", path, err)
	}
	id, err := parseKeyFile(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return id, nil
}
