package syncer

import (
	"errors"
	"math"
	"net/http"
	"sync"
	"testing"
	"time"

	"offgrid/internal/store/sqlstore"
)

func TestBackoffOverflowSafety(t *testing.T) {
	bo := &Backoff{Min: time.Second, Max: 30 * time.Second}
	transientErr := errors.New("temporary network timeout")

	// Call Next 100 times to test bitshift overflow prevention (fails < 62)
	for i := 0; i < 100; i++ {
		wait := bo.Next(transientErr)
		if wait < 0 {
			t.Fatalf("iteration %d: negative backoff wait %v (overflow)", i, wait)
		}
		if wait > 30*time.Second {
			t.Fatalf("iteration %d: backoff %v exceeded Max 30s", i, wait)
		}
		if bo.Failures() > 62 {
			t.Fatalf("bo.fails grew beyond 62: %d", bo.Failures())
		}
	}
}

func TestBackoffZeroAndNegativeClamping(t *testing.T) {
	bo := &Backoff{Min: -5 * time.Second, Max: -10 * time.Second}
	wait := bo.Next(errors.New("err"))

	// Min clamped to time.Second, Max clamped to Min
	if bo.Min != time.Second {
		t.Errorf("expected Min to be clamped to 1s, got %v", bo.Min)
	}
	if bo.Max != time.Second {
		t.Errorf("expected Max to be clamped to 1s, got %v", bo.Max)
	}
	if wait < 500*time.Millisecond || wait > time.Second {
		t.Errorf("expected wait between 500ms and 1s, got %v", wait)
	}
}

func TestBackoffTransientErrorClassificationMatrix(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		transient bool
	}{
		{"nil error", nil, false},
		{"bad signature", ErrBadSignature, false},
		{"protocol error", ErrProtocol, false},
		{"bad frame error", ErrBadFrame, false},
		{"upgrade required remote error", &RemoteError{Status: http.StatusUpgradeRequired, Code: CodeUpgradeRequired}, false},
		{"unauthorized remote error", &RemoteError{Status: http.StatusUnauthorized, Code: CodeUnauthorized}, false},
		{"not authorized remote error", &RemoteError{Status: http.StatusForbidden, Code: CodeNotAuthorized}, false},
		{"replay conflict error", &RemoteError{Status: http.StatusConflict, Code: CodeReplay}, false},
		{"bad request error", &RemoteError{Status: http.StatusBadRequest, Code: CodeBadRequest}, false},
		{"busy peer 503 error", &RemoteError{Status: http.StatusServiceUnavailable, Code: CodeBusy}, true},
		{"internal server 500 error", &RemoteError{Status: http.StatusInternalServerError, Code: CodeInternal}, true},
		{"too many requests 429 error", &RemoteError{Status: http.StatusTooManyRequests, Code: "rate_limited"}, true},
		{"stream cut error", ErrStreamCut, true},
		{"generic connection failure", errors.New("connection refused"), true},
		{"store not authorized", sqlstore.ErrNotAuthorized, false},
		{"store chain mismatch", sqlstore.ErrChain, false},
		{"store fork error", sqlstore.ErrFork, false},
		{"store bad signature", sqlstore.ErrBadSignature, false},
		{"store bad op", sqlstore.ErrBadOp, false},
		{"store key change", sqlstore.ErrKeyChange, false},
		{"store gap error (healable)", sqlstore.ErrGap, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Transient(tc.err)
			if got != tc.transient {
				t.Errorf("Transient(%v) = %v, want %v", tc.err, got, tc.transient)
			}
		})
	}
}

func TestBackoffJitterStrictBounds(t *testing.T) {
	bo := &Backoff{Min: 100 * time.Millisecond, Max: 10 * time.Second}
	err := errors.New("connection dropped")

	for fails := 1; fails <= 10; fails++ {
		ceiling := bo.Min << (fails - 1)
		if ceiling > bo.Max {
			ceiling = bo.Max
		}
		half := ceiling / 2

		// Test with minimum possible rand (0.0)
		bo.Rand = func() float64 { return 0.0 }
		bo.fails = fails - 1
		wMin := bo.Next(err)
		if wMin != half {
			t.Errorf("fails=%d: expected min jitter %v, got %v", fails, half, wMin)
		}

		// Test with max possible rand approaching 1.0 (0.999999)
		bo.Rand = func() float64 { return math.Nextafter(1.0, 0.0) }
		bo.fails = fails - 1
		wMax := bo.Next(err)
		if wMax < half || wMax > ceiling {
			t.Errorf("fails=%d: expected max jitter within [%v, %v], got %v", fails, half, ceiling, wMax)
		}
	}
}

func TestBackoffConcurrentAccess(t *testing.T) {
	bo := &Backoff{Min: 10 * time.Millisecond, Max: 500 * time.Millisecond}
	var wg sync.WaitGroup
	const goroutines = 20
	const iterations = 50

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				if i%10 == 0 {
					bo.Reset()
				} else {
					err := errors.New("concurrent transient error")
					wait := bo.Next(err)
					if wait < 0 {
						t.Errorf("negative wait returned: %v", wait)
					}
					_ = bo.Failures()
				}
			}
		}(g)
	}
	wg.Wait()
}

func TestBackoffStoreErrorClassification(t *testing.T) {
	bo := &Backoff{Min: time.Second, Max: 60 * time.Second}

	// Non-transient store errors should immediately produce Max delay (60s)
	nonTransient := []error{
		sqlstore.ErrNotAuthorized,
		sqlstore.ErrChain,
		sqlstore.ErrFork,
		sqlstore.ErrBadSignature,
		sqlstore.ErrBadOp,
		sqlstore.ErrKeyChange,
	}

	for _, err := range nonTransient {
		bo.Reset()
		wait := bo.Next(err)
		if wait != 60*time.Second {
			t.Errorf("expected non-transient store error %v to yield Max (60s), got %v", err, wait)
		}
	}

	// Transient store error (ErrGap) should produce exponential backoff with jitter, not Max
	bo.Reset()
	wait := bo.Next(sqlstore.ErrGap)
	if wait > time.Second || wait < 500*time.Millisecond {
		t.Errorf("expected first failure with ErrGap to be between 500ms and 1s, got %v", wait)
	}
}
