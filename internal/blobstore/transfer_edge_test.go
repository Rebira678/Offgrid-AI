package blobstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"sync"
	"syscall"
	"testing"
	"testing/iotest"
)

func TestTransferMultiStepResume(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 2<<20)
	const totalSize = 250_000
	data := payload(totalSize)
	h := hashOf(data)

	// Step 1: Write first 50,000 bytes and fail
	tr, err := s.Begin(h, int64(len(data)))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if tr.Offset() != 0 {
		t.Fatalf("initial offset = %d, want 0", tr.Offset())
	}
	r1 := io.MultiReader(bytes.NewReader(data[:50_000]), iotest.ErrReader(errors.New("cut 1")))
	_ = tr.Copy(ctx, r1)
	tr.Close()

	if got := s.PartialSize(h); got != 50_000 {
		t.Fatalf("partial after cut 1 = %d, want 50000", got)
	}

	// Step 2: Resume, write next 75,000 bytes (to 125,000) and fail
	tr, err = s.Begin(h, int64(len(data)))
	if err != nil {
		t.Fatalf("Begin resume 1: %v", err)
	}
	if tr.Offset() != 50_000 {
		t.Fatalf("resume 1 offset = %d, want 50000", tr.Offset())
	}
	r2 := io.MultiReader(bytes.NewReader(data[50_000:125_000]), iotest.ErrReader(errors.New("cut 2")))
	_ = tr.Copy(ctx, r2)
	tr.Close()

	if got := s.PartialSize(h); got != 125_000 {
		t.Fatalf("partial after cut 2 = %d, want 125000", got)
	}

	// Step 3: Resume, write next 100,000 bytes (to 225,000) and fail
	tr, err = s.Begin(h, int64(len(data)))
	if err != nil {
		t.Fatalf("Begin resume 2: %v", err)
	}
	if tr.Offset() != 125_000 {
		t.Fatalf("resume 2 offset = %d, want 125000", tr.Offset())
	}
	r3 := io.MultiReader(bytes.NewReader(data[125_000:225_000]), iotest.ErrReader(errors.New("cut 3")))
	_ = tr.Copy(ctx, r3)
	tr.Close()

	if got := s.PartialSize(h); got != 225_000 {
		t.Fatalf("partial after cut 3 = %d, want 225000", got)
	}

	// Step 4: Resume and complete the final 25,000 bytes
	tr, err = s.Begin(h, int64(len(data)))
	if err != nil {
		t.Fatalf("Begin final: %v", err)
	}
	if tr.Offset() != 225_000 {
		t.Fatalf("final resume offset = %d, want 225000", tr.Offset())
	}
	if err := tr.Copy(ctx, bytes.NewReader(data[225_000:])); err != nil {
		t.Fatalf("final Copy: %v", err)
	}
	ref, err := tr.Commit()
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if ref.Hash != h || ref.Size != totalSize {
		t.Fatalf("committed ref mismatch: %+v", ref)
	}

	// Verify intact in store
	if err := s.Verify(h); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if s.PartialSize(h) != 0 {
		t.Fatalf("partial file was not cleaned up after commit")
	}
}

func TestTransferAbortLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 1<<20)
	data := payload(10_000)
	h := hashOf(data)

	tr, err := s.Begin(h, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	_ = tr.Copy(ctx, bytes.NewReader(data[:5000]))
	if s.PartialSize(h) != 5000 {
		t.Fatalf("partial = %d, want 5000", s.PartialSize(h))
	}

	// Calling Abort cleans up the partial file
	tr.Abort()
	tr.Close()

	if s.PartialSize(h) != 0 {
		t.Fatalf("partial after abort = %d, want 0", s.PartialSize(h))
	}

	// And we can immediately begin anew without ErrBusy
	tr2, err := s.Begin(h, int64(len(data)))
	if err != nil {
		t.Fatalf("Begin after Abort: %v", err)
	}
	if tr2.Offset() != 0 {
		t.Fatalf("offset after abort = %d, want 0", tr2.Offset())
	}
	tr2.Close()
}

func TestTransferContextCancellation(t *testing.T) {
	s := newStore(t, 1<<20)
	data := payload(50_000)
	h := hashOf(data)

	// Context already cancelled
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tr, err := s.Begin(h, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	err = tr.Copy(ctx, bytes.NewReader(data))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestCheckSpaceCalculations(t *testing.T) {
	s := newStore(t, 10<<20)
	s.SetReserve(1000)

	if s.Reserve() != 1000 {
		t.Fatalf("Reserve = %d, want 1000", s.Reserve())
	}

	var freeBytes uint64 = 5000
	s.SetFreeFunc(func(string) (uint64, error) {
		return freeBytes, nil
	})

	// need = 4000: 4000 + 1000 = 5000 == free (just enough space)
	if err := s.CheckSpace(4000); err != nil {
		t.Fatalf("4000 bytes with 5000 free and 1000 reserve should pass: %v", err)
	}

	// need = 4001: 4001 + 1000 = 5001 > 5000 (not enough space)
	if err := s.CheckSpace(4001); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("4001 bytes should fail with ErrNoSpace, got %v", err)
	}

	// need <= 0: should automatically assume at least 1 byte
	if err := s.CheckSpace(0); err != nil {
		t.Fatalf("need 0 should pass: %v", err)
	}

	// free function fails: where free space cannot be measured, CheckSpace allows the write
	// so the filesystem's own ENOSPC can be caught later by wrapNoSpace.
	s.SetFreeFunc(func(string) (uint64, error) {
		return 0, errors.New("statfs disk failure")
	})
	if _, ok := s.Free(); ok {
		t.Fatalf("expected Free() to report ok=false when probe fails")
	}
	if err := s.CheckSpace(100); err != nil {
		t.Fatalf("expected nil when space cannot be measured, got %v", err)
	}
}

func TestTransferConcurrentAccessContention(t *testing.T) {
	s := newStore(t, 1<<20)
	const numGoroutines = 24
	const iterations = 10

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for g := 0; g < numGoroutines; g++ {
		go func(gid int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(gid * 1000)))
			for i := 0; i < iterations; i++ {
				// 50% of the time, target a shared hash; 50% unique hash
				var raw []byte
				if rng.Intn(2) == 0 {
					raw = []byte(fmt.Sprintf("shared-blob-%d", i%3))
				} else {
					raw = []byte(fmt.Sprintf("unique-%d-%d", gid, i))
				}
				h := hashOf(raw)
				sz := int64(len(raw))

				tr, err := s.Begin(h, sz)
				if err != nil {
					if errors.Is(err, ErrBusy) {
						// Expected when competing for the same hash
						continue
					}
					t.Errorf("goroutine %d: unexpected Begin error: %v", gid, err)
					return
				}

				// Either copy partially and close, abort, or copy fully and commit
				action := rng.Intn(3)
				switch action {
				case 0:
					_ = tr.Copy(context.Background(), bytes.NewReader(raw[:sz/2]))
					tr.Close()
				case 1:
					_ = tr.Copy(context.Background(), bytes.NewReader(raw[:sz/2]))
					tr.Abort()
				case 2:
					_ = tr.Copy(context.Background(), bytes.NewReader(raw))
					_, _ = tr.Commit()
					tr.Close()
				}
			}
		}(g)
	}

	wg.Wait()
}

func TestTransferDeclaredSizeLiesOnResume(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 1<<20)
	data := payload(10_000)
	h := hashOf(data)

	// Transfer 6000 bytes with declared size 10,000
	tr, err := s.Begin(h, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	_ = tr.Copy(ctx, bytes.NewReader(data[:6000]))
	tr.Close()

	// Try to resume with declared size 4000 (which is less than partial size 6000).
	// The store detects it is not the blob we thought it was, truncates the partial to 0,
	// and resets the transfer to offset 0.
	tr2, err := s.Begin(h, 4000)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tr2.Close()

	if tr2.Offset() != 0 {
		t.Fatalf("expected offset 0 after truncation of invalid partial, got %d", tr2.Offset())
	}
	if sz := s.PartialSize(h); sz != 0 {
		t.Fatalf("partial size = %d, want 0", sz)
	}
}

func TestTransferENOSPCDuringCopy(t *testing.T) {
	s := newStore(t, 1<<20)
	data := payload(1000)
	h := hashOf(data)

	tr, err := s.Begin(h, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	// Simulating ENOSPC during copy
	enospcErr := &os.PathError{Op: "write", Path: "test", Err: syscall.ENOSPC}
	cut := io.MultiReader(bytes.NewReader(data[:100]), iotest.ErrReader(enospcErr))
	err = tr.Copy(context.Background(), cut)
	if !errors.Is(err, ErrNoSpace) {
		t.Fatalf("expected ErrNoSpace, got %v", err)
	}
}

func TestTransferMultipleAbortsAndClosesAreIdempotent(t *testing.T) {
	s := newStore(t, 1<<20)
	data := payload(1000)
	h := hashOf(data)

	tr, err := s.Begin(h, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}

	// Repeated Abort and Close calls should never panic
	tr.Abort()
	tr.Abort()
	tr.Close()
	tr.Close()

	if s.PartialSize(h) != 0 {
		t.Fatal("partial should be 0 after abort")
	}
}

func BenchmarkTransferResumeAndCommit(b *testing.B) {
	s := newStore(b, 32<<20)
	const blobSize = 1 << 20 // 1 MiB
	data := payload(blobSize)
	h := hashOf(data)
	ctx := context.Background()

	b.SetBytes(blobSize)
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		// Begin and copy first half
		tr, err := s.Begin(h, blobSize)
		if err != nil {
			b.Fatal(err)
		}
		if err := tr.Copy(ctx, bytes.NewReader(data[:blobSize/2])); err != nil {
			b.Fatal(err)
		}
		tr.Close()

		// Resume and copy second half, then commit
		tr2, err := s.Begin(h, blobSize)
		if err != nil {
			b.Fatal(err)
		}
		if err := tr2.Copy(ctx, bytes.NewReader(data[tr2.Offset():])); err != nil {
			b.Fatal(err)
		}
		if _, err := tr2.Commit(); err != nil {
			b.Fatal(err)
		}
		tr2.Close()

		// Remove for next iteration
		_ = s.Remove(h)
	}
}

func FuzzTransferChunking(f *testing.F) {
	f.Add([]byte("hello world fuzzed transfer chunking test"), 5)
	f.Add([]byte("a slightly longer string of bytes to test offset cutting and boundary logic"), 20)

	f.Fuzz(func(t *testing.T, data []byte, cutAt int) {
		if len(data) == 0 || len(data) > 100_000 {
			return
		}
		s, err := New(t.TempDir(), int64(len(data)*2+1024))
		if err != nil {
			t.Fatal(err)
		}
		h := hashOf(data)
		ctx := context.Background()

		cut := cutAt % len(data)
		if cut < 0 {
			cut = -cut
		}

		tr, err := s.Begin(h, int64(len(data)))
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}

		// First segment
		if cut > 0 {
			if err := tr.Copy(ctx, bytes.NewReader(data[:cut])); err != nil {
				t.Fatalf("Copy first: %v", err)
			}
		}
		tr.Close()

		// Resume
		tr2, err := s.Begin(h, int64(len(data)))
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		defer tr2.Close()

		if tr2.Offset() != int64(cut) {
			t.Fatalf("offset = %d, want %d", tr2.Offset(), cut)
		}

		if err := tr2.Copy(ctx, bytes.NewReader(data[cut:])); err != nil {
			t.Fatalf("Copy rest: %v", err)
		}
		ref, err := tr2.Commit()
		if err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if ref.Hash != h || ref.Size != int64(len(data)) {
			t.Fatalf("ref mismatch: got %+v, want hash=%s size=%d", ref, h, len(data))
		}
	})
}
