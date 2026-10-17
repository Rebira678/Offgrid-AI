// Package drain empties the job queue when it is possible to.
//
// Days 9 and 10 put work into a durable queue. This package takes it out
// again, safely:
//
//   - at most Concurrency jobs run at once, started in queue order;
//   - jobs that need the network are not even claimed while the network is
//     down (a claim counts as an attempt, so "claim it and put it back" would
//     burn attempts), while jobs that only need this laptop keep running;
//   - a running job keeps its lease alive (heartbeat), and gives up quietly if
//     it finds the lease was taken away;
//   - stopping, or losing the network mid-job, RELEASES the job instead of
//     failing it: no attempt is used up and the job keeps its place in line;
//   - the result is stored in the same transaction that marks the job done,
//     so a job that ran twice (crash after the work, before the report) still
//     has exactly one result;
//   - if the whole process is killed (kill -9), nothing is lost: the leases
//     expire and the next worker picks the jobs up.
//
// What is NOT promised: exactly-once execution. After a crash a handler can
// run again for the same job. Handlers must be safe to repeat, or must use
// the checkpoint to refuse to repeat (see Interpret for the second pattern).
package drain

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"offgrid/internal/jobqueue"
)

// Progress lets a handler save how far it got. The text comes back in
// Job.Progress if the job is ever run again.
type Progress interface {
	Save(ctx context.Context, data string) error
}

// Handler does the work for one job and returns the result text.
//
// ctx is cancelled when the worker stops, when a job that needs the network
// loses it, or when the job's lease is lost: a handler must return promptly
// when that happens. Return Permanent(err) for an error that retrying cannot
// fix; any other error is retried with backoff until the job runs out of
// attempts.
type Handler func(ctx context.Context, job jobqueue.Job, p Progress) (string, error)

// Spec is how the worker runs one kind of job.
type Spec struct {
	Handle       Handler
	NeedsNetwork bool // only run while the network is up
}

// ErrPermanent marks a failure that retrying cannot fix.
var ErrPermanent = errors.New("permanent failure")

type permanent struct{ err error }

func (p permanent) Error() string        { return p.err.Error() }
func (p permanent) Unwrap() error        { return p.err }
func (p permanent) Is(target error) bool { return target == ErrPermanent }

// Permanent wraps err so the worker sends the job straight to the dead-letter
// state instead of retrying. The message stays exactly err's message.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanent{err}
}

// Internal reasons for cancelling a job's context.
var (
	errLostClaim   = errors.New("drain: the job's lease was taken over")
	errWentOffline = errors.New("drain: the network went away")
)

// EventKind says what happened.
type EventKind string

const (
	EvClaimed   EventKind = "claimed"
	EvDone      EventKind = "done"
	EvRetry     EventKind = "retry"      // failed, will be tried again after Delay
	EvDead      EventKind = "dead"       // failed for good
	EvReleased  EventKind = "released"   // stopped through no fault of the job; attempt handed back
	EvLostClaim EventKind = "lost-claim" // someone else owns the job now; nothing was reported
	EvGate      EventKind = "gate"       // network jobs paused or resumed
	EvError     EventKind = "error"      // the queue itself misbehaved (database busy, etc.)
)

// Event is one thing the worker did, for logs and the screen.
type Event struct {
	Kind  EventKind
	Job   jobqueue.Job // zero for EvGate and EvError
	Err   error
	Delay time.Duration // EvRetry
	Note  string
	At    time.Time
}

// Summary counts what the worker did since it started.
type Summary struct {
	Claimed, Done, Retried, Dead, Released, LostClaims int
}

// Config configures a Worker. Zero values get defaults.
type Config struct {
	Queue *jobqueue.Queue
	Specs map[string]Spec // by job kind; kinds with no Spec are never claimed

	Concurrency int           // jobs at once; default 2
	Lease       time.Duration // how long a claim lasts without a heartbeat; default 2m
	Poll        time.Duration // how often to look for work and check the network; default 1s
	JobTimeout  time.Duration // one attempt may not run longer than this; default 10m

	// Online says whether the network is up. nil means always up.
	Online func() bool

	// ExitWhenIdle makes Run return once nothing is ready and nothing is
	// running anywhere, instead of waiting for more work. Jobs leased by
	// another (or a dead) worker are waited for until their lease ends; jobs
	// waiting out a backoff delay stay queued.
	ExitWhenIdle bool

	// OnEvent is called for every Event, one at a time (the worker holds a
	// lock around it), so it needs no locking of its own, and must not call
	// back into the Worker.
	OnEvent func(Event)

	Now func() time.Time // default time.Now; only used to stamp events
}

// Worker drains the queue.
type Worker struct {
	cfg   Config
	kinds []string // sorted, for stable behaviour

	slots    chan struct{}
	inflight atomic.Int64
	wake     chan struct{}

	mu      sync.Mutex // guards sum and serializes OnEvent
	sum     Summary
	gateSet bool
	gateUp  bool
}

// New checks the configuration and builds a Worker.
func New(cfg Config) (*Worker, error) {
	if cfg.Queue == nil {
		return nil, errors.New("drain: no queue")
	}
	if len(cfg.Specs) == 0 {
		return nil, errors.New("drain: no job kinds to run")
	}
	for k, s := range cfg.Specs {
		if s.Handle == nil {
			return nil, fmt.Errorf("drain: kind %q has no handler", k)
		}
	}
	if cfg.Concurrency < 0 || cfg.Lease < 0 || cfg.Poll < 0 || cfg.JobTimeout < 0 {
		return nil, errors.New("drain: concurrency, lease, poll and timeout must not be negative")
	}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = 2
	}
	if cfg.Lease == 0 {
		cfg.Lease = 2 * time.Minute
	}
	if cfg.Poll == 0 {
		cfg.Poll = time.Second
	}
	if cfg.JobTimeout == 0 {
		cfg.JobTimeout = 10 * time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	w := &Worker{cfg: cfg, slots: make(chan struct{}, cfg.Concurrency), wake: make(chan struct{}, 1)}
	for k := range cfg.Specs {
		w.kinds = append(w.kinds, k)
	}
	sort.Strings(w.kinds)
	return w, nil
}

// Summary returns the counters so far.
func (w *Worker) Summary() Summary {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sum
}

func (w *Worker) emit(e Event) {
	e.At = w.cfg.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	switch e.Kind {
	case EvClaimed:
		w.sum.Claimed++
	case EvDone:
		w.sum.Done++
	case EvRetry:
		w.sum.Retried++
	case EvDead:
		w.sum.Dead++
	case EvReleased:
		w.sum.Released++
	case EvLostClaim:
		w.sum.LostClaims++
	}
	if w.cfg.OnEvent != nil {
		w.cfg.OnEvent(e)
	}
}

func (w *Worker) online() bool { return w.cfg.Online == nil || w.cfg.Online() }

// claimable returns the kinds that may be claimed right now, and tells the
// listener when network jobs get paused or resumed.
func (w *Worker) claimable() []string {
	up := w.online()
	w.noteGate(up)
	if up {
		return w.kinds
	}
	var out []string
	for _, k := range w.kinds {
		if !w.cfg.Specs[k].NeedsNetwork {
			out = append(out, k)
		}
	}
	return out
}

func (w *Worker) noteGate(up bool) {
	needs := false
	for _, s := range w.cfg.Specs {
		needs = needs || s.NeedsNetwork
	}
	if !needs {
		return
	}
	w.mu.Lock()
	changed := !w.gateSet || w.gateUp != up
	w.gateSet, w.gateUp = true, up
	w.mu.Unlock()
	if !changed {
		return
	}
	if up {
		w.emit(Event{Kind: EvGate, Note: "network is up: jobs that need it will run"})
	} else {
		w.emit(Event{Kind: EvGate, Note: "network is down: jobs that need it wait; jobs that only need this laptop still run"})
	}
}

// Run drains the queue until ctx is cancelled (or, with ExitWhenIdle, until
// there is nothing left to do now). It waits for running jobs to wind down
// before returning, and always returns nil: what went wrong is in the events.
func (w *Worker) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	defer wg.Wait()

	for ctx.Err() == nil {
		// Wait for a free slot BEFORE looking at the network and claiming, so
		// the decision is made as late as possible.
		select {
		case w.slots <- struct{}{}:
		case <-ctx.Done():
			return nil
		}
		kinds := w.claimable()
		job, ok, err := w.cfg.Queue.ClaimKinds(ctx, w.cfg.Lease, kinds)
		if err != nil && ctx.Err() == nil {
			w.emit(Event{Kind: EvError, Err: err})
		}
		if !ok {
			<-w.slots
			if err == nil && w.cfg.ExitWhenIdle && w.inflight.Load() == 0 && !w.othersRunning(ctx) {
				return nil
			}
			select {
			case <-time.After(w.cfg.Poll):
			case <-w.wake:
			case <-ctx.Done():
			}
			continue
		}
		w.inflight.Add(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				w.inflight.Add(-1)
				<-w.slots
				select {
				case w.wake <- struct{}{}:
				default:
				}
			}()
			w.run(ctx, job)
		}()
	}
	return nil
}

// othersRunning reports whether the queue still has jobs marked running that
// this worker is not running. They belong to another worker, or to one that
// died: either way their lease will end, and then they become claimable. A
// worker that exits now would leave exactly the jobs a crash recovery exists
// to finish. (If the other worker is alive and busy, this waits for it; run
// without ExitWhenIdle if that is not what you want.)
func (w *Worker) othersRunning(ctx context.Context) bool {
	st, err := w.cfg.Queue.Stats(ctx)
	return err != nil || st.Running > 0
}

// progress implements Progress for one claim.
type progress struct {
	q       *jobqueue.Queue
	id      int64
	attempt int
}

func (p progress) Save(ctx context.Context, data string) error {
	return p.q.Checkpoint(ctx, p.id, p.attempt, data)
}

// run executes one claimed job and reports what happened to it.
func (w *Worker) run(parent context.Context, job jobqueue.Job) {
	spec := w.cfg.Specs[job.Kind]
	w.emit(Event{Kind: EvClaimed, Job: job})

	jctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	tctx, tcancel := context.WithTimeout(jctx, w.cfg.JobTimeout)
	defer tcancel()

	// Reports must reach the queue even while shutting down, so they use a
	// context that keeps the values but not the cancellation.
	report := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
	}

	// Heartbeat and network watch.
	stopWatch := make(chan struct{})
	var watch sync.WaitGroup
	watch.Add(1)
	go func() {
		defer watch.Done()
		hb := time.NewTicker(max(w.cfg.Lease/3, time.Millisecond))
		gate := time.NewTicker(max(min(w.cfg.Poll, w.cfg.Lease/3), time.Millisecond))
		defer hb.Stop()
		defer gate.Stop()
		for {
			select {
			case <-stopWatch:
				return
			case <-jctx.Done():
				return
			case <-hb.C:
				rctx, rc := report()
				err := w.cfg.Queue.Extend(rctx, job.ID, job.Attempts, w.cfg.Lease)
				rc()
				if errors.Is(err, jobqueue.ErrStaleClaim) {
					cancel(errLostClaim)
					return
				}
				if err != nil {
					w.emit(Event{Kind: EvError, Job: job, Err: fmt.Errorf("renewing the lease: %w", err)})
				}
			case <-gate.C:
				if spec.NeedsNetwork && !w.online() {
					cancel(errWentOffline)
					return
				}
			}
		}
	}()
	defer func() { close(stopWatch); watch.Wait() }()

	// The network may have dropped between the claim and now.
	if spec.NeedsNetwork && !w.online() {
		cancel(errWentOffline)
	}

	var result string
	var herr error
	if jctx.Err() == nil {
		result, herr = w.call(tctx, spec.Handle, job, progress{w.cfg.Queue, job.ID, job.Attempts})
	} else {
		herr = context.Cause(jctx)
	}

	rctx, rc := report()
	defer rc()

	if herr == nil {
		switch err := w.cfg.Queue.CompleteWith(rctx, job.ID, job.Attempts, result); {
		case err == nil:
			w.emit(Event{Kind: EvDone, Job: job, Note: result})
		case errors.Is(err, jobqueue.ErrStaleClaim):
			w.emit(Event{Kind: EvLostClaim, Job: job, Note: "finished, but the job had been handed to another worker; this result was discarded"})
		default:
			w.emit(Event{Kind: EvError, Job: job, Err: fmt.Errorf("recording the result: %w", err)})
		}
		return
	}

	cause := context.Cause(jctx)
	switch {
	case errors.Is(cause, errLostClaim):
		w.emit(Event{Kind: EvLostClaim, Job: job, Note: "the job was handed to another worker; stopped without reporting"})
	case errors.Is(cause, errWentOffline) || parent.Err() != nil:
		note := "the network went away"
		if parent.Err() != nil {
			note = "the worker is stopping"
		}
		if err := w.cfg.Queue.Release(rctx, job.ID, job.Attempts); err != nil && !errors.Is(err, jobqueue.ErrStaleClaim) {
			w.emit(Event{Kind: EvError, Job: job, Err: fmt.Errorf("releasing the job: %w", err)})
			return
		}
		w.emit(Event{Kind: EvReleased, Job: job, Note: note + "; the job keeps its place and no attempt was used"})
	default:
		res, err := w.cfg.Queue.Fail(rctx, job.ID, job.Attempts, herr, errors.Is(herr, ErrPermanent))
		switch {
		case errors.Is(err, jobqueue.ErrStaleClaim):
			w.emit(Event{Kind: EvLostClaim, Job: job, Err: herr, Note: "failed, but the job had been handed to another worker"})
		case err != nil:
			w.emit(Event{Kind: EvError, Job: job, Err: fmt.Errorf("reporting a failure: %w", err)})
		case res.Status == jobqueue.Dead:
			w.emit(Event{Kind: EvDead, Job: job, Err: herr})
		default:
			w.emit(Event{Kind: EvRetry, Job: job, Err: herr, Delay: res.Delay})
		}
	}
}

// call runs a handler, turning a panic into an error. One bad job must not
// take the whole worker (and every other job in flight) down with it.
func (w *Worker) call(ctx context.Context, h Handler, job jobqueue.Job, p Progress) (res string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("the handler panicked: %v\n%s", r, debug.Stack())
			if len(err.Error()) > 600 {
				err = fmt.Errorf("the handler panicked: %v", r)
			}
		}
	}()
	return h(ctx, job, p)
}
