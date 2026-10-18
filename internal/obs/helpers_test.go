package obs

import (
	"io"
	"log/slog"
	"sync"
	"time"

	"offgrid/internal/drain"
	"offgrid/internal/jobqueue"
)

var t0 = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

// clock is a hand-wound clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newM() (*Metrics, *clock) {
	c := &clock{t: t0}
	m := NewMetrics()
	m.Now = c.Now
	return m, c
}

func ev(kind drain.EventKind, id int64, jobKind string, at time.Time) drain.Event {
	return drain.Event{Kind: kind, Job: jobqueue.Job{ID: id, Kind: jobKind, Attempts: 1, MaxAttempts: 5}, At: at}
}

func slogDiscard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
