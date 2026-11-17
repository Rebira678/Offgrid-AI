package envelope

import (
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
