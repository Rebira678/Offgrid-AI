package drain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"offgrid/internal/jobqueue"
	"offgrid/internal/store/sqlstore"
)

var bg = context.Background()

// env is a real queue in a temp file (not a fake: the fencing and
// transactions are what is being tested) plus an event log.
type env struct {
	t       *testing.T
	q       *jobqueue.Queue
	rawExec func(query string, args ...any)
	rawInt  func(query string, args ...any) int64
	path    string

	mu     sync.Mutex
	events []Event
}

func newEnv(t *testing.T) *env {
	t.Helper()
	path := filepath.Join(t.TempDir(), "queue.db")
	return openEnv(t, path)
}

func openEnv(t *testing.T, path string) *env {
	t.Helper()
	db, err := sqlstore.OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	q, err := jobqueue.New(bg, db, jobqueue.Config{
		Backoff: jobqueue.Backoff{Base: 20 * time.Millisecond, Max: 100 * time.Millisecond, Jitter: jobqueue.NoJitter},
	})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, q: q, path: path}
	e.rawExec = func(query string, args ...any) {
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	e.rawInt = func(query string, args ...any) int64 {
		var n int64
		if err := db.QueryRow(query, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	return e
}

func (e *env) add(key, kind string) jobqueue.Job {
	return e.addWith(key, kind, jobqueue.EnqueueOptions{})
}

func (e *env) addWith(key, kind string, o jobqueue.EnqueueOptions) jobqueue.Job {
	e.t.Helper()
	j, _, err := e.q.Enqueue(bg, key, kind, json.RawMessage(`{"text":"hello"}`), o)
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

func (e *env) get(id int64) jobqueue.Job {
	e.t.Helper()
	j, err := e.q.Get(bg, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

func (e *env) record(ev Event) {
	e.mu.Lock()
	e.events = append(e.events, ev)
	e.mu.Unlock()
}

func (e *env) count(k EventKind) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, ev := range e.events {
		if ev.Kind == k {
			n++
		}
	}
	return n
}

func (e *env) has(k EventKind, noteContains string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ev := range e.events {
		if ev.Kind == k && strings.Contains(ev.Note, noteContains) {
			return true
		}
	}
	return false
}

// start runs a worker in the background and returns a stop function that
// cancels it and waits for Run to return.
func (e *env) start(cfg Config) (w *Worker, stop func()) {
	e.t.Helper()
	cfg.Queue = e.q
	cfg.OnEvent = e.record
	if cfg.Poll == 0 {
		cfg.Poll = 10 * time.Millisecond
	}
	w, err := New(cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				e.t.Error("Run did not return after cancel")
			}
		})
	}
	e.t.Cleanup(stop)
	return w, stop
}

func eventually(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func okHandler(result string) Spec {
	return Spec{Handle: func(ctx context.Context, j jobqueue.Job, p Progress) (string, error) { return result, nil }}
}

func (e *env) result(id int64) (jobqueue.Result, bool) {
	e.t.Helper()
	r, found, err := e.q.Result(bg, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return r, found
}

func allDone(e *env, ids ...int64) func() bool {
	return func() bool {
		for _, id := range ids {
			if e.get(id).Status != jobqueue.Done {
				return false
			}
		}
		return true
	}
}

// ---- the basics --------------------------------------------------------------------

func TestDrainsEverythingInOrderWithConcurrencyOne(t *testing.T) {
	e := newEnv(t)
	var ids []int64
	for i := 0; i < 20; i++ {
		ids = append(ids, e.add(fmt.Sprint("k", i), "work").ID)
	}
	var mu sync.Mutex
	var order []int64
	e.start(Config{Concurrency: 1, Specs: map[string]Spec{"work": {Handle: func(ctx context.Context, j jobqueue.Job, p Progress) (string, error) {
		mu.Lock()
		order = append(order, j.ID)
		mu.Unlock()
		return fmt.Sprint("result of ", j.ID), nil
	}}}})
	eventually(t, "all jobs done", 5*time.Second, allDone(e, ids...))

	mu.Lock()
	defer mu.Unlock()
	for i, id := range order {
		if id != ids[i] {
			t.Fatalf("job %d ran in position %d, want %d: the queue's order was not kept (%v)", id, i, ids[i], order)
		}
	}
	for _, id := range ids {
		if r, ok := e.result(id); !ok || r.Text != fmt.Sprint("result of ", id) {
			t.Errorf("job %d result = %+v ok=%v", id, r, ok)
		}
	}
}

func TestConcurrencyLimitIsRespectedAndUsed(t *testing.T) {
	e := newEnv(t)
	var ids []int64
	for i := 0; i < 15; i++ {
		ids = append(ids, e.add(fmt.Sprint("k", i), "work").ID)
	}
	var cur, peak atomic.Int64
	e.start(Config{Concurrency: 3, Specs: map[string]Spec{"work": {Handle: func(ctx context.Context, j jobqueue.Job, p Progress) (string, error) {
		n := cur.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
		cur.Add(-1)
		return "ok", nil
	}}}})
	eventually(t, "all jobs done", 10*time.Second, allDone(e, ids...))
	if peak.Load() != 3 {
		t.Errorf("peak concurrency = %d, want exactly 3 (never above the limit, and the limit is used)", peak.Load())
	}
}

func TestDefaultConcurrencyIsTwo(t *testing.T) {
	e := newEnv(t)
	var ids []int64
	for i := 0; i < 8; i++ {
		ids = append(ids, e.add(fmt.Sprint("k", i), "work").ID)
	}
	var cur, peak atomic.Int64
	e.start(Config{Specs: map[string]Spec{"work": {Handle: func(ctx context.Context, j jobqueue.Job, p Progress) (string, error) {
		n := cur.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		cur.Add(-1)
		return "ok", nil
	}}}})
	eventually(t, "done", 10*time.Second, allDone(e, ids...))
	if peak.Load() != 2 {
		t.Errorf("peak = %d, want 2", peak.Load())
	}
}

func TestSummaryAndEvents(t *testing.T) {
	e := newEnv(t)
	a := e.add("a", "work")
	w, _ := e.start(Config{Concurrency: 1, Specs: map[string]Spec{"work": okHandler("fine")}})
	eventually(t, "done", 5*time.Second, allDone(e, a.ID))
	eventually(t, "summary", time.Second, func() bool { return w.Summary().Done == 1 })
	if s := w.Summary(); s.Claimed != 1 || s.Retried != 0 || s.Dead != 0 || s.Released != 0 {
		t.Errorf("summary %+v", s)
	}
	if !e.has(EvDone, "fine") {
		t.Error("no done event carrying the result")
	}
}

func TestEventsAreDeliveredOneAtATime(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 30; i++ {
		e.add(fmt.Sprint("k", i), "work")
	}
	plain := 0 // deliberately not atomic: -race fails this test if events overlap
	w, err := New(Config{Queue: e.q, Concurrency: 6, Poll: 5 * time.Millisecond, ExitWhenIdle: true,
		Specs:   map[string]Spec{"work": okHandler("x")},
		OnEvent: func(Event) { plain++ }})
	if err != nil {
		t.Fatal(err)
	}
	w.Run(bg)
	if plain < 60 {
		t.Errorf("only %d events for 30 jobs", plain)
	}
}

// ---- the network gate ---------------------------------------------------------------

func TestOfflineNetworkJobsWaitAndLocalJobsRun(t *testing.T) {
	e := newEnv(t)
	net1 := e.add("n1", "net")
	loc := e.add("l1", "local")
	var online atomic.Bool
	e.start(Config{Concurrency: 2, Online: online.Load, Specs: map[string]Spec{
		"net":   {NeedsNetwork: true, Handle: okHandler("from the network").Handle},
		"local": okHandler("from the laptop"),
	}})
	eventually(t, "local job done while offline", 3*time.Second, allDone(e, loc.ID))
	time.Sleep(150 * time.Millisecond) // many polls: still must not be touched
	if got := e.get(net1.ID); got.Status != jobqueue.Queued || got.Attempts != 0 {
		t.Fatalf("the network job was touched while offline: %+v", got)
	}
	if !e.has(EvGate, "network is down") {
		t.Error("no event saying network jobs are waiting")
	}

	online.Store(true) // the reconnect
	eventually(t, "network job drained after reconnect", 3*time.Second, allDone(e, net1.ID))
	if r, _ := e.result(net1.ID); r.Text != "from the network" {
		t.Errorf("result %+v", r)
	}
	if !e.has(EvGate, "network is up") {
		t.Error("no event saying the network is back")
	}
	if got := e.get(net1.ID); got.Attempts != 1 {
		t.Errorf("attempts = %d: waiting offline must not use attempts", got.Attempts)
	}
}

func TestDrainsInOrderOnReconnect(t *testing.T) {
	e := newEnv(t)
	var ids []int64
	for i := 0; i < 10; i++ {
		ids = append(ids, e.add(fmt.Sprint("k", i), "net").ID)
	}
	var online atomic.Bool
	var mu sync.Mutex
	var order []int64
	e.start(Config{Concurrency: 1, Online: online.Load, Specs: map[string]Spec{"net": {NeedsNetwork: true,
		Handle: func(ctx context.Context, j jobqueue.Job, p Progress) (string, error) {
			mu.Lock()
			order = append(order, j.ID)
			mu.Unlock()
			return "ok", nil
		}}}})
	time.Sleep(80 * time.Millisecond)
	mu.Lock()
	if len(order) != 0 {
		t.Fatalf("ran %v while offline", order)
	}
	mu.Unlock()
	online.Store(true)
	eventually(t, "drained", 5*time.Second, allDone(e, ids...))
	mu.Lock()
	defer mu.Unlock()
	for i := range ids {
		if order[i] != ids[i] {
			t.Fatalf("order after reconnect %v, want %v", order, ids)
		}
	}
}

func TestLosingTheNetworkMidJobReleasesItWithoutUsingAnAttempt(t *testing.T) {
	e := newEnv(t)
	j := e.add("a", "net")
	var online atomic.Bool
	online.Store(true)
	started := make(chan struct{}, 4)
	var runs atomic.Int64
	e.start(Config{Concurrency: 1, Online: online.Load, Specs: map[string]Spec{"net": {NeedsNetwork: true,
		Handle: func(ctx context.Context, jb jobqueue.Job, p Progress) (string, error) {
			if runs.Add(1) == 1 {
				started <- struct{}{}
				<-ctx.Done() // a long network call, interrupted by the partition
				return "", ctx.Err()
			}
			return "second time works", nil
		}}}})
	<-started
	online.Store(false) // the partition
	eventually(t, "released", 3*time.Second, func() bool { return e.count(EvReleased) == 1 })
	got := e.get(j.ID)
	if got.Status != jobqueue.Queued || got.Attempts != 0 || got.LastError != "" {
		t.Errorf("after the network dropped: %+v (want queued, attempts 0, no error)", got)
	}
	if e.count(EvRetry) != 0 || e.count(EvDead) != 0 {
		t.Error("a lost network was reported as a failure")
	}
	online.Store(true)
	eventually(t, "done after reconnect", 3*time.Second, allDone(e, j.ID))
	if r, _ := e.result(j.ID); r.Text != "second time works" || r.Attempt != 1 {
		t.Errorf("result %+v", r)
	}
}

func TestLocalJobsKeepRunningWhenTheNetworkDrops(t *testing.T) {
	e := newEnv(t)
	j := e.add("a", "local")
	var online atomic.Bool
	online.Store(true)
	started := make(chan struct{})
	finish := make(chan struct{})
	e.start(Config{Online: online.Load, Specs: map[string]Spec{"local": {
		Handle: func(ctx context.Context, jb jobqueue.Job, p Progress) (string, error) {
			close(started)
			select {
			case <-finish:
				return "done anyway", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}}}})
	<-started
	online.Store(false)
	time.Sleep(80 * time.Millisecond)
	close(finish)
	eventually(t, "done", 3*time.Second, allDone(e, j.ID))
	if e.count(EvReleased) != 0 {
		t.Error("a job that does not need the network was interrupted by its loss")
	}
}

// ---- stopping --------------------------------------------------------------------------

func TestShutdownReleasesRunningJobs(t *testing.T) {
	e := newEnv(t)
	a, b := e.add("a", "work"), e.add("b", "work")
	var started atomic.Int64
	_, stop := e.start(Config{Concurrency: 2, Specs: map[string]Spec{"work": {
		Handle: func(ctx context.Context, jb jobqueue.Job, p Progress) (string, error) {
			started.Add(1)
			<-ctx.Done()
			return "", ctx.Err()
		}}}})
	eventually(t, "both running", 3*time.Second, func() bool { return started.Load() == 2 })
	stop()
	for _, id := range []int64{a.ID, b.ID} {
		got := e.get(id)
		if got.Status != jobqueue.Queued || got.Attempts != 0 {
			t.Errorf("job %d after shutdown: %+v (want queued with its attempt handed back)", id, got)
		}
	}
	if e.count(EvReleased) != 2 || e.count(EvRetry) != 0 || e.count(EvDead) != 0 {
		t.Errorf("events: released=%d retry=%d dead=%d", e.count(EvReleased), e.count(EvRetry), e.count(EvDead))
	}
}

func TestShutdownNeverEatsAttempts(t *testing.T) {
	e := newEnv(t)
	j := e.addWith("a", "work", jobqueue.EnqueueOptions{MaxAttempts: 2})
	for i := 0; i < 6; i++ { // six stop/start cycles on a job with only 2 attempts
		started := make(chan struct{}, 1)
		_, stop := e.start(Config{Concurrency: 1, Specs: map[string]Spec{"work": {
			Handle: func(ctx context.Context, jb jobqueue.Job, p Progress) (string, error) {
				started <- struct{}{}
				<-ctx.Done()
				return "", ctx.Err()
			}}}})
		<-started
		stop()
	}
	if got := e.get(j.ID); got.Status != jobqueue.Queued || got.Attempts != 0 {
		t.Errorf("after six restarts: %+v", got)
	}
}

// ---- failure ----------------------------------------------------------------------------

func TestFailureRetriesWithBackoffThenGoesDead(t *testing.T) {
	e := newEnv(t)
	j := e.addWith("a", "work", jobqueue.EnqueueOptions{MaxAttempts: 3})
	var runs atomic.Int64
	e.start(Config{Specs: map[string]Spec{"work": {Handle: func(ctx context.Context, jb jobqueue.Job, p Progress) (string, error) {
		runs.Add(1)
		return "", errors.New("model is busy")
	}}}})
	eventually(t, "dead", 5*time.Second, func() bool { return e.get(j.ID).Status == jobqueue.Dead })
	got := e.get(j.ID)
	if got.Attempts != 3 || runs.Load() != 3 || got.LastError != "model is busy" {
		t.Errorf("attempts=%d runs=%d lastError=%q", got.Attempts, runs.Load(), got.LastError)
	}
	if e.count(EvRetry) != 2 || e.count(EvDead) != 1 {
		t.Errorf("retry=%d dead=%d", e.count(EvRetry), e.count(EvDead))
	}
	if _, found := e.result(j.ID); found {
		t.Error("a dead job has a result")
	}
}

func TestPermanentErrorSkipsTheRetries(t *testing.T) {
	e := newEnv(t)
	j := e.add("a", "work")
	var runs atomic.Int64
	e.start(Config{Specs: map[string]Spec{"work": {Handle: func(ctx context.Context, jb jobqueue.Job, p Progress) (string, error) {
		runs.Add(1)
		return "", Permanent(errors.New("payload is nonsense"))
	}}}})
	eventually(t, "dead", 3*time.Second, func() bool { return e.get(j.ID).Status == jobqueue.Dead })
	time.Sleep(60 * time.Millisecond)
	if runs.Load() != 1 || e.get(j.ID).LastError != "payload is nonsense" {
		t.Errorf("runs=%d lastError=%q", runs.Load(), e.get(j.ID).LastError)
	}
}

func TestAPanickingHandlerFailsOneJobNotTheWorker(t *testing.T) {
	e := newEnv(t)
	bad := e.addWith("bad", "work", jobqueue.EnqueueOptions{MaxAttempts: 1})
	good := e.add("good", "ok")
	e.start(Config{Specs: map[string]Spec{
		"work": {Handle: func(ctx context.Context, jb jobqueue.Job, p Progress) (string, error) { panic("kaboom") }},
		"ok":   okHandler("fine"),
	}})
	eventually(t, "good job done", 3*time.Second, allDone(e, good.ID))
	eventually(t, "bad job dead", 3*time.Second, func() bool { return e.get(bad.ID).Status == jobqueue.Dead })
	if !strings.Contains(e.get(bad.ID).LastError, "panicked: kaboom") {
		t.Errorf("last error %q", e.get(bad.ID).LastError)
	}
}

func TestJobTimeoutIsAFailureNotARelease(t *testing.T) {
	e := newEnv(t)
	j := e.addWith("a", "work", jobqueue.EnqueueOptions{MaxAttempts: 1})
	e.start(Config{JobTimeout: 40 * time.Millisecond, Specs: map[string]Spec{"work": {
		Handle: func(ctx context.Context, jb jobqueue.Job, p Progress) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		}}}})
	eventually(t, "dead", 3*time.Second, func() bool { return e.get(j.ID).Status == jobqueue.Dead })
	if !strings.Contains(e.get(j.ID).LastError, "deadline") || e.count(EvReleased) != 0 {
		t.Errorf("lastError=%q released=%d: a hung handler must be failed, not endlessly given back", e.get(j.ID).LastError, e.count(EvReleased))
	}
}

func TestUnknownKindsAreNeverClaimed(t *testing.T) {
	e := newEnv(t)
	mystery := e.add("m", "mystery")
	mine := e.add("a", "work")
	e.start(Config{Specs: map[string]Spec{"work": okHandler("x")}})
	eventually(t, "mine done", 3*time.Second, allDone(e, mine.ID))
	time.Sleep(50 * time.Millisecond)
	if got := e.get(mystery.ID); got.Status != jobqueue.Queued || got.Attempts != 0 {
		t.Errorf("a job of a kind this worker cannot run was touched: %+v", got)
	}
}

// ---- leases ------------------------------------------------------------------------------

func TestHeartbeatKeepsALongJobFromBeingTakenOver(t *testing.T) {
	e := newEnv(t)
	j := e.add("a", "work")
	started := make(chan struct{})
	e.start(Config{Lease: 120 * time.Millisecond, Specs: map[string]Spec{"work": {
		Handle: func(ctx context.Context, jb jobqueue.Job, p Progress) (string, error) {
			close(started)
			time.Sleep(600 * time.Millisecond) // five times the lease
			return "slow but finished", nil
		}}}})
	<-started // only now does the "other worker" start looking for work
	var thieves atomic.Int64
	stopThief := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopThief:
				return
			default:
			}
			if _, ok, _ := e.q.Claim(bg, time.Minute); ok {
				thieves.Add(1)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	eventually(t, "done", 5*time.Second, allDone(e, j.ID))
	close(stopThief)
	wg.Wait()
	if thieves.Load() != 0 {
		t.Errorf("another worker took the job %d time(s): the lease was not renewed", thieves.Load())
	}
	if got := e.get(j.ID); got.Attempts != 1 {
		t.Errorf("attempts = %d", got.Attempts)
	}
}

// takeOver simulates a second worker winning the job after the first one's
// lease ran out: it expires the lease by hand and claims the job.
//
// It does this in ONE statement, the same bump of the attempt number that
// Claim makes. Expiring the lease and then calling Claim leaves a gap in which
// the worker under test (which also polls for work, and renews short leases)
// can win the job itself, which made this helper fail on a loaded machine.
func (e *env) takeOver() jobqueue.Job {
	e.t.Helper()
	id := e.rawInt(`SELECT id FROM jobs WHERE status='running'`)
	until := time.Now().Add(time.Minute).UnixNano()
	e.rawExec(`UPDATE jobs SET attempts = attempts + 1, lease_until = ?, updated_at = ?,
		last_error = 'lease expired: the worker never reported back'
		WHERE id = ? AND status = 'running'`, until, time.Now().UnixNano(), id)
	j, err := e.q.Get(bg, id)
	if err != nil {
		e.t.Fatalf("takeover: %v", err)
	}
	return j
}

func TestLostLeaseStopsTheHandlerAndReportsNothing(t *testing.T) {
	e := newEnv(t)
	j := e.add("a", "work")
	started := make(chan struct{})
	stopped := make(chan error, 1)
	e.start(Config{Lease: 60 * time.Millisecond, Specs: map[string]Spec{"work": {
		Handle: func(ctx context.Context, jb jobqueue.Job, p Progress) (string, error) {
			close(started)
			<-ctx.Done()
			stopped <- context.Cause(ctx)
			return "", ctx.Err()
		}}}})
	<-started
	other := e.takeOver() // attempts is now 2: owned by "someone else"
	select {
	case cause := <-stopped:
		if !errors.Is(cause, errLostClaim) {
			t.Errorf("handler stopped for the wrong reason: %v", cause)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the handler kept running after its lease was taken over")
	}
	eventually(t, "lost-claim event", 3*time.Second, func() bool { return e.count(EvLostClaim) == 1 })
	got := e.get(j.ID)
	if got.Status != jobqueue.Running || got.Attempts != other.Attempts {
		t.Errorf("the stale worker disturbed the new owner's claim: %+v", got)
	}
	if e.count(EvRetry)+e.count(EvDead)+e.count(EvReleased) != 0 {
		t.Error("the stale worker reported the job")
	}
	// the new owner can still finish it
	if err := e.q.CompleteWith(bg, other.ID, other.Attempts, "new owner"); err != nil {
		t.Errorf("new owner could not complete: %v", err)
	}
}

func TestResultOfAWorkerThatLostItsClaimIsDiscarded(t *testing.T) {
	e := newEnv(t)
	j := e.add("a", "work")
	started := make(chan struct{})
	release := make(chan struct{})
	e.start(Config{Lease: time.Hour, Specs: map[string]Spec{"work": {
		Handle: func(ctx context.Context, jb jobqueue.Job, p Progress) (string, error) {
			close(started)
			<-release // ignores ctx on purpose: a handler that does not notice
			return "late result from the old worker", nil
		}}}})
	<-started
	other := e.takeOver()
	if err := e.q.CompleteWith(bg, other.ID, other.Attempts, "result from the new owner"); err != nil {
		t.Fatal(err)
	}
	close(release)
	eventually(t, "lost-claim event", 3*time.Second, func() bool { return e.count(EvLostClaim) == 1 })
	if r, _ := e.result(j.ID); r.Text != "result from the new owner" || r.Attempt != 2 {
		t.Errorf("the old worker overwrote the result: %+v", r)
	}
	if e.count(EvDone) != 0 {
		t.Error("the old worker reported done")
	}
}

// ---- progress ------------------------------------------------------------------------------

func TestCheckpointComesBackOnTheNextAttempt(t *testing.T) {
	e := newEnv(t)
	j := e.addWith("a", "work", jobqueue.EnqueueOptions{MaxAttempts: 3})
	var seen []string
	var mu sync.Mutex
	e.start(Config{Specs: map[string]Spec{"work": {
		Handle: func(ctx context.Context, jb jobqueue.Job, p Progress) (string, error) {
			mu.Lock()
			seen = append(seen, jb.Progress)
			n := len(seen)
			mu.Unlock()
			if n == 1 {
				if err := p.Save(ctx, "batch 1 of 3 done"); err != nil {
					return "", err
				}
				return "", errors.New("crashed half way")
			}
			return "resumed from: " + jb.Progress, nil
		}}}})
	eventually(t, "done", 5*time.Second, allDone(e, j.ID))
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "" || seen[1] != "batch 1 of 3 done" {
		t.Errorf("progress seen by successive attempts: %q", seen)
	}
	if r, _ := e.result(j.ID); r.Text != "resumed from: batch 1 of 3 done" {
		t.Errorf("result %q", r.Text)
	}
}

// ---- lifecycle ------------------------------------------------------------------------------

func TestExitWhenIdleDrainsThenReturns(t *testing.T) {
	e := newEnv(t)
	var ids []int64
	for i := 0; i < 12; i++ {
		ids = append(ids, e.add(fmt.Sprint("k", i), "work").ID)
	}
	w, err := New(Config{Queue: e.q, ExitWhenIdle: true, Concurrency: 3, Poll: 5 * time.Millisecond,
		Specs: map[string]Spec{"work": {Handle: func(ctx context.Context, jb jobqueue.Job, p Progress) (string, error) {
			time.Sleep(10 * time.Millisecond)
			return "ok", nil
		}}}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { w.Run(bg); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run never returned although the queue was empty")
	}
	for _, id := range ids {
		if e.get(id).Status != jobqueue.Done {
			t.Errorf("job %d not done when Run returned", id)
		}
	}
	if s := w.Summary(); s.Done != 12 {
		t.Errorf("summary %+v", s)
	}
}

func TestExitWhenIdleWaitsForRunningJobs(t *testing.T) {
	e := newEnv(t)
	j := e.add("a", "work")
	w, _ := New(Config{Queue: e.q, ExitWhenIdle: true, Poll: 5 * time.Millisecond,
		Specs: map[string]Spec{"work": {Handle: func(ctx context.Context, jb jobqueue.Job, p Progress) (string, error) {
			time.Sleep(150 * time.Millisecond)
			return "ok", nil
		}}}})
	w.Run(bg)
	if e.get(j.ID).Status != jobqueue.Done {
		t.Error("Run returned while a job was still running")
	}
}

func TestExitWhenIdleLeavesBackedOffJobsQueued(t *testing.T) {
	e := newEnv(t)
	j := e.addWith("a", "work", jobqueue.EnqueueOptions{RunAt: time.Now().Add(time.Hour)})
	w, _ := New(Config{Queue: e.q, ExitWhenIdle: true, Poll: 5 * time.Millisecond, Specs: map[string]Spec{"work": okHandler("x")}})
	start := time.Now()
	w.Run(bg)
	if time.Since(start) > 2*time.Second {
		t.Error("waited for a job that is not due for an hour")
	}
	if e.get(j.ID).Status != jobqueue.Queued {
		t.Error("a future job was run")
	}
}

func TestNewWorkIsPickedUpWhileRunning(t *testing.T) {
	e := newEnv(t)
	e.start(Config{Poll: 10 * time.Millisecond, Specs: map[string]Spec{"work": okHandler("x")}})
	time.Sleep(50 * time.Millisecond)
	j := e.add("late", "work")
	eventually(t, "late job done", 3*time.Second, allDone(e, j.ID))
}

func TestConfigValidation(t *testing.T) {
	e := newEnv(t)
	ok := map[string]Spec{"w": okHandler("x")}
	bad := []Config{
		{Specs: ok},
		{Queue: e.q},
		{Queue: e.q, Specs: map[string]Spec{"w": {}}},
		{Queue: e.q, Specs: ok, Concurrency: -1},
		{Queue: e.q, Specs: ok, Lease: -time.Second},
		{Queue: e.q, Specs: ok, Poll: -1},
		{Queue: e.q, Specs: ok, JobTimeout: -1},
	}
	for i, c := range bad {
		if _, err := New(c); err == nil {
			t.Errorf("case %d accepted: %+v", i, c)
		}
	}
}

func TestPermanentWrapper(t *testing.T) {
	if Permanent(nil) != nil {
		t.Error("Permanent(nil) must be nil")
	}
	base := errors.New("nope")
	p := Permanent(base)
	if !errors.Is(p, ErrPermanent) || !errors.Is(p, base) || p.Error() != "nope" {
		t.Errorf("Permanent: %v is=%v %v", p, errors.Is(p, ErrPermanent), errors.Is(p, base))
	}
	if errors.Is(base, ErrPermanent) {
		t.Error("a plain error looks permanent")
	}
	if !errors.Is(fmt.Errorf("wrapped: %w", p), ErrPermanent) {
		t.Error("wrapping hides the permanent marker")
	}
}

// The network can drop in the instant between the claim and the handler
// starting. The worker checks once more before running, rather than waiting
// for its next periodic check (set absurdly far away here so only the
// immediate check can save this job).
func TestNetworkDroppingBetweenClaimAndStartReleasesTheJob(t *testing.T) {
	e := newEnv(t)
	j := e.add("a", "net")
	var asked atomic.Int64
	online := func() bool { return asked.Add(1) == 1 } // up for the claim, down right after
	var ran atomic.Bool
	e.start(Config{Concurrency: 1, Online: online, Poll: time.Hour, Lease: time.Hour, Specs: map[string]Spec{"net": {
		NeedsNetwork: true,
		Handle: func(ctx context.Context, jb jobqueue.Job, p Progress) (string, error) {
			ran.Store(true)
			return "should not have started", nil
		}}}})
	eventually(t, "released before it started", 3*time.Second, func() bool { return e.count(EvReleased) == 1 })
	if ran.Load() {
		t.Error("the handler started although the network was already down")
	}
	if got := e.get(j.ID); got.Status != jobqueue.Queued || got.Attempts != 0 {
		t.Errorf("job = %+v", got)
	}
}
