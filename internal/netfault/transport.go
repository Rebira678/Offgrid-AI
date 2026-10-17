// Package netfault breaks the network on purpose, and the same way every time.
//
// Transport wraps an http.RoundTripper and can add latency, lose requests and
// simulate a full partition. Because every HTTP client in Offgrid (the
// connectivity probes today, the sync client later) is built on a
// RoundTripper, one wrapper makes all of them testable against a bad network
// without touching the real one.
//
// Three kinds of fault, each modelled on something real:
//
//	latency    a slow link: every request that gets through is delayed
//	drop       packet loss: the request vanishes and the caller waits
//	partition  the network is gone, in one of two ways (see PartitionMode)
//
// Nothing here is random unless you ask for it, and the randomness is seeded:
// the same Seed and the same request order give the same drops.
package netfault

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Sentinel errors. Use errors.Is to tell injected faults from real ones.
var (
	ErrPartitioned = errors.New("network partitioned")
	ErrDropped     = errors.New("request dropped")
)

// faultError marks an error as injected, and satisfies net.Error so code that
// asks "was that a timeout?" gets a sensible answer.
type faultError struct {
	kind    error
	detail  string
	timeout bool
}

func (e *faultError) Error() string   { return fmt.Sprintf("%s (injected: %s)", e.detail, e.kind) }
func (e *faultError) Unwrap() error   { return e.kind }
func (e *faultError) Timeout() bool   { return e.timeout }
func (e *faultError) Temporary() bool { return true }

// PartitionMode says what a partition looks like to the caller.
type PartitionMode int

const (
	// Refuse fails immediately, like WiFi switched off or a cable pulled:
	// "network is unreachable". This is the default.
	Refuse PartitionMode = iota
	// Blackhole swallows the request and never answers, like an upstream
	// router that died while the local link is still up. The caller finds out
	// only when its own timeout fires. This is the nasty one: code with no
	// timeout hangs forever.
	Blackhole
)

func (m PartitionMode) String() string {
	if m == Blackhole {
		return "blackhole"
	}
	return "refuse"
}

// ParseMode reads "refuse" or "blackhole".
func ParseMode(s string) (PartitionMode, error) {
	switch s {
	case "refuse", "":
		return Refuse, nil
	case "blackhole":
		return Blackhole, nil
	}
	return 0, fmt.Errorf("partition mode must be refuse or blackhole (got %q)", s)
}

// Config describes the faults. The zero value injects nothing.
type Config struct {
	Base http.RoundTripper // the real transport (default http.DefaultTransport)

	Latency time.Duration // added to every request that gets through
	Jitter  time.Duration // plus a random 0..Jitter on top

	DropPercent float64 // 0..100: chance a request is lost

	// Partitioned is asked once per request. nil means "never partitioned".
	// An error means the control source is broken; the request fails loudly
	// rather than silently pretending the network is fine.
	Partitioned func() (bool, error)
	Mode        PartitionMode

	// Hang is how long a lost or blackholed request holds on before giving up
	// when the caller set no deadline of its own. 0 means wait for the
	// caller's context forever. Callers should always have a deadline; this is
	// a safety net so a demo never freezes.
	Hang time.Duration

	Seed int64 // drives drops and jitter; same seed, same sequence

	// Sleep waits for d or until ctx ends. Tests inject a fake; the default
	// uses a real timer.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Stats counts what the transport did. Safe to read while it runs.
type Stats struct {
	Requests    int64 // every request seen
	Passed      int64 // reached the real transport
	Partitioned int64 // refused or blackholed by a partition
	Dropped     int64 // lost to DropPercent
}

// Transport is the fault-injecting http.RoundTripper.
type Transport struct {
	cfg Config

	mu  sync.Mutex // *rand.Rand is not safe for concurrent use
	rnd *rand.Rand

	requests, passed, partitioned, dropped atomic.Int64
}

// New validates cfg and builds a Transport.
func New(cfg Config) (*Transport, error) {
	if cfg.Latency < 0 || cfg.Jitter < 0 || cfg.Hang < 0 {
		return nil, errors.New("latency, jitter and hang must not be negative")
	}
	if cfg.DropPercent < 0 || cfg.DropPercent > 100 {
		return nil, fmt.Errorf("drop percentage must be between 0 and 100 (got %v)", cfg.DropPercent)
	}
	if cfg.Base == nil {
		cfg.Base = http.DefaultTransport
	}
	if cfg.Sleep == nil {
		cfg.Sleep = realSleep
	}
	return &Transport{cfg: cfg, rnd: rand.New(rand.NewSource(cfg.Seed))}, nil
}

func realSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stats returns the counters so far.
func (t *Transport) Stats() Stats {
	return Stats{
		Requests:    t.requests.Load(),
		Passed:      t.passed.Load(),
		Partitioned: t.partitioned.Load(),
		Dropped:     t.dropped.Load(),
	}
}

// rolls draws the two random numbers every request uses, always both and
// always in the same order. That is what keeps runs comparable: turning the
// latency up does not change WHICH requests are dropped, because the drop
// roll is drawn whether or not it is used.
func (t *Transport) rolls() (drop, jitter float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.rnd.Float64(), t.rnd.Float64()
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.requests.Add(1)
	ctx := req.Context()
	// A RoundTripper that does not send the request must still close its body.
	giveUp := func(err error) (*http.Response, error) {
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return giveUp(err)
	}

	dropRoll, jitterRoll := t.rolls()

	if t.cfg.Partitioned != nil {
		part, err := t.cfg.Partitioned()
		if err != nil {
			return giveUp(fmt.Errorf("netfault: fault control: %w", err))
		}
		if part {
			t.partitioned.Add(1)
			if t.cfg.Mode == Blackhole {
				return giveUp(t.hold(ctx, ErrPartitioned, "no answer from the network"))
			}
			return giveUp(&faultError{kind: ErrPartitioned, detail: "dial: network is unreachable"})
		}
	}

	if t.cfg.DropPercent > 0 && dropRoll*100 < t.cfg.DropPercent {
		t.dropped.Add(1)
		return giveUp(t.hold(ctx, ErrDropped, "no answer from the network"))
	}

	if d := t.cfg.Latency + time.Duration(jitterRoll*float64(t.cfg.Jitter)); d > 0 {
		if err := t.cfg.Sleep(ctx, d); err != nil {
			return giveUp(err)
		}
	}

	t.passed.Add(1)
	return t.cfg.Base.RoundTrip(req)
}

// hold models a request that disappears: the caller waits until its own
// deadline (returned as the context's error, exactly as a real stalled
// connection would end) or, failing that, until Hang runs out.
func (t *Transport) hold(ctx context.Context, kind error, detail string) error {
	if t.cfg.Hang <= 0 {
		<-ctx.Done()
		return ctx.Err()
	}
	if err := t.cfg.Sleep(ctx, t.cfg.Hang); err != nil {
		return err
	}
	return &faultError{kind: kind, detail: detail, timeout: true}
}
