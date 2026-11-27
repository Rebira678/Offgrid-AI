package governor

import (
	"context"
	"sync/atomic"
	"time"
)

// limiter lets at most `slots` holders in at once, lets at most `queue` more wait, and makes a waiter
// give up after `wait`. Everything beyond that is refused at once with a *Refusal: a node that is
// asked for more than it can do says so in a moment instead of piling up goroutines and memory.
type limiter struct {
	slots   chan struct{}
	waiting atomic.Int64
	queue   int64
	wait    time.Duration
	what    string
}

func newLimiter(what string, slots, queue int, wait time.Duration) *limiter {
	if slots < 0 {
		slots = 0
	}
	return &limiter{slots: make(chan struct{}, slots), queue: int64(queue), wait: wait, what: what}
}

// acquire returns the function that gives the slot back. It is safe to call that function once only.
func (l *limiter) acquire(ctx context.Context) (func(), error) {
	if cap(l.slots) == 0 {
		return nil, &Refusal{Kind: KindNoSlots, Reason: l.what + " calls are not allowed on this node"}
	}
	release := func() { <-l.slots }
	select { // the common case: a free slot, no waiting
	case l.slots <- struct{}{}:
		return release, nil
	default:
	}
	if l.waiting.Add(1) > l.queue {
		l.waiting.Add(-1)
		return nil, &Refusal{Kind: KindBusy, Reason: l.what + " model is busy and its queue is full"}
	}
	defer l.waiting.Add(-1)
	t := time.NewTimer(l.wait)
	defer t.Stop()
	select {
	case l.slots <- struct{}{}:
		return release, nil
	case <-t.C:
		return nil, &Refusal{Kind: KindBusy, Reason: l.what + " model is busy: no slot freed up within " + l.wait.String()}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
