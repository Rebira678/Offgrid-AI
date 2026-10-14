// Package jobqueue is a durable job queue on SQLite: work that cannot be done
// right now (the model is too slow, the network is down) is written to disk
// and retried later, surviving restarts and crashes.
package jobqueue

import (
	"errors"
	"fmt"
	"time"
)

// Jitter says how randomness is mixed into a retry delay.
type Jitter int

const (
	// NoJitter: every retry waits exactly the ceiling. Predictable, and the
	// reason for retry storms: a thousand clients that failed together all
	// come back together, at the same instant, every time.
	NoJitter Jitter = iota
	// EqualJitter: half the ceiling is guaranteed, the other half is random.
	// Spreads retries out while still guaranteeing a pause. The default.
	EqualJitter
	// FullJitter: anywhere from zero to the ceiling. The best spreading, but
	// a retry can come back almost immediately.
	FullJitter
)

func (j Jitter) String() string {
	switch j {
	case NoJitter:
		return "none"
	case EqualJitter:
		return "equal"
	case FullJitter:
		return "full"
	}
	return fmt.Sprintf("Jitter(%d)", int(j))
}

// ParseJitter turns "none", "equal" or "full" into a Jitter.
func ParseJitter(s string) (Jitter, error) {
	switch s {
	case "none":
		return NoJitter, nil
	case "equal":
		return EqualJitter, nil
	case "full":
		return FullJitter, nil
	}
	return 0, fmt.Errorf("unknown jitter %q (use none, equal or full)", s)
}

// Backoff computes how long to wait before retrying. The ceiling doubles with
// each failed attempt (Base, 2 x Base, 4 x Base, ...) up to Max; jitter then
// picks the actual delay at or below the ceiling.
type Backoff struct {
	Base   time.Duration
	Max    time.Duration
	Jitter Jitter
}

// DefaultBackoff: 2 s, 4 s, 8 s ... capped at 5 minutes, with equal jitter.
func DefaultBackoff() Backoff {
	return Backoff{Base: 2 * time.Second, Max: 5 * time.Minute, Jitter: EqualJitter}
}

// Validate rejects settings that make no sense.
func (b Backoff) Validate() error {
	switch {
	case b.Base <= 0:
		return errors.New("backoff base must be positive")
	case b.Max < b.Base:
		return fmt.Errorf("backoff max (%v) must be at least the base (%v)", b.Max, b.Base)
	case b.Jitter < NoJitter || b.Jitter > FullJitter:
		return fmt.Errorf("unknown jitter %d", int(b.Jitter))
	}
	return nil
}

// Ceiling is the longest the delay can be after the given failed attempt
// (1 = the first failure). It never overflows: a job that has failed a
// thousand times simply gets Max.
func (b Backoff) Ceiling(attempt int) time.Duration {
	d := b.Base
	for i := 1; i < attempt; i++ {
		if d >= b.Max/2+1 { // doubling would reach or pass Max (and cannot overflow)
			return b.Max
		}
		d *= 2
	}
	if d > b.Max {
		return b.Max
	}
	return d
}

// Delay is the wait before retry number `attempt`. r is a random number in
// [0, 1) supplied by the caller, so tests can choose it exactly (the queue
// passes a real random number).
func (b Backoff) Delay(attempt int, r float64) time.Duration {
	c := b.Ceiling(attempt)
	if r < 0 {
		r = 0
	}
	if r >= 1 {
		r = 0.999999999
	}
	switch b.Jitter {
	case EqualJitter:
		half := c / 2
		return half + time.Duration(r*float64(c-half))
	case FullJitter:
		return time.Duration(r * float64(c))
	default:
		return c
	}
}
