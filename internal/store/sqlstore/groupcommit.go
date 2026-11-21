package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Group commit.
//
// Every Create, Update, AddNote... used to be its own transaction, and with synchronous=FULL each
// commit waits for the disk (an fsync): about 2,000 writes a second here however many callers
// there were, and far fewer on a slow disk. Now all writes go through one writer goroutine. It
// takes the requests that are waiting (and, with FlushWindow set, those that arrive within a short
// window), runs them one after the other inside ONE transaction, commits once, and only then tells
// each caller it is done. Many writers share one fsync; none is acknowledged before the data is
// on disk.
//
// What does not change:
//   - Durability: a caller gets nil only after the COMMIT that holds its write returned.
//   - Isolation between callers' failures: each request runs inside a SAVEPOINT. If its function
//     returns an error, only its own changes are rolled back and only it gets the error; the
//     others in the batch still commit. (A batch of one skips the savepoint and rolls back the
//     whole transaction, which is the same thing.)
//   - Atomicity of one request: a crash leaves each request fully in or fully out, and since a
//     batch is a single transaction, a whole batch is in or out.
//   - Order: requests run in the order they were queued.
//
// What does:
//   - A latency floor, only if FlushWindow is set: a lone writer waits up to the window for company.
//     The default is 0: no waiting; batches form on their own while the previous commit is on its
//     way to disk. Under no load a write costs the same as before plus one channel hand-off.
//   - Request functions must not call write themselves (the writer is busy running them: it would
//     wait for itself). None does; a nested write would hang, and the test suite would show it.
//   - A function that panics is recovered on the writer, its savepoint rolled back, and the panic
//     re-raised in the caller's goroutine, so the writer survives and the caller sees what it
//     saw before.
//   - Cancelling a caller's context before its request has been picked up returns ctx.Err() and
//     the request never runs. Once picked up it runs to completion and the caller gets the real result.

// ErrClosed is returned by writes to a store that has been closed.
var ErrClosed = errors.New("sqlstore: the store is closed")

const (
	defaultMaxBatch = 128
	queueDepth      = 1024
)

const (
	reqQueued int32 = iota
	reqClaimed
	reqCancelled
)

type writeResult struct {
	err      error
	panicked bool
	panicVal any
}

type writeReq struct {
	ctx   context.Context
	fn    func(tx execer) error
	state atomic.Int32
	done  chan writeResult
}

// CommitStats counts what the writer has done since the store opened.
type CommitStats struct {
	Batches   int64 // transactions committed (each is one fsync)
	Writes    int64 // requests acknowledged OK
	Failed    int64 // requests that returned an error (rolled back individually)
	MaxBatch  int64 // the most requests one commit carried
	Cancelled int64 // requests dropped because their context ended while queued
}

type committer struct {
	s        *Store
	window   time.Duration
	maxBatch int

	queue chan *writeReq

	startOnce sync.Once
	mu        sync.RWMutex // guards closed against enqueue
	closed    bool
	running   bool // the writer goroutine was started (guarded by mu)
	stopped   chan struct{}

	batches, writes, failed, maxSeen, cancelled atomic.Int64

	// beforeCommit is a test hook: it runs after every request of a batch has run and before COMMIT.
	// If it returns an error the transaction is rolled back and every request gets that error.
	// nil in production.
	beforeCommit func() error
}

func newCommitter(s *Store, window time.Duration, maxBatch int) *committer {
	if maxBatch <= 0 {
		maxBatch = defaultMaxBatch
	}
	if window < 0 {
		window = 0
	}
	return &committer{s: s, window: window, maxBatch: maxBatch, queue: make(chan *writeReq, queueDepth), stopped: make(chan struct{})}
}

// submit queues fn and waits for its result.
func (c *committer) submit(ctx context.Context, fn func(tx execer) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	req := &writeReq{ctx: ctx, fn: fn, done: make(chan writeResult, 1)}

	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return ErrClosed
	}
	c.startOnce.Do(func() { c.running = true; go c.run() })
	select {
	case c.queue <- req:
		c.mu.RUnlock()
	case <-ctx.Done():
		c.mu.RUnlock()
		return ctx.Err()
	}

	select {
	case res := <-req.done:
		return res.finish()
	case <-ctx.Done():
		if req.state.CompareAndSwap(reqQueued, reqCancelled) {
			c.cancelled.Add(1)
			return ctx.Err()
		}
		return (<-req.done).finish() // already running: it will finish; report what really happened
	}
}

func (r writeResult) finish() error {
	if r.panicked {
		panic(r.panicVal)
	}
	return r.err
}

// close stops accepting new writes (they get ErrClosed), lets everything already queued commit and
// be answered, and waits for the writer to exit.
func (c *committer) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.startOnce.Do(func() {}) // a writer that never started never will
	started := c.running
	close(c.queue)
	c.mu.Unlock()
	if started {
		<-c.stopped
	}
}

func (c *committer) run() {
	defer close(c.stopped)
	for first := range c.queue {
		batch := c.collect(first)
		c.commit(batch)
	}
}

// collect gathers a batch starting with first: everything already queued, up to maxBatch, and then,
// if a window is set, whatever arrives before the window ends.
func (c *committer) collect(first *writeReq) []*writeReq {
	batch := append(make([]*writeReq, 0, 16), first)
	for len(batch) < c.maxBatch {
		select {
		case r, ok := <-c.queue:
			if !ok {
				return batch
			}
			batch = append(batch, r)
			continue
		default:
		}
		break
	}
	if c.window <= 0 || len(batch) >= c.maxBatch {
		return batch
	}
	timer := time.NewTimer(c.window)
	defer timer.Stop()
	for len(batch) < c.maxBatch {
		select {
		case r, ok := <-c.queue:
			if !ok {
				return batch
			}
			batch = append(batch, r)
		case <-timer.C:
			return batch
		}
	}
	return batch
}

// commit runs a batch in one transaction and answers every request in it.
func (c *committer) commit(batch []*writeReq) {
	live := batch[:0:0]
	for _, r := range batch {
		if r.state.CompareAndSwap(reqQueued, reqClaimed) {
			live = append(live, r)
		}
	}
	if len(live) == 0 {
		return
	}
	// Begin on a context that outlives any one caller: the batch belongs to all of them.
	tx, err := c.s.db.BeginTx(context.Background(), nil)
	if err != nil {
		for _, r := range live {
			r.done <- writeResult{err: err}
		}
		c.failed.Add(int64(len(live)))
		return
	}
	cache := newStmtCache(tx, c.s.pool)

	results := make([]writeResult, len(live))
	ok := 0
	for i, r := range live {
		results[i] = c.runOne(r, cache, tx, len(live) > 1)
		if results[i].err == nil && !results[i].panicked {
			ok++
		}
	}

	if ok == 0 {
		tx.Rollback()
		c.s.pool.fill(context.Background())
		c.failed.Add(int64(len(live)))
		for i, r := range live {
			r.done <- results[i]
		}
		return
	}
	var cerr error
	if c.beforeCommit != nil {
		cerr = c.beforeCommit()
	}
	if cerr != nil {
		tx.Rollback()
	} else {
		cerr = tx.Commit()
	}
	if cerr != nil {
		cerr = fmt.Errorf("sqlstore: commit of %d writes: %w", ok, cerr)
		for i := range results {
			if results[i].err == nil && !results[i].panicked {
				results[i].err = cerr
			}
		}
		ok = 0
	}
	c.s.pool.fill(context.Background())
	failed := 0
	for i := range live {
		if results[i].err != nil || results[i].panicked {
			failed++
		}
	}
	c.batches.Add(1)
	c.writes.Add(int64(ok))
	c.failed.Add(int64(failed))
	for {
		cur := c.maxSeen.Load()
		if int64(len(live)) <= cur || c.maxSeen.CompareAndSwap(cur, int64(len(live))) {
			break
		}
	}
	// Only now, with the commit behind us and stats updated, is anyone told.
	for i, r := range live {
		r.done <- results[i]
	}
}

// runOne runs one request, inside a savepoint when it shares the transaction with others.
func (c *committer) runOne(r *writeReq, cache *stmtCache, tx *sql.Tx, shared bool) (res writeResult) {
	if shared {
		if _, err := tx.ExecContext(context.Background(), "SAVEPOINT w"); err != nil {
			return writeResult{err: err}
		}
	}
	defer func() {
		if p := recover(); p != nil {
			res = writeResult{panicked: true, panicVal: p}
		}
		if shared {
			if res.err != nil || res.panicked {
				if _, err := tx.ExecContext(context.Background(), "ROLLBACK TO w"); err != nil {
					res = writeResult{err: fmt.Errorf("sqlstore: undoing a failed write: %w", err)}
				}
			}
			if _, err := tx.ExecContext(context.Background(), "RELEASE w"); err != nil && res.err == nil && !res.panicked {
				res = writeResult{err: err}
			}
		}
	}()
	res.err = r.fn(cache)
	return res
}

// Stats returns the writer's counters.
func (s *Store) CommitStats() CommitStats {
	c := s.gc
	return CommitStats{Batches: c.batches.Load(), Writes: c.writes.Load(), Failed: c.failed.Load(), MaxBatch: c.maxSeen.Load(), Cancelled: c.cancelled.Load()}
}
