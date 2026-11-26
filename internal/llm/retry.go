package llm

import (
	"context"
	"errors"
	"math/rand"
	"time"
)

// RetryPolicy says how a Client that failed with ErrUnavailable is tried again.
//
// Only ErrUnavailable is retried: it is the one error that "may work later". A missing model, a
// rejected key, a malformed request and a cancelled context will fail the same way again, so they
// return at once. A reply that had already started streaming is never retried either: the caller has
// seen pieces of it, and a second answer would be glued onto the first.
//
// The wait before retry n is a random time between 0 and min(Max, Base*2^n) ("full jitter"), so a crew
// of devices that lost the same LAN node do not all knock on it again at the same instant.
type RetryPolicy struct {
	Attempts int           // total tries, the first included; 0 means 3
	Base     time.Duration // 0 means 200ms
	Max      time.Duration // cap on one wait; 0 means 2s

	Sleep func(ctx context.Context, d time.Duration) error // for tests; nil sleeps for real
	Rand  func() float64                                   // in [0,1); for tests; nil uses math/rand
}

func (p RetryPolicy) attempts() int {
	if p.Attempts <= 0 {
		return 3
	}
	return p.Attempts
}

// Wait returns how long to wait after the n-th failure (n starts at 0).
func (p RetryPolicy) Wait(n int) time.Duration {
	base, max := p.Base, p.Max
	if base <= 0 {
		base = 200 * time.Millisecond
	}
	if max <= 0 {
		max = 2 * time.Second
	}
	ceil := max
	if n < 30 && base<<uint(n) < max && base<<uint(n) > 0 { // the shift cannot overflow below the cap
		ceil = base << uint(n)
	}
	r := rand.Float64
	if p.Rand != nil {
		r = p.Rand
	}
	return time.Duration(r() * float64(ceil))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type retrying struct {
	next Client
	p    RetryPolicy
}

// WithRetry wraps c so that ErrUnavailable is retried under p. It keeps the whole Client contract:
// the context still ends the call promptly, and the pieces handed to onToken still join to the text.
func WithRetry(c Client, p RetryPolicy) Client { return &retrying{c, p} }

func (r *retrying) Chat(ctx context.Context, req Request, onToken func(string)) (Response, error) {
	sleep := r.p.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	started := false
	var tok func(string)
	if onToken != nil {
		tok = func(s string) { started = true; onToken(s) }
	}
	var err error
	for n := 0; n < r.p.attempts(); n++ {
		var resp Response
		resp, err = r.next.Chat(ctx, req, tok)
		if err == nil || !errors.Is(err, ErrUnavailable) || started || ctx.Err() != nil {
			return resp, err
		}
		if n+1 == r.p.attempts() {
			break
		}
		if serr := sleep(ctx, r.p.Wait(n)); serr != nil {
			return Response{}, serr
		}
	}
	return Response{}, err
}
