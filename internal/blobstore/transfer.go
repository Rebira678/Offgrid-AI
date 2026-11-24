package blobstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// PartialTTL is how long an unfinished transfer is kept for a resume before it is swept.
const PartialTTL = 7 * 24 * time.Hour

// ErrBusy means another transfer of the same blob is running in this process.
var ErrBusy = errors.New("blobstore: this blob is already being received")

// Transfer receives one blob in pieces, possibly over several connections. The bytes accumulate in
// tmp/part-<hash>; a cut connection leaves them there, and the next Begin continues from their length.
// Nothing becomes visible under the final name until Commit has checked the whole file against the hash,
// so a wrong or damaged continuation is caught and the partial is thrown away.
type Transfer struct {
	s    *Store
	hash string
	size int64
	f    *os.File
	off  int64
	done bool
}

func (s *Store) partPath(h string) string { return filepath.Join(s.dir, tmpDir, "part-"+h) }

// PartialSize is how many bytes of this blob a cut transfer left behind (0 if none).
func (s *Store) PartialSize(h string) int64 {
	if !ValidHash(h) {
		return 0
	}
	if fi, err := os.Stat(s.partPath(h)); err == nil {
		return fi.Size()
	}
	return 0
}

// Begin starts or resumes receiving the blob with this hash and exactly this size (the size the op declared).
// It refuses an oversize blob, an empty one, and one that does not fit on the disk (only the bytes still
// missing are counted). Offset says where the sender must continue.
func (s *Store) Begin(hash string, size int64) (*Transfer, error) {
	if !ValidHash(hash) {
		return nil, ErrBadHash
	}
	if size == 0 {
		return nil, ErrEmpty
	}
	if size < 0 || size > s.max {
		return nil, fmt.Errorf("%w: %d bytes declared, the cap is %d", ErrTooLarge, size, s.max)
	}
	s.mu.Lock()
	if s.active[hash] {
		s.mu.Unlock()
		return nil, ErrBusy
	}
	s.active[hash] = true
	s.mu.Unlock()
	release := func() { s.mu.Lock(); delete(s.active, hash); s.mu.Unlock() }

	f, err := os.OpenFile(s.partPath(hash), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		release()
		return nil, fmt.Errorf("blobstore: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		release()
		return nil, err
	}
	off := fi.Size()
	if off > size { // not the blob we thought it was
		if err := f.Truncate(0); err != nil {
			f.Close()
			release()
			return nil, err
		}
		off = 0
	}
	if err := s.CheckSpace(size - off); err != nil {
		f.Close()
		if off == 0 {
			os.Remove(s.partPath(hash))
		}
		release()
		return nil, err
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		f.Close()
		release()
		return nil, err
	}
	return &Transfer{s: s, hash: hash, size: size, f: f, off: off}, nil
}

// Offset is the number of bytes already held; the sender continues from here.
func (t *Transfer) Offset() int64 { return t.off }

// Copy appends bytes from r until it ends. More bytes than the blob has left is ErrSizeMismatch (and the
// partial is dropped: the sender is not sending what it said). A read error keeps what arrived so far.
func (t *Transfer) Copy(ctx context.Context, r io.Reader) error {
	remaining := t.size - t.off
	bufPtr := getBuf()
	defer putBuf(bufPtr)
	n, err := io.CopyBuffer(t.f, &ctxReader{ctx, io.LimitReader(r, remaining+1)}, *bufPtr)
	t.off += n
	if err != nil {
		return wrapNoSpace(err)
	}
	if t.off > t.size {
		t.Abort()
		return fmt.Errorf("%w: more than the declared %d bytes arrived", ErrSizeMismatch, t.size)
	}
	return nil
}

// Commit checks the complete file against the hash and moves it into place. An incomplete file is
// ErrSizeMismatch (keep it, resume later); a file that hashes wrong is ErrHashMismatch and is deleted.
func (t *Transfer) Commit() (Ref, error) {
	if t.off != t.size {
		return Ref{}, fmt.Errorf("%w: %d of %d bytes", ErrSizeMismatch, t.off, t.size)
	}
	if err := t.f.Sync(); err != nil {
		return Ref{}, wrapNoSpace(err)
	}
	if _, err := t.f.Seek(0, io.SeekStart); err != nil {
		return Ref{}, err
	}
	hs := sha256.New()
	bufPtr := getBuf()
	defer putBuf(bufPtr)
	if _, err := io.CopyBuffer(hs, t.f, *bufPtr); err != nil {
		return Ref{}, err
	}
	if got := hex.EncodeToString(hs.Sum(nil)); got != t.hash {
		t.Abort()
		return Ref{}, fmt.Errorf("%w: asked for %s, received %s", ErrHashMismatch, t.hash[:12], got[:12])
	}
	s := t.s
	if err := os.MkdirAll(filepath.Join(s.dir, t.hash[:2]), 0o700); err != nil {
		return Ref{}, err
	}
	if err := os.Rename(s.partPath(t.hash), s.path(t.hash)); err != nil {
		return Ref{}, err
	}
	syncDir(filepath.Join(s.dir, t.hash[:2]))
	syncDir(s.dir)
	t.done = true
	t.Close()
	return Ref{Hash: t.hash, Size: t.size}, nil
}

// Abort throws the partial away (the bytes were wrong).
func (t *Transfer) Abort() {
	t.f.Close()
	os.Remove(t.s.partPath(t.hash))
	t.off = 0
	t.done = true
}

// Close releases the transfer, keeping any partial for a resume. Safe to call more than once.
func (t *Transfer) Close() {
	t.f.Close()
	t.s.mu.Lock()
	delete(t.s.active, t.hash)
	t.s.mu.Unlock()
}

// Walk calls fn for every stored blob (not partials) with its size and the time it was stored or last
// re-added. Order is not defined.
func (s *Store) Walk(fn func(hash string, size int64, mod time.Time) error) error {
	shards, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, sh := range shards {
		if !sh.IsDir() || len(sh.Name()) != 2 {
			continue
		}
		es, err := os.ReadDir(filepath.Join(s.dir, sh.Name()))
		if err != nil {
			return err
		}
		for _, e := range es {
			h := sh.Name() + e.Name()
			if !ValidHash(h) {
				continue
			}
			fi, err := e.Info()
			if err != nil || !fi.Mode().IsRegular() {
				continue
			}
			if err := fn(h, fi.Size(), fi.ModTime()); err != nil {
				return err
			}
		}
	}
	return nil
}

// Remove deletes a blob. A blob that is already gone is not an error.
func (s *Store) Remove(h string) error {
	if !ValidHash(h) {
		return ErrBadHash
	}
	if err := os.Remove(s.path(h)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// touch marks a blob as just stored, so a collector counts its age from now.
func (s *Store) touch(h string) {
	now := time.Now()
	_ = os.Chtimes(s.path(h), now, now)
}
