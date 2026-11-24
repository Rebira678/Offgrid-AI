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
	"syscall"
	"testing"
	"testing/iotest"
	"time"
)

func payload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i>>8)
	}
	return b
}

func TestATransferCutHalfwayResumesFromWhereItStopped(t *testing.T) {
	s := newStore(t, 1<<20)
	data := payload(300_000)
	h := hashOf(data)
	ctx := context.Background()

	tr, err := s.Begin(h, int64(len(data)))
	if err != nil || tr.Offset() != 0 {
		t.Fatalf("%v %d", err, tr.Offset())
	}
	cut := io.MultiReader(bytes.NewReader(data[:123_456]), iotest.ErrReader(errors.New("connection reset")))
	if err := tr.Copy(ctx, cut); err == nil {
		t.Fatal("the cut was not reported")
	}
	tr.Close()
	if s.Has(h) {
		t.Fatal("half a blob is visible under its final name")
	}
	if got := s.PartialSize(h); got != 123_456 {
		t.Fatalf("partial = %d, want 123456", got)
	}

	tr, err = s.Begin(h, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if tr.Offset() != 123_456 {
		t.Fatalf("resume offset = %d", tr.Offset())
	}
	if err := tr.Copy(ctx, bytes.NewReader(data[tr.Offset():])); err != nil { // the peer sends only the rest
		t.Fatal(err)
	}
	ref, err := tr.Commit()
	if err != nil || ref.Hash != h || ref.Size != int64(len(data)) {
		t.Fatalf("%v %+v", err, ref)
	}
	if err := s.Verify(h); err != nil {
		t.Fatal(err)
	}
	if s.PartialSize(h) != 0 || countTemps(t, s) != 0 {
		t.Fatal("partial file left after the commit")
	}
}

func TestResumedBytesThatAreWrongAreCaughtAndDiscarded(t *testing.T) {
	s := newStore(t, 1<<20)
	data := payload(10_000)
	h := hashOf(data)
	ctx := context.Background()
	tr, _ := s.Begin(h, int64(len(data)))
	tr.Copy(ctx, io.MultiReader(bytes.NewReader(data[:4000]), iotest.ErrReader(io.ErrUnexpectedEOF)))
	tr.Close()
	// the peer continues with a different file's bytes
	tr, _ = s.Begin(h, int64(len(data)))
	evil := payload(10_000)
	evil[9000] ^= 0xff
	if err := tr.Copy(ctx, bytes.NewReader(evil[tr.Offset():])); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Commit(); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("got %v", err)
	}
	tr.Close()
	if s.Has(h) || s.PartialSize(h) != 0 {
		t.Fatal("corrupt data kept: the next attempt must start from zero")
	}
}

func TestAnIncompleteTransferCannotBeCommitted(t *testing.T) {
	s := newStore(t, 1<<20)
	data := payload(5000)
	tr, _ := s.Begin(hashOf(data), 5000)
	tr.Copy(context.Background(), bytes.NewReader(data[:100]))
	if _, err := tr.Commit(); !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("got %v", err)
	}
	tr.Close()
	if s.Has(hashOf(data)) {
		t.Fatal("committed an incomplete blob")
	}
}

func TestMoreBytesThanDeclaredAreRefused(t *testing.T) {
	s := newStore(t, 1<<20)
	data := payload(5000)
	tr, _ := s.Begin(hashOf(data), 4000)
	err := tr.Copy(context.Background(), bytes.NewReader(data))
	if !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("got %v", err)
	}
	tr.Close()
	if s.PartialSize(hashOf(data)) != 0 {
		t.Fatal("an overlong body left a partial")
	}
}

func TestOverTheCapOrABadHashCannotBegin(t *testing.T) {
	s := newStore(t, 1000)
	if _, err := s.Begin(hashOf([]byte("x")), 5000); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v", err)
	}
	if _, err := s.Begin("../x", 10); !errors.Is(err, ErrBadHash) {
		t.Fatalf("got %v", err)
	}
	if _, err := s.Begin(hashOf([]byte("x")), 0); !errors.Is(err, ErrEmpty) {
		t.Fatalf("got %v", err)
	}
}

func TestTwoTransfersOfTheSameBlobCannotRunAtOnce(t *testing.T) {
	s := newStore(t, 1<<20)
	h := hashOf([]byte("x"))
	a, err := s.Begin(h, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Begin(h, 10); !errors.Is(err, ErrBusy) {
		t.Fatalf("got %v", err)
	}
	a.Close()
	b, err := s.Begin(h, 10)
	if err != nil {
		t.Fatalf("after Close: %v", err)
	}
	b.Close()
}

func TestAFullDiskRefusesAndSaysWhy(t *testing.T) {
	s := newStore(t, 10<<20)
	s.SetReserve(1 << 20)
	var avail uint64 = 3 << 20
	s.SetFreeFunc(func(string) (uint64, error) { return avail, nil })
	ctx := context.Background()

	// 2 MiB fits in 3 MiB free with a 1 MiB reserve only just: refused (needs 2 + 1 = 3, strictly more than left after)
	data := payload(2<<20 + 1)
	_, err := s.PutExpected(ctx, bytes.NewReader(data), hashOf(data), int64(len(data)))
	if !errors.Is(err, ErrNoSpace) {
		t.Fatalf("got %v", err)
	}
	var ns *NoSpaceError
	if !errors.As(err, &ns) || ns.Need != int64(len(data)) || ns.Free != 3<<20 || ns.Reserve != 1<<20 {
		t.Fatalf("the error does not say why: %v", err)
	}
	if countBlobs(t, s)+countTemps(t, s) != 0 {
		t.Fatal("something was written to a full disk")
	}
	if _, err := s.Begin(hashOf(data), int64(len(data))); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("Begin: %v", err)
	}
	// room appears
	avail = 100 << 20
	if _, err := s.PutExpected(ctx, bytes.NewReader(data), hashOf(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
}

func TestAResumeOnlyNeedsRoomForTheRest(t *testing.T) {
	s := newStore(t, 10<<20)
	s.SetReserve(0)
	data := payload(1 << 20)
	h := hashOf(data)
	avail := uint64(2 << 20)
	s.SetFreeFunc(func(string) (uint64, error) { return avail, nil })
	tr, err := s.Begin(h, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	tr.Copy(context.Background(), io.MultiReader(bytes.NewReader(data[:900_000]), iotest.ErrReader(io.ErrUnexpectedEOF)))
	tr.Close()
	avail = 200_000 // less than the whole blob, more than what is left to fetch
	tr, err = s.Begin(h, int64(len(data)))
	if err != nil {
		t.Fatalf("a resume was refused for room it already has: %v", err)
	}
	tr.Close()
}

func TestENOSPCFromTheFilesystemIsReportedAsNoSpace(t *testing.T) {
	err := wrapNoSpace(&os.PathError{Op: "write", Path: "x", Err: syscall.ENOSPC})
	if !errors.Is(err, ErrNoSpace) {
		t.Fatalf("got %v", err)
	}
	if other := errors.New("boom"); wrapNoSpace(other) != other {
		t.Fatal("other errors must pass through")
	}
}

func TestPutOfAnExistingBlobRefreshesItsAge(t *testing.T) {
	s := newStore(t, 1<<20)
	ref, _ := s.Put(context.Background(), bytes.NewReader([]byte("again")), -1)
	p := filepath.Join(s.Dir(), ref.Hash[:2], ref.Hash[2:])
	old := time.Now().Add(-30 * 24 * time.Hour)
	os.Chtimes(p, old, old)
	s.Put(context.Background(), bytes.NewReader([]byte("again")), -1)
	fi, _ := os.Stat(p)
	if time.Since(fi.ModTime()) > time.Minute {
		t.Fatal("re-adding a blob left its age alone: a collector could remove it before the op is written")
	}
}

func TestWalkAndRemove(t *testing.T) {
	s := newStore(t, 1<<20)
	var refs []Ref
	for _, w := range []string{"a", "bb", "ccc"} {
		r, _ := s.Put(context.Background(), bytes.NewReader([]byte(w)), -1)
		refs = append(refs, r)
	}
	seen := map[string]int64{}
	if err := s.Walk(func(h string, size int64, mod time.Time) error { seen[h] = size; return nil }); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 || seen[refs[2].Hash] != 3 {
		t.Fatalf("%v", seen)
	}
	if err := s.Remove(refs[0].Hash); err != nil || s.Has(refs[0].Hash) {
		t.Fatalf("%v", err)
	}
	if err := s.Remove(refs[0].Hash); err != nil { // already gone is not an error
		t.Fatal(err)
	}
	if err := s.Remove("../../x"); !errors.Is(err, ErrBadHash) {
		t.Fatalf("got %v", err)
	}
}

func TestOldPartialsAreSweptButRecentOnesSurviveARestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "b")
	s, _ := New(dir, 1000)
	h1, h2 := hashOf([]byte("1")), hashOf([]byte("2"))
	for _, h := range []string{h1, h2} {
		tr, _ := s.Begin(h, 100)
		tr.Copy(context.Background(), io.MultiReader(bytes.NewReader([]byte("part")), iotest.ErrReader(io.ErrUnexpectedEOF)))
		tr.Close()
	}
	old := time.Now().Add(-2 * PartialTTL)
	os.Chtimes(filepath.Join(dir, tmpDir, "part-"+h1), old, old)
	s2, _ := New(dir, 1000)
	if s2.PartialSize(h1) != 0 {
		t.Fatal("an abandoned partial was kept forever")
	}
	if s2.PartialSize(h2) != 4 {
		t.Fatal("a recent partial did not survive the restart: resume would start over")
	}
}

func TestALargeTransferStaysFlatInMemoryAcrossAResume(t *testing.T) {
	if testing.Short() {
		t.Skip("moves 64 MiB")
	}
	s := newStore(t, 128<<20)
	const size = 64 << 20
	h, _ := hashStream(size)
	var before, peak runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	ctx := context.Background()
	tr, _ := s.Begin(h, size)
	r1 := &sampling{r: io.MultiReader(io.LimitReader(newPattern(0), 20<<20), iotest.ErrReader(io.ErrUnexpectedEOF)), every: 4 << 20, peak: &peak}
	tr.Copy(ctx, r1)
	tr.Close()
	tr, _ = s.Begin(h, size)
	off := tr.Offset()
	r2 := &sampling{r: io.LimitReader(newPattern(off), size-off), every: 4 << 20, peak: &peak}
	if err := tr.Copy(ctx, r2); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Commit(); err != nil {
		t.Fatal(err)
	}
	if g := int64(peak.HeapAlloc) - int64(before.HeapAlloc); g > 8<<20 {
		t.Fatalf("heap grew %d MiB moving a 64 MiB blob", g>>20)
	}
}

// pattern yields a deterministic stream starting at offset pos.
type pattern struct{ pos int64 }

func newPattern(off int64) *pattern { return &pattern{off} }

func (p *pattern) Read(b []byte) (int, error) {
	for i := range b {
		b[i] = byte((p.pos + int64(i)) * 31 >> 3)
	}
	p.pos += int64(len(b))
	return len(b), nil
}

func hashStream(n int64) (string, error) {
	h := sha256.New()
	_, err := io.Copy(h, io.LimitReader(newPattern(0), n))
	return hex.EncodeToString(h.Sum(nil)), err
}
