package envelope

import (
	"errors"
	"math"
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

func TestScopeInvalidPurposeRejected(t *testing.T) {
	s := NewScope()
	for _, badP := range []Purpose{0, 3, 99, 255} {
		if _, err := s.Next(badP); !errors.Is(err, ErrInvalidPurpose) {
			t.Errorf("purpose %d: expected ErrInvalidPurpose, got %v", badP, err)
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
