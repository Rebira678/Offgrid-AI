package envelope

import (
	"sync"
	"testing"
)

// No two draws from one scope are equal, across purposes, however many are drawn.
func TestAScopeNeverRepeatsANonce(t *testing.T) {
	s := NewScope()
	seen := map[[NonceSize]byte]string{}
	for i := 0; i < 50_000; i++ {
		for _, p := range []Purpose{PurposeBody, PurposeWrap} {
			n, err := s.Next(p)
			if err != nil {
				t.Fatal(err)
			}
			if prev, dup := seen[n]; dup {
				t.Fatalf("nonce %x used twice (%s, then purpose %d draw %d)", n, prev, p, i)
			}
			seen[n] = "draw"
		}
	}
}

// The same counter for two purposes is two nonces: wrapping a key and sealing a body can both be "number 0".
func TestPurposesSeparateNonces(t *testing.T) {
	if Nonce(PurposeBody, 0) == Nonce(PurposeWrap, 0) {
		t.Fatal("body and wrap nonces collide at counter 0")
	}
	if Nonce(PurposeBody, 7) == Nonce(PurposeBody, 8) {
		t.Fatal("consecutive counters collide")
	}
	// the layout is part of the design: purpose, three zero bytes, big-endian counter
	want := [NonceSize]byte{2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 2}
	if got := Nonce(PurposeWrap, 0x0102); got != want {
		t.Fatalf("layout = %x, want %x", got, want)
	}
}

// Many goroutines drawing at once still never share a nonce.
func TestConcurrentDrawsNeverShareANonce(t *testing.T) {
	s := NewScope()
	var mu sync.Mutex
	seen := map[[NonceSize]byte]bool{}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 5_000; i++ {
				n, err := s.Next(PurposeWrap)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				if seen[n] {
					t.Errorf("nonce %x handed out twice", n)
				}
				seen[n] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != 40_000 {
		t.Fatalf("%d distinct nonces, want 40000", len(seen))
	}
}

// A key that has used all its nonces refuses a new one, and keeps refusing.
func TestAnExhaustedScopeRefuses(t *testing.T) {
	s := NewScope()
	s.limit = 3
	for i := 0; i < 3; i++ {
		if _, err := s.Next(PurposeBody); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := s.Next(PurposeBody); err != ErrExhausted {
			t.Fatalf("draw past the limit: %v, want ErrExhausted", err)
		}
	}
	if _, err := s.Next(PurposeWrap); err != nil { // another purpose counts on its own
		t.Fatal(err)
	}
}
