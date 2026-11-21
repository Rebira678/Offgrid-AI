package sqlstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"offgrid/internal/task"
)

func gcStore(t *testing.T, opt Options) *Store {
	t.Helper()
	if opt.NodeID == "" {
		opt.NodeID = "node-gc"
	}
	return open(t, newDB(t), opt)
}

func gcMk(s *Store, title string) error {
	_, _, err := s.Create(context.Background(), task.NewTask{Title: title, Priority: task.PriorityMedium})
	return err
}

func titlesOf(t *testing.T, s *Store) map[string]bool {
	t.Helper()
	all, err := s.List(context.Background(), task.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]bool{}
	for _, x := range all {
		m[x.Title] = true
	}
	return m
}

func verifyOK(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.Verify(context.Background()); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if _, err := s.VerifyChain(context.Background()); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
}

func TestConcurrentWritersShareCommits(t *testing.T) {
	s := gcStore(t, Options{FlushWindow: 2 * time.Millisecond})
	const writers, each = 16, 25
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if err := gcMk(s, fmt.Sprintf("w%d-%d", w, i)); err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	st := s.CommitStats()
	if st.Writes != writers*each {
		t.Fatalf("writes = %d, want %d", st.Writes, writers*each)
	}
	if st.Batches >= st.Writes || st.MaxBatch < 2 {
		t.Fatalf("no sharing: %+v", st)
	}
	if got := len(titlesOf(t, s)); got != writers*each {
		t.Fatalf("%d tasks stored, want %d", got, writers*each)
	}
	verifyOK(t, s)
}

func TestAFailingRequestOnlyFailsItself(t *testing.T) {
	s := gcStore(t, Options{FlushWindow: 300 * time.Millisecond})
	const n = 6
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			title := fmt.Sprintf("ok-%d", i)
			if i == 3 {
				title = "" // refused by validation inside the request
			}
			errs[i] = gcMk(s, title)
		}(i)
	}
	wg.Wait()
	bad := 0
	for i, e := range errs {
		if e != nil {
			bad++
			if i != 3 {
				t.Fatalf("request %d failed: %v", i, e)
			}
		}
	}
	if bad != 1 {
		t.Fatalf("%d failures, want 1", bad)
	}
	if got := len(titlesOf(t, s)); got != n-1 {
		t.Fatalf("%d stored, want %d", got, n-1)
	}
	if st := s.CommitStats(); st.MaxBatch < 2 {
		t.Skipf("requests did not land in one batch (%+v); isolation not exercised", st)
	}
	verifyOK(t, s)
}

func TestNoAckBeforeCommit(t *testing.T) {
	s := gcStore(t, Options{})
	gate := make(chan struct{})
	reached := make(chan struct{}, 1)
	s.gc.beforeCommit = func() error {
		select {
		case reached <- struct{}{}:
		default:
		}
		<-gate
		return nil
	}
	var acked atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := gcMk(s, "slow"); err != nil {
			t.Error(err)
		}
		acked.Store(true)
	}()
	<-reached
	time.Sleep(50 * time.Millisecond)
	if acked.Load() {
		t.Fatal("caller was acknowledged before COMMIT")
	}
	close(gate)
	<-done
	if !titlesOf(t, s)["slow"] {
		t.Fatal("write missing after ack")
	}
}

func TestCommitFailureFailsEveryRequestAndWriterSurvives(t *testing.T) {
	s := gcStore(t, Options{FlushWindow: 100 * time.Millisecond})
	boom := errors.New("disk full")
	var fail atomic.Bool
	fail.Store(true)
	s.gc.beforeCommit = func() error {
		if fail.Load() {
			return boom
		}
		return nil
	}
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) { defer wg.Done(); errs[i] = gcMk(s, fmt.Sprintf("x%d", i)) }(i)
	}
	wg.Wait()
	for i, e := range errs {
		if !errors.Is(e, boom) {
			t.Fatalf("request %d: %v, want the commit error", i, e)
		}
	}
	if n := len(titlesOf(t, s)); n != 0 {
		t.Fatalf("%d tasks survived a failed commit", n)
	}
	fail.Store(false)
	if err := gcMk(s, "after"); err != nil {
		t.Fatalf("writer did not survive: %v", err)
	}
	if !titlesOf(t, s)["after"] {
		t.Fatal("write after failure missing")
	}
	verifyOK(t, s)
}

func TestCancelledWhileQueuedNeverRuns(t *testing.T) {
	s := gcStore(t, Options{})
	gate := make(chan struct{})
	reached := make(chan struct{}, 1)
	var once sync.Once
	s.gc.beforeCommit = func() error {
		once.Do(func() { reached <- struct{}{}; <-gate })
		return nil
	}
	first := make(chan error, 1)
	go func() { first <- gcMk(s, "first") }()
	<-reached // writer is stuck in commit of "first"

	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() {
		_, _, err := s.Create(ctx, task.NewTask{Title: "ghost", Priority: task.PriorityLow})
		res <- err
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	if err := <-res; !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	close(gate)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := gcMk(s, "last"); err != nil {
		t.Fatal(err)
	}
	m := titlesOf(t, s)
	if m["ghost"] || !m["first"] || !m["last"] {
		t.Fatalf("titles = %v", m)
	}
	if s.CommitStats().Cancelled != 1 {
		t.Fatalf("stats %+v", s.CommitStats())
	}
}

func TestPanicInARequestIsRaisedInTheCallerAndWriterSurvives(t *testing.T) {
	s := gcStore(t, Options{})
	func() {
		defer func() {
			if r := recover(); r != "kaboom" {
				t.Fatalf("recovered %v, want kaboom", r)
			}
		}()
		_ = s.write(context.Background(), func(tx execer) error { panic("kaboom") })
		t.Fatal("panic was swallowed")
	}()
	if err := gcMk(s, "still alive"); err != nil {
		t.Fatal(err)
	}
}

func TestWriteAfterCloseIsErrClosed(t *testing.T) {
	s := gcStore(t, Options{})
	if err := gcMk(s, "a"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := gcMk(s, "b"); !errors.Is(err, ErrClosed) {
		t.Fatalf("got %v, want ErrClosed", err)
	}
}

func TestCloseWithWritersInFlightLosesNoAckedWrite(t *testing.T) {
	path := newDB(t)
	s, err := Open(context.Background(), path, Options{NodeID: "n", FlushWindow: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	acked := map[string]bool{}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				title := fmt.Sprintf("c%d-%d", w, i)
				if err := gcMk(s, title); err != nil {
					return // ErrClosed
				}
				mu.Lock()
				acked[title] = true
				mu.Unlock()
			}
		}(w)
	}
	time.Sleep(80 * time.Millisecond)
	s.Close()
	wg.Wait()
	s2 := open(t, path, Options{NodeID: "n"})
	have := titlesOf(t, s2)
	for k := range acked {
		if !have[k] {
			t.Fatalf("acknowledged write %s lost", k)
		}
	}
	if len(acked) == 0 {
		t.Fatal("nothing was acknowledged; test proved nothing")
	}
	verifyOK(t, s2)
}

func TestFlushWindowIsALatencyFloor(t *testing.T) {
	timeOne := func(w time.Duration) time.Duration {
		s := gcStore(t, Options{FlushWindow: w})
		_ = gcMk(s, "warm")
		best := time.Hour
		for i := 0; i < 5; i++ {
			t0 := time.Now()
			_ = gcMk(s, fmt.Sprintf("t%d", i))
			if d := time.Since(t0); d < best {
				best = d
			}
		}
		return best
	}
	const w = 40 * time.Millisecond
	if d := timeOne(w); d < w {
		t.Fatalf("lone write took %s with a %s window", d, w)
	}
	if d := timeOne(0); d >= w {
		t.Fatalf("lone write took %s with no window", d)
	}
}

func TestDurabilityPragmasOnTheRealConnection(t *testing.T) {
	s := gcStore(t, Options{})
	var sync_, mode string
	if err := s.db.QueryRow(`PRAGMA synchronous`).Scan(&sync_); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if sync_ != "2" || mode != "wal" {
		t.Fatalf("synchronous=%s journal_mode=%s, want 2 (FULL) and wal", sync_, mode)
	}
}

func TestWALStaysBoundedUnderSustainedWrites(t *testing.T) {
	path := newDB(t)
	s := open(t, path, Options{NodeID: "n"})
	var wg sync.WaitGroup
	n := 400
	if testing.Short() {
		n = 100
	}
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < n; i++ {
				if err := gcMk(s, fmt.Sprintf("wal%d-%d", w, i)); err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	fi, err := os.Stat(path + "-wal")
	if err == nil && fi.Size() > 16<<20 {
		t.Fatalf("WAL grew to %d bytes", fi.Size())
	}
	_ = filepath.Base(path)
}

func TestReadersSeeAcknowledgedWrites(t *testing.T) {
	s := gcStore(t, Options{})
	if s.rdb == nil {
		t.Fatal("file store has no reader pool")
	}
	for i := 0; i < 50; i++ {
		title := fmt.Sprintf("ryw-%d", i)
		tk, _, err := s.Create(context.Background(), task.NewTask{Title: title, Priority: task.PriorityLow})
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(context.Background(), tk.ID)
		if err != nil || got.Title != title {
			t.Fatalf("read-your-write failed at %d: %v %v", i, got.Title, err)
		}
	}
}

func TestMemoryStoreHasNoReaderPool(t *testing.T) {
	s, err := Open(context.Background(), ":memory:", Options{NodeID: "m"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.rdb != nil {
		t.Fatal(":memory: must share the single connection")
	}
	if err := gcMk(s, "x"); err != nil {
		t.Fatal(err)
	}
}

// A request that wrote something and then failed must leave no trace, even though the
// transaction it shared went on to commit: that is what the savepoint is for.
func TestAFailedRequestsPartialWritesAreRolledBackButNeighboursCommit(t *testing.T) {
	s := gcStore(t, Options{FlushWindow: 300 * time.Millisecond})
	var wg sync.WaitGroup
	var failErr, okErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		failErr = s.write(context.Background(), func(tx execer) error {
			if _, err := tx.ExecContext(context.Background(), `CREATE TABLE gc_probe(x)`); err != nil {
				return err
			}
			return errors.New("changed my mind")
		})
	}()
	go func() { defer wg.Done(); okErr = gcMk(s, "neighbour") }()
	wg.Wait()
	if failErr == nil || okErr != nil {
		t.Fatalf("fail=%v ok=%v", failErr, okErr)
	}
	if s.CommitStats().MaxBatch < 2 {
		t.Skip("requests did not share a batch")
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='gc_probe'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("partial write of the failed request survived (n=%d err=%v)", n, err)
	}
	if !titlesOf(t, s)["neighbour"] {
		t.Fatal("neighbour lost")
	}
}
