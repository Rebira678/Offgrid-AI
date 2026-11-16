package syncer

import (
	"errors"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"offgrid/internal/store/sqlstore"
)

// Backoff spaces out retries after failed rounds: the wait doubles with each failure up to Max, with
// jitter (between half the cap and the cap) so devices that failed together do not all retry together.
// A success resets it. A failure that waiting cannot fix (a wrong key, a revoked device, a peer on another
// protocol version, a tampered answer) waits the full Max at once, because hammering the peer helps nobody; the
// round is still retried, so fixing the cause is picked up without restarting. A peer's Retry-After is a
// floor for the wait.
type Backoff struct {
	Min, Max time.Duration
	// Rand returns a number in [0, 1); nil uses math/rand.
	Rand func() float64

	mu    sync.Mutex
	fails int
}

// Next is called after a failed round and returns how long to wait before the next one.
func (b *Backoff) Next(err error) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.Min <= 0 {
		b.Min = time.Second
	}
	if b.Max < b.Min {
		b.Max = b.Min
	}
	if b.fails < 62 {
		b.fails++
	}
	var wait time.Duration
	if !Transient(err) {
		wait = b.Max
	} else {
		ceiling := b.Max
		if b.fails-1 < 40 {
			if c := b.Min << (b.fails - 1); c > 0 && c < ceiling {
				ceiling = c
			}
		}
		r := rand.Float64
		if b.Rand != nil {
			r = b.Rand
		}
		wait = ceiling/2 + time.Duration(r()*float64(ceiling-ceiling/2))
	}
	var re *RemoteError
	if errors.As(err, &re) && re.RetryAfter > wait {
		wait = re.RetryAfter
	}
	if wait > b.Max {
		wait = b.Max
	}
	return wait
}

// Reset is called after a round that worked.
func (b *Backoff) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fails = 0
}

// Failures says how many rounds in a row have failed.
func (b *Backoff) Failures() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fails
}

// Transient says whether waiting and trying again can plausibly help: a network error, a cut stream, a
// server error or a busy peer, yes; a refusal of who we are or what we sent, no.
func Transient(err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, ErrBadSignature), errors.Is(err, ErrProtocol), errors.Is(err, ErrBadFrame):
		return false
	case errors.Is(err, sqlstore.ErrNotAuthorized), errors.Is(err, sqlstore.ErrBadSignature),
		errors.Is(err, sqlstore.ErrChain), errors.Is(err, sqlstore.ErrFork),
		errors.Is(err, sqlstore.ErrBadOp), errors.Is(err, sqlstore.ErrKeyChange):
		return false
	}
	var re *RemoteError
	if errors.As(err, &re) {
		return re.Status >= http.StatusInternalServerError || re.Status == http.StatusTooManyRequests || re.Code == CodeBusy
	}
	return true
}
