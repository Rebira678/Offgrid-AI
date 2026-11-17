package envelope

import (
	"encoding/hex"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
)

func TestParseNonceRoundTrip(t *testing.T) {
	testCounters := []uint64{
		0,
		1,
		2,
		255,
		256,
		65535,
		0xdeadbeef,
		(1 << 32) - 1,
		1 << 32,
		(1 << 48) + 1234,
		math.MaxUint64 - 1,
		math.MaxUint64,
	}

	for _, p := range []Purpose{PurposeBody, PurposeWrap} {
		for _, c := range testCounters {
			n := Nonce(p, c)
			gotP, gotC, err := ParseNonce(n)
			if err != nil {
				t.Fatalf("ParseNonce(Nonce(%v, %d)) returned unexpected error: %v", p, c, err)
			}
			if gotP != p {
				t.Errorf("ParseNonce purpose mismatch: got %v, want %v", gotP, p)
			}
			if gotC != c {
				t.Errorf("ParseNonce counter mismatch: got %d, want %d", gotC, c)
			}
		}
	}
}

func TestParseNonceTamperedPaddingRejected(t *testing.T) {
	valid := Nonce(PurposeBody, 42)

	// Test corruption in each padding byte (indices 1, 2, 3)
	for padIdx := 1; padIdx <= 3; padIdx++ {
		for _, badByte := range []byte{0x01, 0x10, 0x7f, 0x80, 0xff} {
			corrupt := valid
			corrupt[padIdx] = badByte

			_, _, err := ParseNonce(corrupt)
			if !errors.Is(err, ErrMalformedNonce) {
				t.Errorf("padIdx %d = 0x%02x: expected ErrMalformedNonce, got %v", padIdx, badByte, err)
			}
		}
	}
}

func TestParseNonceInvalidPurposeRejected(t *testing.T) {
	for _, badP := range []byte{0x00, 0x03, 0x10, 0x80, 0xff} {
		n := Nonce(Purpose(badP), 100)
		_, _, err := ParseNonce(n)
		if !errors.Is(err, ErrInvalidPurpose) {
			t.Errorf("purpose 0x%02x: expected ErrInvalidPurpose, got %v", badP, err)
		}
	}
}

func TestScopeNilReceiverSafety(t *testing.T) {
	var s *Scope
	if _, err := s.Next(PurposeBody); !errors.Is(err, ErrNilScope) {
		t.Fatalf("expected ErrNilScope on nil Scope.Next, got %v", err)
	}
	if count := s.Count(PurposeBody); count != 0 {
		t.Fatalf("expected 0 count on nil Scope.Count, got %d", count)
	}
	if rem := s.Remaining(PurposeBody); rem != 0 {
		t.Fatalf("expected 0 remaining on nil Scope.Remaining, got %d", rem)
	}
}

func TestScopeInvalidPurposeRejected(t *testing.T) {
	s := NewScope()
	for _, badP := range []Purpose{0, 3, 99, 255} {
		if _, err := s.Next(badP); !errors.Is(err, ErrInvalidPurpose) {
			t.Errorf("purpose %d: expected ErrInvalidPurpose, got %v", badP, err)
		}
	}
}

func TestScopeCustomLimits(t *testing.T) {
	t.Run("limit 0 immediately exhausted", func(t *testing.T) {
		s0 := NewScopeWithLimit(0)
		if rem := s0.Remaining(PurposeBody); rem != 0 {
			t.Fatalf("expected 0 remaining on limit 0, got %d", rem)
		}
		if _, err := s0.Next(PurposeBody); !errors.Is(err, ErrExhausted) {
			t.Fatalf("expected ErrExhausted on limit 0, got %v", err)
		}
		if s0.Count(PurposeBody) != 0 {
			t.Fatalf("expected count 0, got %d", s0.Count(PurposeBody))
		}
	})

	t.Run("limit 1 allows exactly 1 draw", func(t *testing.T) {
		s1 := NewScopeWithLimit(1)
		if rem := s1.Remaining(PurposeBody); rem != 1 {
			t.Fatalf("expected 1 remaining, got %d", rem)
		}

		n, err := s1.Next(PurposeBody)
		if err != nil {
			t.Fatalf("unexpected error on first draw: %v", err)
		}
		if n != Nonce(PurposeBody, 0) {
			t.Fatalf("got nonce %x, want counter 0", n)
		}
		if rem := s1.Remaining(PurposeBody); rem != 0 {
			t.Fatalf("expected 0 remaining after single draw, got %d", rem)
		}

		for i := 0; i < 5; i++ {
			if _, err := s1.Next(PurposeBody); !errors.Is(err, ErrExhausted) {
				t.Fatalf("draw %d: expected ErrExhausted, got %v", i+2, err)
			}
		}
		if s1.Count(PurposeBody) != 1 {
			t.Fatalf("expected count 1, got %d", s1.Count(PurposeBody))
		}
	})
}

func TestScopeIndependentPurposesUnderCustomLimit(t *testing.T) {
	s := NewScopeWithLimit(2)

	// Draw 2 body nonces
	for i := uint64(0); i < 2; i++ {
		if rem := s.Remaining(PurposeBody); rem != 2-i {
			t.Fatalf("body rem: got %d, want %d", rem, 2-i)
		}
		n, err := s.Next(PurposeBody)
		if err != nil {
			t.Fatalf("body draw %d: unexpected error: %v", i, err)
		}
		if n != Nonce(PurposeBody, i) {
			t.Fatalf("body draw %d: got %x, want counter %d", i, n, i)
		}
	}

	// 3rd body draw must fail
	if _, err := s.Next(PurposeBody); !errors.Is(err, ErrExhausted) {
		t.Fatalf("body draw 3: expected ErrExhausted, got %v", err)
	}

	// Wrap nonces must still succeed up to 2
	for i := uint64(0); i < 2; i++ {
		if rem := s.Remaining(PurposeWrap); rem != 2-i {
			t.Fatalf("wrap rem: got %d, want %d", rem, 2-i)
		}
		n, err := s.Next(PurposeWrap)
		if err != nil {
			t.Fatalf("wrap draw %d: unexpected error: %v", i, err)
		}
		if n != Nonce(PurposeWrap, i) {
			t.Fatalf("wrap draw %d: got %x, want counter %d", i, n, i)
		}
	}

	// 3rd wrap draw must fail
	if _, err := s.Next(PurposeWrap); !errors.Is(err, ErrExhausted) {
		t.Fatalf("wrap draw 3: expected ErrExhausted, got %v", err)
	}

	if s.Count(PurposeBody) != 2 {
		t.Fatalf("expected body count 2, got %d", s.Count(PurposeBody))
	}
	if s.Count(PurposeWrap) != 2 {
		t.Fatalf("expected wrap count 2, got %d", s.Count(PurposeWrap))
	}
}

func TestScopeMaxUint64OverflowSafety(t *testing.T) {
	s := NewScopeWithLimit(math.MaxUint64)
	s.next[PurposeBody] = math.MaxUint64

	// If counter is at MaxUint64, it must return ErrExhausted rather than overflowing to 0
	if _, err := s.Next(PurposeBody); !errors.Is(err, ErrExhausted) {
		t.Fatalf("expected ErrExhausted at MaxUint64 boundary, got %v", err)
	}
}

func TestConcurrentMixedPurposesStress(t *testing.T) {
	s := NewScope()
	const goroutines = 32
	const drawsPerRoutine = 2_000

	var mu sync.Mutex
	seen := make(map[[NonceSize]byte]struct{}, goroutines*drawsPerRoutine)

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		routineID := g
		go func() {
			defer wg.Done()
			for i := 0; i < drawsPerRoutine; i++ {
				// Alternate between PurposeBody and PurposeWrap
				p := PurposeBody
				if (routineID+i)%2 == 1 {
					p = PurposeWrap
				}

				n, err := s.Next(p)
				if err != nil {
					t.Errorf("routine %d iteration %d: unexpected error: %v", routineID, i, err)
					return
				}

				// Validate on the fly
				parsedP, _, parseErr := ParseNonce(n)
				if parseErr != nil {
					t.Errorf("routine %d: parse error on nonce %x: %v", routineID, n, parseErr)
					return
				}
				if parsedP != p {
					t.Errorf("routine %d: parsed purpose %v != expected %v", routineID, parsedP, p)
					return
				}

				mu.Lock()
				if _, dup := seen[n]; dup {
					t.Errorf("collision detected! Nonce %x issued twice", n)
				}
				seen[n] = struct{}{}
				mu.Unlock()
			}
		}()
	}

	wg.Wait()

	totalExpected := goroutines * drawsPerRoutine
	if len(seen) != totalExpected {
		t.Fatalf("expected %d distinct nonces, got %d", totalExpected, len(seen))
	}

	totalCounted := s.Count(PurposeBody) + s.Count(PurposeWrap)
	if totalCounted != uint64(totalExpected) {
		t.Fatalf("expected total count %d, got %d", totalExpected, totalCounted)
	}
}

func TestConcurrentExhaustionRace(t *testing.T) {
	const limit = 250
	s := NewScopeWithLimit(limit)

	const workers = 20
	const drawsPerWorker = 50 // total 1,000 attempts for 250 available nonces

	var successCount atomic.Int64
	var exhaustedCount atomic.Int64
	var mu sync.Mutex
	seen := make(map[[NonceSize]byte]struct{}, limit)

	var wg sync.WaitGroup
	wg.Add(workers)

	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < drawsPerWorker; i++ {
				n, err := s.Next(PurposeBody)
				if err == nil {
					successCount.Add(1)
					mu.Lock()
					if _, dup := seen[n]; dup {
						t.Errorf("duplicate nonce issued in race: %x", n)
					}
					seen[n] = struct{}{}
					mu.Unlock()
				} else if errors.Is(err, ErrExhausted) {
					exhaustedCount.Add(1)
				} else {
					t.Errorf("unexpected error: %v", err)
				}
			}
		}()
	}

	wg.Wait()

	if successCount.Load() != limit {
		t.Fatalf("expected %d successful draws, got %d", limit, successCount.Load())
	}
	if exhaustedCount.Load() != (workers*drawsPerWorker)-limit {
		t.Fatalf("expected %d exhausted draws, got %d", (workers*drawsPerWorker)-limit, exhaustedCount.Load())
	}
	if len(seen) != limit {
		t.Fatalf("expected %d unique nonces in seen map, got %d", limit, len(seen))
	}
}

func TestPurposeStringAndValidity(t *testing.T) {
	if !PurposeBody.Valid() {
		t.Error("PurposeBody should be valid")
	}
	if !PurposeWrap.Valid() {
		t.Error("PurposeWrap should be valid")
	}
	if Purpose(0).Valid() {
		t.Error("Purpose(0) must not be valid")
	}
	if Purpose(42).Valid() {
		t.Error("Purpose(42) must not be valid")
	}

	if PurposeBody.String() != "body" {
		t.Errorf("got %q, want %q", PurposeBody.String(), "body")
	}
	if PurposeWrap.String() != "wrap" {
		t.Errorf("got %q, want %q", PurposeWrap.String(), "wrap")
	}
	if Purpose(0).String() != "unknown(0)" {
		t.Errorf("got %q, want %q", Purpose(0).String(), "unknown(0)")
	}
	if Purpose(99).String() != "unknown(99)" {
		t.Errorf("got %q, want %q", Purpose(99).String(), "unknown(99)")
	}
}

func TestNonceByteEndiannessExplicitLayout(t *testing.T) {
	// Verify big-endian wire format guarantees
	counter := uint64(0x1122334455667788)
	n := Nonce(PurposeBody, counter)

	want := [NonceSize]byte{
		0x01,                   // purpose = PurposeBody
		0x00, 0x00, 0x00,       // 3 zero padding bytes
		0x11, 0x22, 0x33, 0x44, // big-endian upper 4 bytes
		0x55, 0x66, 0x77, 0x88, // big-endian lower 4 bytes
	}

	if n != want {
		t.Fatalf("layout mismatch:\ngot:  %#v\nwant: %#v", n, want)
	}
}

func TestNonceFromBytes(t *testing.T) {
	valid := Nonce(PurposeBody, 12345)

	t.Run("valid slice", func(t *testing.T) {
		got, err := NonceFromBytes(valid[:])
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !Equal(got, valid) {
			t.Fatalf("got %x, want %x", got, valid)
		}
	})

	t.Run("invalid lengths", func(t *testing.T) {
		lengths := []int{0, 1, 11, 13, 20}
		for _, l := range lengths {
			buf := make([]byte, l)
			_, err := NonceFromBytes(buf)
			if !errors.Is(err, ErrInvalidNonceSize) {
				t.Errorf("len %d: expected ErrInvalidNonceSize, got %v", l, err)
			}
		}
		_, errNil := NonceFromBytes(nil)
		if !errors.Is(errNil, ErrInvalidNonceSize) {
			t.Errorf("nil slice: expected ErrInvalidNonceSize, got %v", errNil)
		}
	})

	t.Run("invalid content in 12-byte slice", func(t *testing.T) {
		corrupt := valid
		corrupt[1] = 0x99 // violate padding
		_, err := NonceFromBytes(corrupt[:])
		if !errors.Is(err, ErrMalformedNonce) {
			t.Errorf("expected ErrMalformedNonce, got %v", err)
		}
	})
}

func TestNonceHexEncodingAndDecoding(t *testing.T) {
	n := Nonce(PurposeWrap, 0xaabbcc)
	encoded := NonceHex(n)

	if len(encoded) != NonceHexLen {
		t.Fatalf("expected hex len %d, got %d (%s)", NonceHexLen, len(encoded), encoded)
	}

	decoded, err := NonceFromHex(encoded)
	if err != nil {
		t.Fatalf("NonceFromHex failed: %v", err)
	}
	if !Equal(decoded, n) {
		t.Fatalf("decoded %x != original %x", decoded, n)
	}

	// Invalid hex strings
	invalidHexes := []string{
		"",
		"010203",                                       // too short
		"01000000000000000000000000",                   // 26 chars (too long)
		"01000000000000000000002z",                     // non-hex character 'z'
		"000000000000000000000001",                     // invalid purpose 0
		"010100000000000000000001",                     // non-zero padding byte at index 1
	}
	for _, ih := range invalidHexes {
		_, err := NonceFromHex(ih)
		if err == nil {
			t.Errorf("expected error for hex %q, got nil", ih)
		}
	}
}

func TestConstantTimeEqual(t *testing.T) {
	a := Nonce(PurposeBody, 100)
	b := Nonce(PurposeBody, 100)
	c := Nonce(PurposeBody, 101)
	d := Nonce(PurposeWrap, 100)

	if !Equal(a, b) {
		t.Error("identical nonces must be Equal")
	}
	if Equal(a, c) {
		t.Error("nonces with different counters must not be Equal")
	}
	if Equal(a, d) {
		t.Error("nonces with different purposes must not be Equal")
	}

	// Bit-flip in padding byte
	corrupt := a
	corrupt[2] ^= 0x01
	if Equal(a, corrupt) {
		t.Error("nonces with padding difference must not be Equal")
	}
}

func BenchmarkNonceDerivationZeroAlloc(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = Nonce(PurposeBody, uint64(i))
	}
}

func BenchmarkScopeNextWarm(b *testing.B) {
	s := NewScopeWithLimit(math.MaxUint64)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = s.Next(PurposeBody)
	}
}

func BenchmarkConstantTimeEqual(b *testing.B) {
	n1 := Nonce(PurposeBody, 42)
	n2 := Nonce(PurposeBody, 42)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = Equal(n1, n2)
	}
}

func BenchmarkNonceHexEncoding(b *testing.B) {
	n := Nonce(PurposeBody, 0x12345678)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = NonceHex(n)
	}
}

func FuzzParseNonce(f *testing.F) {
	f.Add(byte(1), uint64(0))
	f.Add(byte(2), uint64(42))
	f.Add(byte(1), uint64(1<<32))
	f.Add(byte(0), uint64(10))
	f.Add(byte(99), uint64(100))

	f.Fuzz(func(t *testing.T, purposeByte byte, counter uint64) {
		p := Purpose(purposeByte)
		n := Nonce(p, counter)
		gotP, gotC, err := ParseNonce(n)
		if p.Valid() {
			if err != nil {
				t.Fatalf("valid purpose %d failed: %v", p, err)
			}
			if gotP != p || gotC != counter {
				t.Fatalf("mismatch: got (%v, %d), want (%v, %d)", gotP, gotC, p, counter)
			}
		} else {
			if err == nil {
				t.Fatalf("invalid purpose %d unexpectedly succeeded", purposeByte)
			}
		}
	})
}

func FuzzNonceFromHex(f *testing.F) {
	f.Add("010000000000000000000001")
	f.Add("02000000000000000000002a")
	f.Add("000000000000000000000000")
	f.Add("invalid")

	f.Fuzz(func(t *testing.T, rawHex string) {
		n, err := NonceFromHex(rawHex)
		if err == nil {
			// If it decoded successfully, encoding it back must match lowercase
			reEncoded := NonceHex(n)
			if hex.EncodeToString(n[:]) != reEncoded {
				t.Fatalf("hex re-encode mismatch: %s vs %s", reEncoded, hex.EncodeToString(n[:]))
			}
		}
	})
}
