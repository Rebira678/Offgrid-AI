package hub

import (
	"testing"
	"time"
)

// FuzzLimiterAllow tests the token bucket limiter invariants under arbitrary rates,
// bursts, and simulated time steps.
func FuzzLimiterAllow(f *testing.F) {
	f.Add(10.0, 20, int64(100))
	f.Add(1.0, 1, int64(1000))
	f.Add(0.0, 0, int64(-50))
	f.Add(-5.0, -1, int64(0))
	f.Add(100000.0, 50000, int64(5000000))
	f.Add(1e-6, 5, int64(1000000000))

	f.Fuzz(func(t *testing.T, rate float64, burst int, stepNanos int64) {
		nowNano := int64(1700000000000000000)
		clockFn := func() time.Time {
			return time.Unix(0, nowNano)
		}

		l := NewLimiter(rate, burst, clockFn)
		if l == nil {
			t.Fatal("NewLimiter returned nil")
		}

		// Perform multiple Allow iterations with varying time steps
		for i := 0; i < 10; i++ {
			ok, retry := l.Allow()
			l.mu.Lock()
			tokens := l.tokens
			curBurst := l.burst
			l.mu.Unlock()

			if tokens < 0 {
				t.Fatalf("tokens fell below 0: %v", tokens)
			}
			if tokens > curBurst {
				t.Fatalf("tokens %v exceeded burst %v", tokens, curBurst)
			}
			if !ok && retry < 0 {
				t.Fatalf("refused request returned negative retry duration: %v", retry)
			}

			// Advance time
			if stepNanos > 0 && stepNanos < int64(365*24*time.Hour) {
				nowNano += stepNanos
			} else if stepNanos < 0 && stepNanos > -int64(365*24*time.Hour) {
				nowNano += stepNanos // simulated backwards clock jump
			}
		}
	})
}
