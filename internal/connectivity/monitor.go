package connectivity

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

// Event is published every time the state changes.
type Event struct {
	From, To State
	At       time.Time
	Reason   string
	Last     Result // the probe that tipped it
}

// Config tunes a Monitor. Zero values get defaults from withDefaults.
type Config struct {
	Prober       Prober
	Interval     time.Duration // time between probes (default 2s)
	ProbeTimeout time.Duration // a probe taking longer than this has failed (default 1500ms)
	SlowAfter    time.Duration // answered, but slower than this, counts as Slow (default 800ms)
	Thresholds   Thresholds    // default DefaultThresholds()
	Buffer       int           // size of the Events channel (default 16)

	// OnProbe, if set, sees every probe and its verdict (used by the CLI to
	// print a live line). It runs on the monitor goroutine, so keep it fast.
	OnProbe func(Result, Verdict, State)

	// Now and Wait exist so tests do not need real time. Wait blocks for d
	// and reports false if ctx ended first.
	Now  func() time.Time
	Wait func(ctx context.Context, d time.Duration) bool
}

func (c Config) withDefaults() Config {
	if c.Interval <= 0 {
		c.Interval = 2 * time.Second
	}
	if c.ProbeTimeout <= 0 {
		c.ProbeTimeout = 1500 * time.Millisecond
	}
	if c.SlowAfter <= 0 {
		c.SlowAfter = 800 * time.Millisecond
	}
	if c.Thresholds == (Thresholds{}) {
		c.Thresholds = DefaultThresholds()
	}
	if c.Buffer <= 0 {
		c.Buffer = 16
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Wait == nil {
		c.Wait = sleep
	}
	return c
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// Monitor probes the network on a timer and publishes state changes.
type Monitor struct {
	cfg     Config
	machine *Machine
	events  chan Event

	state   atomic.Int32 // a State, readable from any goroutine
	dropped atomic.Int64
	started atomic.Bool
}

// New validates the configuration and builds a Monitor. Nothing runs until Run.
func New(cfg Config) (*Monitor, error) {
	if cfg.Prober == nil {
		return nil, errors.New("connectivity: a Prober is required")
	}
	cfg = cfg.withDefaults()
	if cfg.SlowAfter >= cfg.ProbeTimeout {
		return nil, fmt.Errorf("connectivity: SlowAfter (%v) must be shorter than ProbeTimeout (%v), or no probe could ever be slow", cfg.SlowAfter, cfg.ProbeTimeout)
	}
	m, err := NewMachine(cfg.Thresholds)
	if err != nil {
		return nil, err
	}
	return &Monitor{cfg: cfg, machine: m, events: make(chan Event, cfg.Buffer)}, nil
}

// Events delivers state changes. It is closed when Run returns. Read it, or
// ignore it: a reader that falls behind loses the oldest events, never blocks
// the monitor (see publish).
func (m *Monitor) Events() <-chan Event { return m.events }

// State is the current state. Safe to call from any goroutine at any time, so
// code that only needs "are we online right now?" never has to track events.
func (m *Monitor) State() State { return State(m.state.Load()) }

// Dropped counts events discarded because the reader was too slow.
func (m *Monitor) Dropped() int64 { return m.dropped.Load() }

// Run probes until ctx is cancelled, then closes Events and returns ctx's
// error. It is meant to be started with `go`. It may be called only once.
//
// Shutdown is prompt: the wait between probes and the probe itself both watch
// ctx, so cancelling never has to wait out an interval or a slow request.
func (m *Monitor) Run(ctx context.Context) error {
	if !m.started.CompareAndSwap(false, true) {
		return errors.New("connectivity: Run called twice")
	}
	defer close(m.events)

	for {
		res := m.probeOnce(ctx)
		if ctx.Err() != nil {
			// Cancelled mid-probe: the failure is ours, not the network's, so
			// it must not count as evidence.
			return ctx.Err()
		}
		v := m.verdict(res)
		tr := m.machine.Observe(v)
		if tr != nil {
			m.state.Store(int32(tr.To))
		}
		if m.cfg.OnProbe != nil {
			m.cfg.OnProbe(res, v, m.machine.State())
		}
		if tr != nil {
			m.publish(Event{From: tr.From, To: tr.To, At: m.cfg.Now(), Reason: tr.Reason, Last: res})
		}
		if !m.cfg.Wait(ctx, m.cfg.Interval) {
			return ctx.Err()
		}
	}
}

func (m *Monitor) probeOnce(ctx context.Context) Result {
	pctx, cancel := context.WithTimeout(ctx, m.cfg.ProbeTimeout)
	defer cancel()
	return m.cfg.Prober.Probe(pctx)
}

func (m *Monitor) verdict(r Result) Verdict {
	switch {
	case !r.OK:
		return Fail
	case r.Latency > m.cfg.SlowAfter:
		return Slow
	default:
		return Good
	}
}

// publish never blocks. If the buffer is full the OLDEST event is discarded to
// make room for the newest, because a late reader cares most about where
// things stand now. The monitor is the only sender, so after making room one
// retry always succeeds.
func (m *Monitor) publish(e Event) {
	for {
		select {
		case m.events <- e:
			return
		default:
		}
		select {
		case <-m.events:
			m.dropped.Add(1)
		default:
		}
	}
}
