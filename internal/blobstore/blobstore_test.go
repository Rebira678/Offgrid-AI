package blobstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/iotest"
)

func hashOf(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func newStore(t *testing.T, max int64) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "blobs"), max)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPutNamesTheFileByItsHashAndGetReturnsTheBytes(t *testing.T) {
	s := newStore(t, 1<<20)
	data := []byte("a photo of a leaking pump")
	ref, err := s.Put(context.Background(), bytes.NewReader(data), -1)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Hash != hashOf(data) || ref.Size != int64(len(data)) {
		t.Fatalf("ref = %+v", ref)
	}
	f, err := s.Open(ref.Hash)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, _ := io.ReadAll(f)
	if !bytes.Equal(got, data) {
		t.Fatal("bytes differ")
	}
	if !s.Has(ref.Hash) || s.Has(hashOf([]byte("other"))) {
		t.Fatal("Has is wrong")
	}
	// on disk: dir/ab/<rest>
	if _, err := os.Stat(filepath.Join(s.Dir(), ref.Hash[:2], ref.Hash[2:])); err != nil {
		t.Fatal(err)
	}
}

func TestPutTheSameContentTwiceStoresItOnce(t *testing.T) {
	s := newStore(t, 1<<20)
	a, _ := s.Put(context.Background(), strings.NewReader("same"), -1)
	b, err := s.Put(context.Background(), strings.NewReader("same"), -1)
	if err != nil || a != b {
		t.Fatalf("%v %v %v", a, b, err)
	}
	if n := countBlobs(t, s); n != 1 {
		t.Fatalf("%d blobs", n)
	}
	if n := countTemps(t, s); n != 0 {
		t.Fatalf("%d temp files left", n)
	}
}

func TestOverTheCapIsRejectedBeforeAnyBodyIsRead(t *testing.T) {
	s := newStore(t, 100)
	r := &countingReader{r: bytes.NewReader(make([]byte, 1000))}
	_, err := s.Put(context.Background(), r, 1000) // the sender says 1000
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v", err)
	}
	if r.n != 0 {
		t.Fatalf("read %d bytes of a body that was over the cap", r.n)
	}
	if countBlobs(t, s)+countTemps(t, s) != 0 {
		t.Fatal("something was written")
	}
}

func TestALyingSizeCannotSmuggleMoreThanTheCap(t *testing.T) {
	s := newStore(t, 100)
	r := &countingReader{r: bytes.NewReader(make([]byte, 5000))}
	_, err := s.Put(context.Background(), r, 10) // claims 10, sends 5000
	if !errors.Is(err, ErrTooLarge) && !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("got %v", err)
	}
	if r.n > 101+32*1024 { // at most the cap plus one read buffer
		t.Fatalf("read %d bytes", r.n)
	}
	if countBlobs(t, s)+countTemps(t, s) != 0 {
		t.Fatal("something was left behind")
	}
}

func TestUnknownLengthOverTheCapStopsAtTheCap(t *testing.T) {
	s := newStore(t, 100)
	r := &countingReader{r: iotest.OneByteReader(bytes.NewReader(make([]byte, 5000)))}
	if _, err := s.Put(context.Background(), r, -1); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v", err)
	}
	if r.n > 101 {
		t.Fatalf("read %d bytes past a 100-byte cap", r.n)
	}
}

func TestEmptyIsRefused(t *testing.T) {
	s := newStore(t, 100)
	if _, err := s.Put(context.Background(), strings.NewReader(""), -1); !errors.Is(err, ErrEmpty) {
		t.Fatalf("got %v", err)
	}
}

func TestPutExpectedAcceptsTheRightHashAndRefusesTheWrongOne(t *testing.T) {
	s := newStore(t, 1<<20)
	data := []byte("the bytes a peer sent")
	if _, err := s.PutExpected(context.Background(), bytes.NewReader(data), hashOf(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	evil := []byte("different bytes, same claimed hash")
	other := newStore(t, 1<<20)
	_, err := other.PutExpected(context.Background(), bytes.NewReader(evil), hashOf(data), int64(len(evil)))
	if !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("got %v", err)
	}
	if other.Has(hashOf(data)) || other.Has(hashOf(evil)) || countBlobs(t, other)+countTemps(t, other) != 0 {
		t.Fatal("a corrupted blob was kept")
	}
	_, err = other.PutExpected(context.Background(), bytes.NewReader(data[:5]), hashOf(data), int64(len(data)))
	if !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("a short body: got %v", err)
	}
}

func TestAReadErrorLeavesNothingBehind(t *testing.T) {
	s := newStore(t, 1<<20)
	boom := errors.New("connection cut")
	r := io.MultiReader(strings.NewReader("half of it"), iotest.ErrReader(boom))
	if _, err := s.Put(context.Background(), r, -1); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	if countBlobs(t, s)+countTemps(t, s) != 0 {
		t.Fatal("partial file left")
	}
}

func TestACancelledContextStopsTheCopy(t *testing.T) {
	s := newStore(t, 1<<30)
	ctx, cancel := context.WithCancel(context.Background())
	r := &cancelAfter{r: zeroReader{}, limit: 1 << 20, cancel: cancel}
	_, err := s.Put(ctx, r, -1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if countBlobs(t, s)+countTemps(t, s) != 0 {
		t.Fatal("partial file left")
	}
}

func TestBadHashesCannotEscapeTheDirectory(t *testing.T) {
	s := newStore(t, 100)
	for _, h := range []string{"", "../../etc/passwd", strings.Repeat("A", 64), strings.Repeat("g", 64), strings.Repeat("a", 63), strings.Repeat("a", 65), "ab/" + strings.Repeat("a", 61), "..", "."} {
		if s.Has(h) {
			t.Errorf("Has(%q)", h)
		}
		if _, err := s.Open(h); !errors.Is(err, ErrBadHash) {
			t.Errorf("Open(%q): %v", h, err)
		}
		if _, err := s.PutExpected(context.Background(), strings.NewReader("x"), h, 1); !errors.Is(err, ErrBadHash) {
			t.Errorf("PutExpected(%q): %v", h, err)
		}
	}
}

func TestOpenOfAMissingBlobIsNotFound(t *testing.T) {
	s := newStore(t, 100)
	if _, err := s.Open(hashOf([]byte("nope"))); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestVerifyDetectsADamagedBlob(t *testing.T) {
	s := newStore(t, 1<<20)
	ref, _ := s.Put(context.Background(), strings.NewReader("good bytes"), -1)
	if err := s.Verify(ref.Hash); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(s.Dir(), ref.Hash[:2], ref.Hash[2:])
	if err := os.WriteFile(p, []byte("bad  bytes"), 0o600); err != nil { // bit rot: same length
		t.Fatal(err)
	}
	if err := s.Verify(ref.Hash); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("got %v", err)
	}
	if err := s.Verify(hashOf([]byte("absent"))); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestAPutAfterDamageRepairsTheBlob(t *testing.T) {
	s := newStore(t, 1<<20)
	ref, _ := s.Put(context.Background(), strings.NewReader("good bytes"), -1)
	p := filepath.Join(s.Dir(), ref.Hash[:2], ref.Hash[2:])
	os.WriteFile(p, []byte("rotten"), 0o600)
	// A peer sends it again: the stored copy fails its hash, so it is replaced rather than trusted.
	if _, err := s.PutExpected(context.Background(), strings.NewReader("good bytes"), ref.Hash, ref.Size); err != nil {
		t.Fatal(err)
	}
	if err := s.Verify(ref.Hash); err != nil {
		t.Fatalf("not repaired: %v", err)
	}
}

func TestFilesAndDirectoriesArePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix modes")
	}
	s := newStore(t, 100)
	ref, _ := s.Put(context.Background(), strings.NewReader("x"), -1)
	for _, p := range []string{s.Dir(), filepath.Join(s.Dir(), ref.Hash[:2])} {
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o700 {
			t.Errorf("%s is %v", p, fi.Mode().Perm())
		}
	}
	if fi, _ := os.Stat(filepath.Join(s.Dir(), ref.Hash[:2], ref.Hash[2:])); fi.Mode().Perm() != 0o600 {
		t.Errorf("blob is %v", fi.Mode().Perm())
	}
}

func TestStaleTempFilesAreSweptOnOpen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "blobs")
	s, _ := New(dir, 100)
	stale := filepath.Join(s.Dir(), tmpDir, "put-crashed")
	os.WriteFile(stale, []byte("half"), 0o600)
	if _, err := New(dir, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("a leftover temp file from a crash was kept")
	}
}

// Memory stays flat: a 64 MiB blob goes through with far less than 64 MiB of heap growth.
func TestALargeBlobIsNeverHeldInMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("moves 64 MiB")
	}
	s := newStore(t, 128<<20)
	const size = 64 << 20
	var before, peak runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	r := &sampling{r: io.LimitReader(zeroReader{}, size), every: 4 << 20, peak: &peak}
	ref, err := s.Put(context.Background(), r, -1)
	if err != nil || ref.Size != size {
		t.Fatalf("%v %+v", err, ref)
	}
	growth := int64(peak.HeapAlloc) - int64(before.HeapAlloc)
	if growth > 8<<20 {
		t.Fatalf("heap grew %d MiB while a %d MiB blob was written", growth>>20, size>>20)
	}
	f, _ := s.Open(ref.Hash)
	defer f.Close()
	if n, _ := io.Copy(io.Discard, f); n != size {
		t.Fatalf("read back %d", n)
	}
}

// ---- helpers

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) { n, err := c.r.Read(p); c.n += n; return n, err }

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

type cancelAfter struct {
	r      io.Reader
	limit  int
	n      int
	cancel context.CancelFunc
}

func (c *cancelAfter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	if c.n >= c.limit {
		c.cancel()
	}
	return n, err
}

type sampling struct {
	r     io.Reader
	n     int
	every int
	peak  *runtime.MemStats
}

func (s *sampling) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	s.n += n
	if s.n >= s.every {
		s.n = 0
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		if m.HeapAlloc > s.peak.HeapAlloc {
			*s.peak = m
		}
	}
	return n, err
}

func countBlobs(t *testing.T, s *Store) int {
	t.Helper()
	n := 0
	filepath.WalkDir(s.Dir(), func(p string, d os.DirEntry, err error) error {
		rel, _ := filepath.Rel(s.Dir(), p)
		if err == nil && !d.IsDir() && !strings.HasPrefix(rel, tmpDir+string(filepath.Separator)) {
			n++
		}
		return nil
	})
	return n
}

func countTemps(t *testing.T, s *Store) int {
	t.Helper()
	es, _ := os.ReadDir(filepath.Join(s.Dir(), tmpDir))
	return len(es)
}
