package jobqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"offgrid/internal/store/sqlstore"
)

var epoch = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

var ctx = context.Background()

// newQueue builds a queue in a fresh file with a hand-set clock and
// randomness fixed at 0.5 (so jitter is predictable).
func newQueue(t *testing.T, mut func(*Config)) (*Queue, *clock, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "queue.db")
	return openQueue(t, path, &clock{t: epoch}, mut)
}

func openQueue(t *testing.T, path string, clk *clock, mut func(*Config)) (*Queue, *clock, string) {
	t.Helper()
	db, err := sqlstore.OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := Config{
		Backoff: Backoff{Base: 10 * time.Second, Max: 5 * time.Minute, Jitter: NoJitter},
		Now:     clk.now,
		Rand:    func() float64 { return 0.5 },
	}
	if mut != nil {
		mut(&cfg)
	}
	q, err := New(ctx, db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return q, clk, path
}

func enqueue(t *testing.T, q *Queue, key string) Job {
	t.Helper()
	j, created, err := q.Enqueue(ctx, key, "summarize", json.RawMessage(`{"week":41}`), EnqueueOptions{})
	if err != nil || !created {
		t.Fatalf("Enqueue(%s): created=%v err=%v", key, created, err)
	}
	return j
}

func claim(t *testing.T, q *Queue) Job {
	t.Helper()
	j, ok, err := q.Claim(ctx, time.Minute)
	if err != nil || !ok {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}
	return j
}

func mustGet(t *testing.T, q *Queue, id int64) Job {
	t.Helper()
	j, err := q.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestEnqueueClaimComplete(t *testing.T) {
	q, clk, _ := newQueue(t, nil)
	j := enqueue(t, q, "k1")
	if j.Status != Queued || j.Attempts != 0 || j.MaxAttempts != 5 || !j.RunAt.Equal(epoch) || !j.CreatedAt.Equal(epoch) {
		t.Fatalf("new job = %+v", j)
	}
	if string(j.Payload) != `{"week":41}` || j.Kind != "summarize" || j.Key != "k1" {
		t.Fatalf("job content = %+v", j)
	}

	clk.advance(time.Second)
	c := claim(t, q)
	if c.ID != j.ID || c.Status != Running || c.Attempts != 1 || !c.LeaseUntil.Equal(epoch.Add(time.Second+time.Minute)) {
		t.Fatalf("claimed = %+v", c)
	}
	if _, ok, _ := q.Claim(ctx, time.Minute); ok {
		t.Fatal("a running job must not be handed out twice")
	}
	if err := q.Complete(ctx, c.ID, c.Attempts); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, q, c.ID); got.Status != Done || !got.LeaseUntil.IsZero() {
		t.Fatalf("after complete: %+v", got)
	}
	if _, ok, _ := q.Claim(ctx, time.Minute); ok {
		t.Fatal("a done job must not come back")
	}
}

func TestClaimOrderIsRunAtThenID(t *testing.T) {
	q, clk, _ := newQueue(t, nil)
	_, _, _ = q.Enqueue(ctx, "late", "k", nil, EnqueueOptions{RunAt: epoch.Add(time.Hour)})
	enqueue(t, q, "first")
	clk.advance(time.Second)
	enqueue(t, q, "second")
	clk.advance(2 * time.Hour)
	var order []string
	for i := 0; i < 3; i++ {
		order = append(order, claim(t, q).Key)
	}
	want := []string{"first", "second", "late"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestNothingReadyBeforeRunAt(t *testing.T) {
	q, clk, _ := newQueue(t, nil)
	_, _, _ = q.Enqueue(ctx, "later", "k", nil, EnqueueOptions{RunAt: epoch.Add(time.Minute)})
	if _, ok, _ := q.Claim(ctx, time.Minute); ok {
		t.Fatal("claimed before run_at")
	}
	clk.advance(time.Minute - time.Nanosecond)
	if _, ok, _ := q.Claim(ctx, time.Minute); ok {
		t.Fatal("claimed one nanosecond early")
	}
	clk.advance(time.Nanosecond)
	claim(t, q)
}

// ---- idempotency --------------------------------------------------------

func TestEnqueueIsIdempotentByKey(t *testing.T) {
	q, _, _ := newQueue(t, nil)
	a := enqueue(t, q, "same")
	b, created, err := q.Enqueue(ctx, "same", "summarize", json.RawMessage(`{"week": 41}`), EnqueueOptions{}) // same JSON, different spacing
	if err != nil || created {
		t.Fatalf("repeat: created=%v err=%v", created, err)
	}
	if b.ID != a.ID {
		t.Errorf("repeat returned job %d, want the original %d", b.ID, a.ID)
	}
	if s, _ := q.Stats(ctx); s.Total() != 1 {
		t.Errorf("total = %d, want 1: a repeat must not add a job", s.Total())
	}
}

func TestIdempotencySurvivesTheJobFinishing(t *testing.T) {
	q, _, _ := newQueue(t, nil)
	j := enqueue(t, q, "once")
	c := claim(t, q)
	if err := q.Complete(ctx, c.ID, c.Attempts); err != nil {
		t.Fatal(err)
	}
	again, created, err := q.Enqueue(ctx, "once", "summarize", json.RawMessage(`{"week":41}`), EnqueueOptions{})
	if err != nil || created || again.ID != j.ID || again.Status != Done {
		t.Fatalf("re-enqueue of a finished job: %+v created=%v err=%v", again, created, err)
	}
}

func TestSameKeyDifferentContentIsAConflict(t *testing.T) {
	q, _, _ := newQueue(t, nil)
	enqueue(t, q, "k")
	if _, _, err := q.Enqueue(ctx, "k", "summarize", json.RawMessage(`{"week":42}`), EnqueueOptions{}); !errors.Is(err, ErrKeyConflict) {
		t.Errorf("different payload: %v, want ErrKeyConflict", err)
	}
	if _, _, err := q.Enqueue(ctx, "k", "other", json.RawMessage(`{"week":41}`), EnqueueOptions{}); !errors.Is(err, ErrKeyConflict) {
		t.Errorf("different kind: %v, want ErrKeyConflict", err)
	}
}

func TestEnqueueValidation(t *testing.T) {
	q, _, _ := newQueue(t, nil)
	long := make([]byte, maxKey+1)
	for i := range long {
		long[i] = 'x'
	}
	cases := []struct {
		name, key, kind string
		payload         string
		max             int
	}{
		{"empty key", "", "k", "{}", 0},
		{"blank key", "   ", "k", "{}", 0},
		{"long key", string(long), "k", "{}", 0},
		{"empty kind", "a", "", "{}", 0},
		{"upper kind", "a", "Summarize", "{}", 0},
		{"kind with space", "a", "do it", "{}", 0},
		{"bad json", "a", "k", "{nope", 0},
		{"huge payload", "a", "k", `"` + string(make([]byte, maxPayload)) + `"`, 0},
		{"negative attempts", "a", "k", "{}", -1},
	}
	for _, c := range cases {
		if _, _, err := q.Enqueue(ctx, c.key, c.kind, json.RawMessage(c.payload), EnqueueOptions{MaxAttempts: c.max}); err == nil {
			t.Errorf("%s: should be refused", c.name)
		}
	}
	if s, _ := q.Stats(ctx); s.Total() != 0 {
		t.Errorf("refused jobs must not be stored, total = %d", s.Total())
	}
}

func TestEmptyPayloadBecomesEmptyObject(t *testing.T) {
	q, _, _ := newQueue(t, nil)
	j, _, err := q.Enqueue(ctx, "k", "ping", nil, EnqueueOptions{})
	if err != nil || string(j.Payload) != "{}" {
		t.Fatalf("%v %s", err, j.Payload)
	}
}

// ---- retries, backoff, dead letter ----------------------------------------

func TestFailRetriesWithBackoff(t *testing.T) {
	q, clk, _ := newQueue(t, nil)
	enqueue(t, q, "k")
	// attempts 1..4 fail and back off 10s, 20s, 40s, 80s (no jitter in this fixture)
	for i, wantDelay := range []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second} {
		c := claim(t, q)
		if c.Attempts != i+1 {
			t.Fatalf("attempt number = %d, want %d", c.Attempts, i+1)
		}
		res, err := q.Fail(ctx, c.ID, c.Attempts, errors.New("model timed out"), false)
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != Queued || res.Delay != wantDelay || !res.RunAt.Equal(clk.now().Add(wantDelay)) {
			t.Fatalf("attempt %d: %+v, want retry in %v", i+1, res, wantDelay)
		}
		got := mustGet(t, q, c.ID)
		if got.Status != Queued || got.LastError != "model timed out" || !got.RunAt.Equal(clk.now().Add(wantDelay)) {
			t.Fatalf("stored = %+v", got)
		}
		// not ready until the delay has passed
		if _, ok, _ := q.Claim(ctx, time.Minute); ok {
			t.Fatal("retried before the backoff elapsed")
		}
		clk.advance(wantDelay)
	}
}

func TestJitterIsAppliedFromTheInjectedRandom(t *testing.T) {
	q, _, _ := newQueue(t, func(c *Config) {
		c.Backoff = Backoff{Base: 10 * time.Second, Max: time.Hour, Jitter: FullJitter}
		c.Rand = func() float64 { return 0.25 }
	})
	enqueue(t, q, "k")
	c := claim(t, q)
	res, _ := q.Fail(ctx, c.ID, c.Attempts, errors.New("x"), false)
	if res.Delay != 2500*time.Millisecond {
		t.Errorf("delay %v, want 2.5s (0.25 x 10s ceiling)", res.Delay)
	}
}

func TestDeadAfterMaxAttempts(t *testing.T) {
	q, clk, _ := newQueue(t, nil)
	j, _, _ := q.Enqueue(ctx, "k", "summarize", nil, EnqueueOptions{MaxAttempts: 3})
	var res FailResult
	for i := 1; i <= 3; i++ {
		c := claim(t, q)
		var err error
		res, err = q.Fail(ctx, c.ID, c.Attempts, fmt.Errorf("boom %d", i), false)
		if err != nil {
			t.Fatal(err)
		}
		clk.advance(time.Hour)
	}
	if res.Status != Dead {
		t.Fatalf("third failure: %+v, want dead", res)
	}
	got := mustGet(t, q, j.ID)
	if got.Status != Dead || got.LastError != "boom 3" || got.Attempts != 3 {
		t.Fatalf("dead job = %+v (the last error must be kept)", got)
	}
	if _, ok, _ := q.Claim(ctx, time.Minute); ok {
		t.Fatal("a dead job must never be handed out")
	}
}

func TestPermanentFailureIsDeadImmediately(t *testing.T) {
	q, _, _ := newQueue(t, nil)
	enqueue(t, q, "k")
	c := claim(t, q)
	res, err := q.Fail(ctx, c.ID, c.Attempts, errors.New("payload is invalid"), true)
	if err != nil || res.Status != Dead {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestMaxAttemptsOfOneNeverRetries(t *testing.T) {
	q, _, _ := newQueue(t, nil)
	_, _, _ = q.Enqueue(ctx, "k", "summarize", nil, EnqueueOptions{MaxAttempts: 1})
	c := claim(t, q)
	if res, _ := q.Fail(ctx, c.ID, c.Attempts, errors.New("x"), false); res.Status != Dead {
		t.Errorf("%+v", res)
	}
}

func TestRetryRequeuesADeadJob(t *testing.T) {
	q, clk, _ := newQueue(t, nil)
	_, _, _ = q.Enqueue(ctx, "k", "summarize", nil, EnqueueOptions{MaxAttempts: 1})
	c := claim(t, q)
	q.Fail(ctx, c.ID, c.Attempts, errors.New("x"), false)
	clk.advance(time.Minute)
	if err := q.Retry(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	got := mustGet(t, q, c.ID)
	if got.Status != Queued || got.Attempts != 0 || !got.RunAt.Equal(clk.now()) {
		t.Fatalf("after Retry: %+v", got)
	}
	c2 := claim(t, q)
	if c2.Attempts != 1 {
		t.Errorf("a requeued job starts its attempts over, got %d", c2.Attempts)
	}
	if err := q.Retry(ctx, c2.ID); !errors.Is(err, ErrNotDead) {
		t.Errorf("Retry of a running job: %v, want ErrNotDead", err)
	}
	if err := q.Retry(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("Retry of a missing job: %v, want ErrNotFound", err)
	}
}

func TestLongErrorsAreTruncated(t *testing.T) {
	q, _, _ := newQueue(t, nil)
	enqueue(t, q, "k")
	c := claim(t, q)
	q.Fail(ctx, c.ID, c.Attempts, errors.New(string(make([]byte, 5000))), true)
	if got := mustGet(t, q, c.ID); len(got.LastError) != 1000 {
		t.Errorf("error length %d, want 1000", len(got.LastError))
	}
}

// ---- crashes, leases, fencing ------------------------------------------

func TestExpiredLeaseMakesTheJobClaimableAgain(t *testing.T) {
	q, clk, _ := newQueue(t, nil)
	enqueue(t, q, "k")
	first := claim(t, q) // the worker claims, then crashes: it never reports back
	clk.advance(30 * time.Second)
	if _, ok, _ := q.Claim(ctx, time.Minute); ok {
		t.Fatal("handed out while the lease is still valid")
	}
	clk.advance(31 * time.Second) // lease (1 minute) is over
	second := claim(t, q)
	if second.ID != first.ID || second.Attempts != 2 {
		t.Fatalf("second claim = %+v, want the same job as attempt 2", second)
	}
	if second.LastError == "" {
		t.Error("the lost worker should be recorded in last_error")
	}
}

func TestStaleWorkerCannotOverwriteTheNewClaim(t *testing.T) {
	q, clk, _ := newQueue(t, nil)
	enqueue(t, q, "k")
	slow := claim(t, q)
	clk.advance(2 * time.Minute) // the slow worker's lease runs out
	fresh := claim(t, q)         // someone else takes over

	if err := q.Complete(ctx, slow.ID, slow.Attempts); !errors.Is(err, ErrStaleClaim) {
		t.Errorf("stale Complete: %v, want ErrStaleClaim", err)
	}
	if _, err := q.Fail(ctx, slow.ID, slow.Attempts, errors.New("late"), true); !errors.Is(err, ErrStaleClaim) {
		t.Errorf("stale Fail: %v, want ErrStaleClaim", err)
	}
	if err := q.Extend(ctx, slow.ID, slow.Attempts, time.Hour); !errors.Is(err, ErrStaleClaim) {
		t.Errorf("stale Extend: %v, want ErrStaleClaim", err)
	}
	if got := mustGet(t, q, fresh.ID); got.Status != Running || got.Attempts != 2 {
		t.Fatalf("the stale worker disturbed the job: %+v", got)
	}
	if err := q.Complete(ctx, fresh.ID, fresh.Attempts); err != nil {
		t.Errorf("the current worker must still be able to finish: %v", err)
	}
}

func TestCompleteTwiceAndUnknownJob(t *testing.T) {
	q, _, _ := newQueue(t, nil)
	enqueue(t, q, "k")
	c := claim(t, q)
	if err := q.Complete(ctx, c.ID, c.Attempts); err != nil {
		t.Fatal(err)
	}
	if err := q.Complete(ctx, c.ID, c.Attempts); !errors.Is(err, ErrStaleClaim) {
		t.Errorf("second Complete: %v", err)
	}
	if err := q.Complete(ctx, 12345, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown job: %v", err)
	}
}

func TestExtendKeepsALongJobAlive(t *testing.T) {
	q, clk, _ := newQueue(t, nil)
	enqueue(t, q, "k")
	c := claim(t, q) // 1 minute lease
	for i := 0; i < 5; i++ {
		clk.advance(50 * time.Second)
		if err := q.Extend(ctx, c.ID, c.Attempts, time.Minute); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := q.Claim(ctx, time.Minute); ok {
			t.Fatal("a job whose lease keeps being extended was taken away")
		}
	}
	if err := q.Complete(ctx, c.ID, c.Attempts); err != nil {
		t.Fatal(err)
	}
}

func TestLostWorkerOnTheFinalAttemptGoesToDeadLetter(t *testing.T) {
	q, clk, _ := newQueue(t, nil)
	j, _, _ := q.Enqueue(ctx, "k", "summarize", nil, EnqueueOptions{MaxAttempts: 2})
	claim(t, q) // attempt 1, worker crashes
	clk.advance(2 * time.Minute)
	claim(t, q) // attempt 2 (the last), worker crashes too
	clk.advance(2 * time.Minute)
	if _, ok, _ := q.Claim(ctx, time.Minute); ok {
		t.Fatal("a job with no attempts left was handed out a third time")
	}
	got := mustGet(t, q, j.ID)
	if got.Status != Dead || got.LastError == "" || got.Attempts != 2 {
		t.Fatalf("job = %+v, want dead with an explanation", got)
	}
}

// ---- durability ------------------------------------------------------------

// "100 jobs enqueued offline all persist": enqueue, close everything (as a
// power cut or restart would), reopen the file, and find them all, in order,
// with their content, still claimable.
func Test100JobsPersistAcrossARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	clk := &clock{t: epoch}
	db, err := sqlstore.OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	q, err := New(ctx, db, Config{Now: clk.now})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 100; i++ {
		clk.advance(time.Millisecond)
		if _, _, err := q.Enqueue(ctx, fmt.Sprintf("offline-%03d", i), "summarize", json.RawMessage(fmt.Sprintf(`{"n":%d}`, i)), EnqueueOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	db.Close() // the process ends

	q2, _, _ := openQueue(t, path, clk, nil)
	st, err := q2.Stats(ctx)
	if err != nil || st.Queued != 100 || st.Total() != 100 {
		t.Fatalf("after restart: %+v err=%v, want 100 queued", st, err)
	}
	jobs, _ := q2.List(ctx, "", 0)
	for i, j := range jobs {
		if want := fmt.Sprintf("offline-%03d", i+1); j.Key != want || string(j.Payload) != fmt.Sprintf(`{"n":%d}`, i+1) || j.Status != Queued {
			t.Fatalf("job %d = %+v", i, j)
		}
	}
	clk.advance(time.Second)
	for i := 1; i <= 100; i++ {
		if c := claim(t, q2); c.Key != fmt.Sprintf("offline-%03d", i) {
			t.Fatalf("claim %d returned %s: order must survive the restart", i, c.Key)
		}
	}
}

func TestStateSurvivesRestartMidLifecycle(t *testing.T) {
	q, clk, path := newQueue(t, nil)
	enqueue(t, q, "a")
	enqueue(t, q, "b")
	enqueue(t, q, "c")
	ca := claim(t, q)
	q.Complete(ctx, ca.ID, ca.Attempts)
	cb := claim(t, q)
	q.Fail(ctx, cb.ID, cb.Attempts, errors.New("x"), true)
	// c stays queued

	q2, _, _ := openQueue(t, path, clk, nil)
	st, _ := q2.Stats(ctx)
	if st.Done != 1 || st.Dead != 1 || st.Queued != 1 || st.Running != 0 {
		t.Fatalf("stats after restart = %+v", st)
	}
}

// ---- concurrency -------------------------------------------------------------

func TestConcurrentWorkersNeverShareAJob(t *testing.T) {
	q, _, _ := newQueue(t, nil)
	const jobs, workers = 200, 8
	for i := 0; i < jobs; i++ {
		enqueue(t, q, fmt.Sprintf("j%03d", i))
	}
	var mu sync.Mutex
	var got []string
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				j, ok, err := q.Claim(ctx, time.Minute)
				if err != nil {
					t.Error(err)
					return
				}
				if !ok {
					return
				}
				if err := q.Complete(ctx, j.ID, j.Attempts); err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				got = append(got, j.Key)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	sort.Strings(got)
	if len(got) != jobs {
		t.Fatalf("processed %d jobs, want %d", len(got), jobs)
	}
	for i := 1; i < len(got); i++ {
		if got[i] == got[i-1] {
			t.Fatalf("job %s was handed to two workers", got[i])
		}
	}
}

func TestConcurrentEnqueueOfTheSameKeyCreatesOne(t *testing.T) {
	q, _, _ := newQueue(t, nil)
	var wg sync.WaitGroup
	var mu sync.Mutex
	created := 0
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, c, err := q.Enqueue(ctx, "dup", "summarize", json.RawMessage(`{"a":1}`), EnqueueOptions{})
			if err != nil {
				t.Error(err)
			}
			if c {
				mu.Lock()
				created++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if created != 1 {
		t.Errorf("%d goroutines believe they created the job, want exactly 1", created)
	}
}

// ---- reading, schema, misc ---------------------------------------------------

func TestListAndStats(t *testing.T) {
	q, clk, _ := newQueue(t, nil)
	for i := 0; i < 4; i++ {
		enqueue(t, q, fmt.Sprintf("k%d", i))
	}
	c := claim(t, q)
	q.Complete(ctx, c.ID, c.Attempts)
	c = claim(t, q)
	q.Fail(ctx, c.ID, c.Attempts, errors.New("x"), false) // back to queued, 10s later
	claim(t, q)                                           // running

	st, _ := q.Stats(ctx)
	if st.Done != 1 || st.Running != 1 || st.Queued != 2 || st.Dead != 0 || st.Total() != 4 {
		t.Fatalf("stats = %+v", st)
	}
	if st.NextReady.IsZero() || st.NextReady.After(clk.now().Add(time.Minute+time.Second)) {
		t.Errorf("NextReady = %v", st.NextReady)
	}
	if l, _ := q.List(ctx, Queued, 0); len(l) != 2 {
		t.Errorf("queued list = %d", len(l))
	}
	if l, _ := q.List(ctx, "", 3); len(l) != 3 {
		t.Errorf("limit not applied: %d", len(l))
	}
	if _, err := q.List(ctx, "bogus", 0); err == nil {
		t.Error("unknown status must be refused")
	}
	if _, err := q.Get(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get missing: %v", err)
	}
}

func TestEmptyQueue(t *testing.T) {
	q, _, _ := newQueue(t, nil)
	if _, ok, err := q.Claim(ctx, time.Minute); ok || err != nil {
		t.Errorf("empty Claim: ok=%v err=%v", ok, err)
	}
	st, err := q.Stats(ctx)
	if err != nil || st.Total() != 0 || !st.NextReady.IsZero() {
		t.Errorf("empty stats %+v %v", st, err)
	}
	if l, err := q.List(ctx, "", 0); err != nil || l == nil || len(l) != 0 {
		t.Errorf("empty list must be a non-nil empty slice: %v %v", l, err)
	}
}

func TestBadLeaseRefused(t *testing.T) {
	q, _, _ := newQueue(t, nil)
	if _, _, err := q.Claim(ctx, 0); err == nil {
		t.Error("zero lease")
	}
	if err := q.Extend(ctx, 1, 1, -time.Second); err == nil {
		t.Error("negative extend")
	}
}

func TestSchemaVersioning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.db")
	db, _ := sqlstore.OpenDB(path)
	defer db.Close()
	if _, err := New(ctx, db, Config{}); err != nil {
		t.Fatal(err)
	}
	var v int
	db.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v != len(migrations) {
		t.Errorf("user_version = %d, want %d", v, len(migrations))
	}
	// opening again must not rerun migrations (CREATE TABLE would fail)
	if _, err := New(ctx, db, Config{}); err != nil {
		t.Errorf("second New: %v", err)
	}
	// a file from the future is refused, not modified
	db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, len(migrations)+1))
	if _, err := New(ctx, db, Config{}); !errors.Is(err, ErrNewerSchema) {
		t.Errorf("newer schema: %v", err)
	}
}

func TestFailedMigrationLeavesNothingBehind(t *testing.T) {
	db, _ := sqlstore.OpenDB(filepath.Join(t.TempDir(), "q.db"))
	defer db.Close()
	saved := migrations
	defer func() { migrations = saved }()
	migrations = append(append([]string{}, saved...), `CREATE TABLE half (x); THIS IS NOT SQL;`)
	if _, err := New(ctx, db, Config{}); err == nil {
		t.Fatal("a broken migration must fail")
	}
	var v int
	db.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v != len(saved) {
		t.Errorf("version %d, want %d (only the good migration is recorded)", v, len(saved))
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='half'`).Scan(&n)
	if n != 0 {
		t.Error("the failed migration left a table behind")
	}
}

func TestDatabaseRefusesBadRows(t *testing.T) {
	// The CHECK constraints are the last line of defence if code has a bug.
	q, _, _ := newQueue(t, nil)
	for _, stmt := range []string{
		`INSERT INTO jobs (key,kind,payload,status,max_attempts,run_at,created_at,updated_at) VALUES ('a','k','{}','weird',1,0,0,0)`,
		`INSERT INTO jobs (key,kind,payload,status,max_attempts,run_at,created_at,updated_at) VALUES ('b','k','not json','queued',1,0,0,0)`,
		`INSERT INTO jobs (key,kind,payload,status,max_attempts,run_at,created_at,updated_at) VALUES ('c','k','{}','queued',0,0,0,0)`,
	} {
		if _, err := q.db.Exec(stmt); err == nil {
			t.Errorf("database accepted: %s", stmt)
		}
	}
}

func TestCancelledContext(t *testing.T) {
	q, _, _ := newQueue(t, nil)
	c, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := q.Enqueue(c, "k", "summarize", nil, EnqueueOptions{}); err == nil {
		t.Error("Enqueue with a cancelled context should fail")
	}
	if s, _ := q.Stats(ctx); s.Total() != 0 {
		t.Error("nothing should have been stored")
	}
}

var _ = sql.ErrNoRows

// Repeats must not burn job numbers: a person reading "job #2, job #4" would
// think a job vanished.
func TestRepeatedEnqueueLeavesNoGapsInIDs(t *testing.T) {
	q, _, _ := newQueue(t, nil)
	a := enqueue(t, q, "a")
	for i := 0; i < 5; i++ {
		if _, created, err := q.Enqueue(ctx, "a", "summarize", json.RawMessage(`{"week":41}`), EnqueueOptions{}); err != nil || created {
			t.Fatalf("repeat %d: created=%v err=%v", i, created, err)
		}
	}
	if _, _, err := q.Enqueue(ctx, "a", "summarize", json.RawMessage(`{"week":99}`), EnqueueOptions{}); !errors.Is(err, ErrKeyConflict) {
		t.Fatalf("conflict: %v", err)
	}
	b := enqueue(t, q, "b")
	if b.ID != a.ID+1 {
		t.Errorf("ids %d then %d: repeats and conflicts must not consume ids", a.ID, b.ID)
	}
}
