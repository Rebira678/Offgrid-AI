package blobstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
)

// TestBlobstoreValidHashMatrix verifies that ValidHash accepts only strictly valid
// 64-character lowercase hexadecimal strings and rejects all malformed variants.
func TestBlobstoreValidHashMatrix(t *testing.T) {
	valid := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if !ValidHash(valid) {
		t.Fatalf("expected ValidHash(%q) to be true", valid)
	}

	invalidCases := []struct {
		name string
		h    string
	}{
		{"empty", ""},
		{"short-63", valid[:63]},
		{"long-65", valid + "a"},
		{"uppercase", "0123456789ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef"},
		{"mixed-case", "0123456789aBcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		{"char-before-0", "/" + valid[1:]},
		{"char-after-9", ":" + valid[1:]},
		{"char-before-a", "`" + valid[1:]},
		{"char-after-f", "g" + valid[1:]},
		{"space-prefix", " " + valid[1:]},
		{"space-suffix", valid[:63] + " "},
		{"newline-suffix", valid[:63] + "\n"},
		{"null-byte", "\x00" + valid[1:]},
		{"path-traversal-dotdot", "../" + valid[3:]},
		{"path-traversal-slash", "ab/" + valid[3:]},
		{"path-traversal-backslash", "ab\\" + valid[3:]},
	}

	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			if ValidHash(tc.h) {
				t.Fatalf("ValidHash(%q) should be false", tc.h)
			}
		})
	}
}

// TestBlobstoreConcurrentPutSameBlob tests that multiple goroutines calling Put
// concurrently on the exact same payload race cleanly without corrupting the store,
// leaving no temporary files and resulting in a single verified blob.
func TestBlobstoreConcurrentPutSameBlob(t *testing.T) {
	s := newStore(t, 1<<20)
	payload := bytes.Repeat([]byte("identical-content-burst-12345"), 1000)
	wantHash := hashOf(payload)

	const goroutines = 20
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	refs := make([]Ref, goroutines)

	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			ref, err := s.Put(context.Background(), bytes.NewReader(payload), int64(len(payload)))
			errs[idx] = err
			refs[idx] = ref
		}(i)
	}

	close(start)
	wg.Wait()

	for i := 0; i < goroutines; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d failed: %v", i, errs[i])
		}
		if refs[i].Hash != wantHash || refs[i].Size != int64(len(payload)) {
			t.Fatalf("goroutine %d got ref %+v, want hash %s", i, refs[i], wantHash)
		}
	}

	if err := s.Verify(wantHash); err != nil {
		t.Fatalf("Verify failed after concurrent put: %v", err)
	}

	if n := countBlobs(t, s); n != 1 {
		t.Fatalf("expected 1 blob, found %d", n)
	}
	if n := countTemps(t, s); n != 0 {
		t.Fatalf("expected 0 temp files, found %d", n)
	}
}

// TestBlobstoreConcurrentPutDistinctBlobs verifies concurrent writes of distinct blobs.
func TestBlobstoreConcurrentPutDistinctBlobs(t *testing.T) {
	s := newStore(t, 1<<20)
	const count = 30
	var wg sync.WaitGroup
	hashes := make([]string, count)
	errs := make([]error, count)

	start := make(chan struct{})
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			data := []byte(fmt.Sprintf("unique-blob-content-%04d", idx))
			ref, err := s.Put(context.Background(), bytes.NewReader(data), int64(len(data)))
			errs[idx] = err
			if err == nil {
				hashes[idx] = ref.Hash
			}
		}(i)
	}

	close(start)
	wg.Wait()

	for i := 0; i < count; i++ {
		if errs[i] != nil {
			t.Fatalf("put %d failed: %v", i, errs[i])
		}
		if !s.Has(hashes[i]) {
			t.Fatalf("blob %d (%s) missing", i, hashes[i])
		}
		if err := s.Verify(hashes[i]); err != nil {
			t.Fatalf("blob %d verification failed: %v", i, err)
		}
	}

	if n := countBlobs(t, s); n != count {
		t.Fatalf("expected %d blobs, got %d", count, n)
	}
	if n := countTemps(t, s); n != 0 {
		t.Fatalf("expected 0 temp files, got %d", n)
	}
}

// TestBlobstorePutExpectedCorruptRepair tests that when an on-disk blob has suffered bit-rot
// or damage, calling PutExpected with the correct bytes detects the corrupted state and repairs
// the file so that subsequent Verify calls pass.
func TestBlobstorePutExpectedCorruptRepair(t *testing.T) {
	s := newStore(t, 1<<20)
	goodData := []byte("pristine original asset bytes")
	h := hashOf(goodData)

	ref, err := s.PutExpected(context.Background(), bytes.NewReader(goodData), h, int64(len(goodData)))
	if err != nil {
		t.Fatalf("PutExpected: %v", err)
	}
	if ref.Hash != h {
		t.Fatalf("hash mismatch: got %s, want %s", ref.Hash, h)
	}

	// Corrupt on-disk file
	blobPath := s.path(h)
	if err := os.WriteFile(blobPath, []byte("tampered altered asset bytes!"), 0o600); err != nil {
		t.Fatalf("WriteFile corrupt: %v", err)
	}

	if err := s.Verify(h); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("expected ErrHashMismatch on corrupted blob, got %v", err)
	}

	// Repair via PutExpected
	repairedRef, err := s.PutExpected(context.Background(), bytes.NewReader(goodData), h, int64(len(goodData)))
	if err != nil {
		t.Fatalf("PutExpected repair: %v", err)
	}
	if repairedRef.Hash != h {
		t.Fatalf("repaired hash mismatch: %s", repairedRef.Hash)
	}

	if err := s.Verify(h); err != nil {
		t.Fatalf("Verify after repair failed: %v", err)
	}

	// Verify content on disk matches goodData
	f, err := s.Open(h)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	content, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(content, goodData) {
		t.Fatalf("content not restored: got %q, want %q", content, goodData)
	}
}

// TestBlobstoreFaultyReaderErrors verifies that errors encountered while streaming
// the body cleanly clean up any temporary files and do not record partial blobs.
func TestBlobstoreFaultyReaderErrors(t *testing.T) {
	s := newStore(t, 1<<20)

	// 1. Error on first byte
	ioErr := errors.New("simulated network read failure")
	_, err := s.Put(context.Background(), iotest.ErrReader(ioErr), -1)
	if !errors.Is(err, ioErr) {
		t.Fatalf("expected %v, got %v", ioErr, err)
	}
	if countTemps(t, s) != 0 || countBlobs(t, s) != 0 {
		t.Fatal("temp files or blobs remained after read failure")
	}

	// 2. Error after partial read
	partial := io.MultiReader(
		bytes.NewReader(make([]byte, 1024)),
		iotest.ErrReader(ioErr),
	)
	_, err = s.Put(context.Background(), partial, -1)
	if !errors.Is(err, ioErr) {
		t.Fatalf("expected %v, got %v", ioErr, err)
	}
	if countTemps(t, s) != 0 || countBlobs(t, s) != 0 {
		t.Fatal("temp files or blobs remained after partial read failure")
	}
}

// TestBlobstoreZeroByteVariants verifies zero-byte blob handling across APIs.
func TestBlobstoreZeroByteVariants(t *testing.T) {
	s := newStore(t, 1<<20)

	// Declared 0
	_, err := s.Put(context.Background(), bytes.NewReader(nil), 0)
	if !errors.Is(err, ErrEmpty) {
		t.Fatalf("expected ErrEmpty, got %v", err)
	}

	// Declared -1, but reader has 0 bytes
	_, err = s.Put(context.Background(), bytes.NewReader(nil), -1)
	if !errors.Is(err, ErrEmpty) {
		t.Fatalf("expected ErrEmpty, got %v", err)
	}

	// PutExpected with size 0
	emptyHash := hashOf(nil)
	_, err = s.PutExpected(context.Background(), bytes.NewReader(nil), emptyHash, 0)
	if !errors.Is(err, ErrEmpty) {
		t.Fatalf("expected ErrEmpty for PutExpected 0-byte, got %v", err)
	}

	// PutExpected with negative size
	_, err = s.PutExpected(context.Background(), bytes.NewReader(nil), emptyHash, -1)
	if !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("expected ErrSizeMismatch, got %v", err)
	}
}

// TestBlobstoreContextCancellationEdge verifies context cancellation before and during Put.
func TestBlobstoreContextCancellationEdge(t *testing.T) {
	s := newStore(t, 1<<20)

	// 1. Pre-cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.Put(ctx, strings.NewReader("hello"), 5)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if countTemps(t, s) != 0 || countBlobs(t, s) != 0 {
		t.Fatal("pre-cancelled Put left files behind")
	}

	// 2. Cancellation during read
	ctx2, cancel2 := context.WithCancel(context.Background())
	readerWithCancel := &callbackReader{
		r: bytes.NewReader(make([]byte, 64*1024)),
		onRead: func(n int) {
			if n > 0 {
				cancel2()
			}
		},
	}
	_, err = s.Put(ctx2, readerWithCancel, -1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled during stream, got %v", err)
	}
	if countTemps(t, s) != 0 || countBlobs(t, s) != 0 {
		t.Fatal("cancelled stream left files behind")
	}
}

// TestBlobstoreSweepTempFilesMultiple verifies New removes any arbitrary leftovers in tmp/.
func TestBlobstoreSweepTempFilesMultiple(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "blobs")
	s, err := New(dir, 1024)
	if err != nil {
		t.Fatal(err)
	}

	// Plant multiple stale files in tmp/
	tmpPath := filepath.Join(s.Dir(), tmpDir)
	for i := 0; i < 5; i++ {
		fPath := filepath.Join(tmpPath, fmt.Sprintf("stale-crash-file-%d", i))
		if err := os.WriteFile(fPath, []byte("abandoned"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if countTemps(t, s) != 5 {
		t.Fatalf("expected 5 temp files planted, got %d", countTemps(t, s))
	}

	// Reopen store
	s2, err := New(dir, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if countTemps(t, s2) != 0 {
		t.Fatalf("expected 0 temp files after reopen, got %d", countTemps(t, s2))
	}
}

// TestBlobstoreSizeDiscrepancyCases verifies all variants of declared vs actual byte count.
func TestBlobstoreSizeDiscrepancyCases(t *testing.T) {
	s := newStore(t, 1000)

	// Declared 50, sent 49
	_, err := s.Put(context.Background(), bytes.NewReader(make([]byte, 49)), 50)
	if !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("expected ErrSizeMismatch for short body, got %v", err)
	}

	// Declared 50, sent 51
	_, err = s.Put(context.Background(), bytes.NewReader(make([]byte, 51)), 50)
	if !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("expected ErrSizeMismatch for long body, got %v", err)
	}

	// Exactly at the cap (1000 bytes)
	ref, err := s.Put(context.Background(), bytes.NewReader(make([]byte, 1000)), 1000)
	if err != nil || ref.Size != 1000 {
		t.Fatalf("exact cap failed: %v, ref=%+v", err, ref)
	}

	// Over the cap by 1 byte (1001 bytes, undeclared)
	_, err = s.Put(context.Background(), bytes.NewReader(make([]byte, 1001)), -1)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("expected ErrTooLarge for cap+1, got %v", err)
	}
}

// TestBlobstoreConcurrentOpenAndRead ensures simultaneous readers do not block or corrupt.
func TestBlobstoreConcurrentOpenAndRead(t *testing.T) {
	s := newStore(t, 1<<20)
	data := []byte("content intended for massive parallel reading")
	ref, err := s.Put(context.Background(), bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}

	const readers = 25
	var wg sync.WaitGroup
	var readErrors atomic.Int32

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f, err := s.Open(ref.Hash)
			if err != nil {
				readErrors.Add(1)
				return
			}
			defer f.Close()
			buf, err := io.ReadAll(f)
			if err != nil || !bytes.Equal(buf, data) {
				readErrors.Add(1)
			}
		}()
	}

	wg.Wait()
	if n := readErrors.Load(); n > 0 {
		t.Fatalf("%d parallel reads failed", n)
	}
}

// TestBlobstoreDefaultMaxBytesConfig verifies default size cap when max <= 0.
func TestBlobstoreDefaultMaxBytesConfig(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "blobs"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.MaxBytes() != DefaultMaxBytes {
		t.Fatalf("expected DefaultMaxBytes (%d), got %d", DefaultMaxBytes, s.MaxBytes())
	}

	sNeg, err := New(filepath.Join(t.TempDir(), "blobs-neg"), -500)
	if err != nil {
		t.Fatal(err)
	}
	if sNeg.MaxBytes() != DefaultMaxBytes {
		t.Fatalf("expected DefaultMaxBytes for negative max, got %d", sNeg.MaxBytes())
	}
}

type callbackReader struct {
	r      io.Reader
	onRead func(n int)
}

func (cr *callbackReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	if cr.onRead != nil {
		cr.onRead(n)
	}
	return n, err
}
