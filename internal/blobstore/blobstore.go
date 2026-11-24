// Package blobstore keeps attachments (photos, PDFs) as files named by the SHA-256 of their bytes.
//
// Blobs stay out of the op log on purpose: the log must stay small enough to sync, verify and keep
// forever, and a 4 MB photo in it would make every catch-up, every Verify and every backup carry
// it. The log carries only a reference (hash, size, type); the bytes live here, one file per hash.
//
// Rules this package keeps:
//   - A file is named by its content, so a stored blob is its own checksum, and the same photo
//     added twice, or arriving from two peers, is stored once.
//   - Bytes are streamed through a hasher into a temporary file, fsynced, then moved into place
//     with an atomic rename. A crash leaves either no blob or a whole one, never half of one under
//     its final name; leftovers in tmp/ are swept when the store is opened.
//   - A blob is never read fully into memory (32 KiB at a time), whatever its size.
//   - The size cap is enforced before the body is read when the sender declares a size, and while
//     reading when it does not or lies: at most cap+1 bytes are ever consumed.
//   - Bytes received from a peer are checked against the hash the peer was asked for; a mismatch
//     keeps nothing.
//   - A hash is accepted only as 64 lowercase hex characters, so it can never name a path outside
//     the directory.
//
// A directory belongs to one process. Free-space checks, reference counting and garbage
// collection are not here yet (Day 48).
package blobstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultMaxBytes is the cap on one blob when none is configured: generous for a phone photo, small
// enough that one mistake cannot fill an SD card.
const DefaultMaxBytes = 25 << 20

const (
	tmpDir  = "tmp"
	bufSize = 32 << 10
)

var bufPool = sync.Pool{
	New: func() any {
		b := make([]byte, bufSize)
		return &b
	},
}

func getBuf() *[]byte  { return bufPool.Get().(*[]byte) }
func putBuf(b *[]byte) { bufPool.Put(b) }

var (
	ErrTooLarge     = errors.New("blobstore: the blob is over the size cap")
	ErrEmpty        = errors.New("blobstore: an empty blob is not an attachment")
	ErrHashMismatch = errors.New("blobstore: the bytes do not match their hash")
	ErrSizeMismatch = errors.New("blobstore: the number of bytes does not match the declared size")
	ErrBadHash      = errors.New("blobstore: a hash is 64 lowercase hex characters")
	ErrNotFound     = errors.New("blobstore: no such blob")
)

// Ref names a stored blob.
type Ref struct {
	Hash string // lowercase hex SHA-256
	Size int64
}

// Store is a directory of blobs.
type Store struct {
	dir string
	max int64

	mu      sync.Mutex
	active  map[string]bool // blobs with a Transfer open
	free    FreeFunc
	reserve int64
}

// New opens (creating if needed) the blob directory dir with a per-blob cap of max bytes
// (DefaultMaxBytes if max <= 0). Temporary files left by a crash are removed.
func New(dir string, max int64) (*Store, error) {
	if max <= 0 {
		max = DefaultMaxBytes
	}
	if err := os.MkdirAll(filepath.Join(dir, tmpDir), 0o700); err != nil {
		return nil, fmt.Errorf("blobstore: %w", err)
	}
	// MkdirAll leaves an existing directory's mode alone; blobs may be private photos.
	_ = os.Chmod(dir, 0o700)
	_ = os.Chmod(filepath.Join(dir, tmpDir), 0o700)
	es, err := os.ReadDir(filepath.Join(dir, tmpDir))
	if err != nil {
		return nil, fmt.Errorf("blobstore: %w", err)
	}
	for _, e := range es {
		p := filepath.Join(dir, tmpDir, e.Name())
		if strings.HasPrefix(e.Name(), "part-") { // an unfinished transfer: keep it for a resume unless it is abandoned
			if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) < PartialTTL {
				continue
			}
		}
		_ = os.Remove(p)
	}
	return &Store{dir: dir, max: max, active: map[string]bool{}, reserve: DefaultReserve}, nil
}

// Dir is the directory the blobs are in.
func (s *Store) Dir() string { return s.dir }

// MaxBytes is the per-blob cap.
func (s *Store) MaxBytes() int64 { return s.max }

// ValidHash reports whether h is a well-formed blob hash.
func ValidHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (s *Store) path(h string) string { return filepath.Join(s.dir, h[:2], h[2:]) }

// Has reports whether a blob with this hash is stored. It does not re-hash it (see Verify).
func (s *Store) Has(h string) bool {
	if !ValidHash(h) {
		return false
	}
	fi, err := os.Stat(s.path(h))
	return err == nil && fi.Mode().IsRegular()
}

// Open returns the blob's file for reading. The caller closes it.
func (s *Store) Open(h string) (*os.File, error) {
	if !ValidHash(h) {
		return nil, ErrBadHash
	}
	f, err := os.Open(s.path(h))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return f, err
}

// Verify re-hashes the stored blob and reports ErrHashMismatch if its bytes are no longer the ones
// its name promises (bit rot, a bad SD card, a hand edit).
func (s *Store) Verify(h string) error {
	f, err := s.Open(h)
	if err != nil {
		return err
	}
	defer f.Close()
	hs := sha256.New()
	bufPtr := getBuf()
	defer putBuf(bufPtr)
	if _, err := io.CopyBuffer(hs, f, *bufPtr); err != nil {
		return err
	}
	if hex.EncodeToString(hs.Sum(nil)) != h {
		return ErrHashMismatch
	}
	return nil
}

// Put stores everything r yields and names it by its hash. declared is the size the sender claimed,
// or -1 if unknown: when known and over the cap nothing is read.
func (s *Store) Put(ctx context.Context, r io.Reader, declared int64) (Ref, error) {
	return s.put(ctx, r, "", declared)
}

// PutExpected stores the bytes of r only if they hash to want and number size: this is how a blob
// received from a peer is accepted. Anything else keeps nothing.
func (s *Store) PutExpected(ctx context.Context, r io.Reader, want string, size int64) (Ref, error) {
	if !ValidHash(want) {
		return Ref{}, ErrBadHash
	}
	if size < 0 {
		return Ref{}, ErrSizeMismatch
	}
	return s.put(ctx, r, want, size)
}

func (s *Store) put(ctx context.Context, r io.Reader, want string, declared int64) (ref Ref, err error) {
	if declared > s.max {
		return Ref{}, fmt.Errorf("%w: %d bytes declared, the cap is %d", ErrTooLarge, declared, s.max)
	}
	if declared == 0 {
		return Ref{}, ErrEmpty
	}
	if err := ctx.Err(); err != nil {
		return Ref{}, err
	}
	need := declared
	if need < 1 {
		need = 1
	}
	if err := s.CheckSpace(need); err != nil {
		return Ref{}, err
	}
	// Read one byte more than allowed, so "exactly at the cap" and "over it" can be told apart
	// without ever consuming more than cap+1 bytes.
	limit := s.max + 1
	if declared > 0 {
		limit = declared + 1
	}
	tmp, err := os.CreateTemp(filepath.Join(s.dir, tmpDir), "put-")
	if err != nil {
		return Ref{}, fmt.Errorf("blobstore: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	if err = tmp.Chmod(0o600); err != nil {
		return Ref{}, err
	}
	hs := sha256.New()
	bufPtr := getBuf()
	defer putBuf(bufPtr)
	n, err := io.CopyBuffer(&teeWriter{hs, tmp}, &ctxReader{ctx, io.LimitReader(r, limit)}, *bufPtr)
	if err != nil {
		return Ref{}, wrapNoSpace(err)
	}
	switch {
	case n > s.max:
		return Ref{}, fmt.Errorf("%w: more than %d bytes arrived", ErrTooLarge, s.max)
	case n == 0:
		return Ref{}, ErrEmpty
	case declared > 0 && n != declared:
		return Ref{}, fmt.Errorf("%w: declared %d, received %d", ErrSizeMismatch, declared, n)
	}
	sum := hex.EncodeToString(hs.Sum(nil))
	if want != "" && sum != want {
		return Ref{}, fmt.Errorf("%w: asked for %s, received %s", ErrHashMismatch, want[:12], sum[:12])
	}
	ref = Ref{Hash: sum, Size: n}

	if s.Has(sum) && s.Verify(sum) == nil { // already held, and intact: keep it, drop the copy
		tmp.Close()
		os.Remove(tmpName)
		s.touch(sum)
		return ref, nil
	}
	if err = tmp.Sync(); err != nil {
		return Ref{}, wrapNoSpace(err)
	}
	if err = tmp.Close(); err != nil {
		return Ref{}, err
	}
	shard := filepath.Join(s.dir, sum[:2])
	if err = os.MkdirAll(shard, 0o700); err != nil {
		return Ref{}, err
	}
	if err = os.Rename(tmpName, s.path(sum)); err != nil { // atomic; replaces a damaged copy
		return Ref{}, err
	}
	syncDir(shard)
	syncDir(s.dir)
	return ref, nil
}

func syncDir(p string) {
	if d, err := os.Open(p); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

type teeWriter struct {
	h hash.Hash
	f *os.File
}

func (t *teeWriter) Write(p []byte) (int, error) {
	t.h.Write(p)
	return t.f.Write(p)
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
