package syncer

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestBackoffGrowsToItsCapWithJitterAndResets(t *testing.T) {
	boom := errors.New("cannot reach the peer")
	lo := &Backoff{Min: time.Second, Max: 30 * time.Second, Rand: func() float64 { return 0 }}
	hi := &Backoff{Min: time.Second, Max: 30 * time.Second, Rand: func() float64 { return 0.999999 }}
	// caps: 1, 2, 4, 8, 16, 30, 30 ... each wait between half the cap and the cap
	for i, cp := range []time.Duration{1, 2, 4, 8, 16, 30, 30, 30} {
		cp *= time.Second
		if w := lo.Next(boom); w != cp/2 {
			t.Errorf("failure %d, low jitter: %v, want %v", i+1, w, cp/2)
		}
		if w := hi.Next(boom); w < cp-time.Millisecond || w > cp {
			t.Errorf("failure %d, high jitter: %v, want just under %v", i+1, w, cp)
		}
	}
	if lo.Failures() != 8 {
		t.Fatalf("failures = %d", lo.Failures())
	}
	lo.Reset()
	if w := lo.Next(boom); w != 500*time.Millisecond {
		t.Fatalf("after a success the wait starts over: %v", w)
	}
	// a very long run of failures must not overflow into a negative wait
	big := &Backoff{Min: time.Second, Max: time.Hour}
	for i := 0; i < 500; i++ {
		if w := big.Next(boom); w <= 0 || w > time.Hour {
			t.Fatalf("failure %d: wait %v", i, w)
		}
	}
}

func TestBackoffWaitsTheMaxForFailuresThatWaitingCannotFix(t *testing.T) {
	b := &Backoff{Min: time.Second, Max: time.Minute, Rand: func() float64 { return 0 }}
	for name, err := range map[string]error{
		"bad signature":    ErrBadSignature,
		"upgrade required": &RemoteError{Status: http.StatusUpgradeRequired, Code: CodeUpgradeRequired},
		"revoked":          &RemoteError{Status: http.StatusForbidden, Code: CodeNotAuthorized},
		"a fork":           &RemoteError{Status: http.StatusConflict, Code: CodeFork},
		"a bad frame":      fmt.Errorf("pull: %w", ErrBadFrame),
		"another protocol": fmt.Errorf("%w: x", ErrProtocol),
	} {
		b.Reset()
		if Transient(err) {
			t.Errorf("%s counted as transient", name)
		}
		if w := b.Next(err); w != time.Minute {
			t.Errorf("%s: wait %v, want the max", name, w)
		}
	}
	for name, err := range map[string]error{
		"network":      errors.New("cannot reach the peer: connection refused"),
		"a cut stream": fmt.Errorf("pull: %w", ErrStreamCut),
		"a 503":        &RemoteError{Status: 503, Code: CodeInternal},
		"busy":         &RemoteError{Status: 503, Code: CodeBusy},
		"too many":     &RemoteError{Status: 429},
	} {
		b.Reset()
		if !Transient(err) {
			t.Errorf("%s counted as permanent", name)
		}
		if w := b.Next(err); w >= time.Minute {
			t.Errorf("%s: first wait %v, want a short one", name, w)
		}
	}
}

func TestBackoffHonoursRetryAfterButNotBeyondTheMax(t *testing.T) {
	b := &Backoff{Min: time.Second, Max: 10 * time.Second, Rand: func() float64 { return 0 }}
	if w := b.Next(&RemoteError{Status: 503, Code: CodeBusy, RetryAfter: 4 * time.Second}); w != 4*time.Second {
		t.Fatalf("wait %v, want the peer's 4s", w)
	}
	b.Reset()
	if w := b.Next(&RemoteError{Status: 503, Code: CodeBusy, RetryAfter: time.Hour}); w != 10*time.Second {
		t.Fatalf("wait %v, want the cap", w)
	}
}
