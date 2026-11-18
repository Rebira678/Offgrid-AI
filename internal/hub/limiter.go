package hub

import (
	"math"
	"sync"
	"time"
)

// Limiter is a token bucket: it holds up to Burst tokens, gains Rate per second, and each request spends one.
// It is the small part of golang.org/x/time/rate that the hub needs, written against an injected clock so
// its tests do not sleep, and so Offgrid's go.mod does not grow for it. (rate.Limiter is the drop-in if the
// hub ever needs reservations or contexts.)
type Limiter struct {
	mu     sync.Mutex
	rate   float64 // tokens per second
	burst  float64
	tokens float64
	last   time.Time
	now    func() time.Time
}

// NewLimiter starts full. rate <= 0 or burst < 1 is treated as "one request per second, burst one".
func NewLimiter(rate float64, burst int, now func() time.Time) *Limiter {
	if rate <= 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
		rate = 1
	}
	if burst < 1 {
		burst = 1
	}
	if now == nil {
		now = time.Now
	}
	return &Limiter{rate: rate, burst: float64(burst), tokens: float64(burst), last: now(), now: now}
}

// Allow spends a token if there is one. If not, retry says how long until there will be.
func (l *Limiter) Allow() (ok bool, retry time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.now()
	if dt := t.Sub(l.last); dt > 0 { // a clock that steps back gains nothing
		l.tokens = math.Min(l.burst, l.tokens+dt.Seconds()*l.rate)
	}
	l.last = t
	if l.tokens >= 1 {
		l.tokens--
		return true, 0
	}
	waitSec := (1 - l.tokens) / l.rate
	if waitSec <= 0 {
		return false, time.Nanosecond
	}
	if waitSec > float64(math.MaxInt64/int64(time.Second)) {
		return false, time.Duration(math.MaxInt64)
	}
	return false, time.Duration(waitSec * float64(time.Second))
}
