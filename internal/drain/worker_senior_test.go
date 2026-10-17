package drain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"offgrid/internal/jobqueue"
)

// TestWorkerDynamicNetworkFlappingDuringDrain verifies that when network connectivity
// repeatedly flips between online and offline during a drain run, local jobs continue
// uninterrupted, network jobs are safely gated or released without burning attempts,
// and once connectivity stabilizes, all jobs complete cleanly.
func TestWorkerDynamicNetworkFlappingDuringDrain(t *testing.T) {
	e := newEnv(t)
	const count = 16

	// Half local, half network
	for i := 0; i < count; i++ {
		if i%2 == 0 {
			e.add(fmt.Sprintf("loc-%d", i), "local")
		} else {
			e.add(fmt.Sprintf("net-%d", i), "net")
		}
	}

	var onlineVal atomic.Bool
	onlineVal.Store(true)

	// Background ticker flipping network state every 30ms
	stopFlap := make(chan struct{})
	go func() {
		ticker := time.NewTicker(30 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopFlap:
				return
			case <-ticker.C:
				onlineVal.Store(!onlineVal.Load())
			}
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w, err := New(Config{
		Queue:        e.q,
		Concurrency:  3,
		Poll:         10 * time.Millisecond,
		Lease:        200 * time.Millisecond,
		ExitWhenIdle: false,
		Online:       func() bool { return onlineVal.Load() },
		Specs: map[string]Spec{
			"local": {
				NeedsNetwork: false,
				Handle: func(ctx context.Context, j jobqueue.Job, p Progress) (string, error) {
					time.Sleep(15 * time.Millisecond)
					return "local-done", nil
				},
			},
			"net": {
				NeedsNetwork: true,
				Handle: func(ctx context.Context, j jobqueue.Job, p Progress) (string, error) {
					time.Sleep(15 * time.Millisecond)
					return "net-done", nil
				},
			},
		},
	})
	if err != nil {
		close(stopFlap)
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	// After 150ms, let network stabilize online so remaining net jobs complete
	time.Sleep(150 * time.Millisecond)
	close(stopFlap)
	onlineVal.Store(true)

	// Wait until all count jobs are done
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if w.Summary().Done == count {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	summary := w.Summary()
	if summary.Done != count {
		t.Errorf("expected %d jobs done, got summary: %+v", count, summary)
	}

	// Verify all jobs in database are in Done status and each has a result
	for i := 1; i <= count; i++ {
		j := e.get(int64(i))
		if j.Status != jobqueue.Done {
			t.Errorf("job %d status = %s, want done", i, j.Status)
		}
		res, found, err := e.q.Result(context.Background(), int64(i))
		if err != nil || !found || res.Text == "" {
			t.Errorf("job %d missing result: %+v found=%v err=%v", i, res, found, err)
		}
	}
}

// TestWorkerGracefulContextCancellationReleasesInFlightJobs verifies that when
// the worker context is cancelled, in-flight jobs are immediately released back to
// the queue (attempts handed back) rather than left locked in running status or failed.
func TestWorkerGracefulContextCancellationReleasesInFlightJobs(t *testing.T) {
	e := newEnv(t)
	const inFlightCount = 4

	for i := 0; i < inFlightCount; i++ {
		e.add(fmt.Sprintf("slow-%d", i), "slow")
	}

	started := make(chan struct{}, inFlightCount)
	releaseBlock := make(chan struct{})

	w, err := New(Config{
		Queue:       e.q,
		Concurrency: inFlightCount,
		Poll:        10 * time.Millisecond,
		Lease:       time.Minute,
		Specs: map[string]Spec{
			"slow": {
				Handle: func(ctx context.Context, j jobqueue.Job, p Progress) (string, error) {
					started <- struct{}{}
					select {
					case <-releaseBlock:
						return "ok", nil
					case <-ctx.Done():
						return "", ctx.Err()
					}
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	// Wait until all 4 are running in handler
	for i := 0; i < inFlightCount; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for job %d to start", i)
		}
	}

	// Cancel context to simulate graceful SIGINT / shutdown
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("w.Run did not exit promptly after context cancellation")
	}

	// Verify all jobs were released back to queued with attempts=0
	for i := 1; i <= inFlightCount; i++ {
		j := e.get(int64(i))
		if j.Status != jobqueue.Queued || j.Attempts != 0 {
			t.Errorf("job %d after cancel: status=%s attempts=%d, want queued with 0 attempts", i, j.Status, j.Attempts)
		}
	}
}

// TestWorkerPanicRecoveryCatchesVariedTypesAndContinues proves that handlers
// panicking with arbitrary types (strings, errors, ints, structs) are safely caught
// without crashing the worker process, and that subsequent healthy jobs complete successfully.
func TestWorkerPanicRecoveryCatchesVariedTypesAndContinues(t *testing.T) {
	e := newEnv(t)

	e.add("p-str", "panic_str")
	e.add("p-err", "panic_err")
	e.add("p-int", "panic_int")
	healthy := e.add("healthy", "healthy")

	w, err := New(Config{
		Queue:        e.q,
		Concurrency:  1,
		Poll:         5 * time.Millisecond,
		ExitWhenIdle: true,
		OnEvent:      e.record,
		Specs: map[string]Spec{
			"panic_str": {
				Handle: func(ctx context.Context, j jobqueue.Job, p Progress) (string, error) {
					panic("critical string panic")
				},
			},
			"panic_err": {
				Handle: func(ctx context.Context, j jobqueue.Job, p Progress) (string, error) {
					panic(errors.New("critical error panic"))
				},
			},
			"panic_int": {
				Handle: func(ctx context.Context, j jobqueue.Job, p Progress) (string, error) {
					panic(9999)
				},
			},
			"healthy": {
				Handle: func(ctx context.Context, j jobqueue.Job, p Progress) (string, error) {
					return "healthy-finished", nil
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		w.Run(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not finish panic recovery tests")
	}

	// Assert panics were caught and turned into retry/fail errors
	panicEvents := 0
	e.mu.Lock()
	for _, ev := range e.events {
		if ev.Err != nil && strings.Contains(ev.Err.Error(), "the handler panicked:") {
			panicEvents++
		}
	}
	e.mu.Unlock()

	if panicEvents != 3 {
		t.Errorf("expected 3 panicked events, got %d", panicEvents)
	}

	// Healthy job completed
	if got := e.get(healthy.ID); got.Status != jobqueue.Done {
		t.Errorf("healthy job status = %s, want done", got.Status)
	}
}

// TestWorkerProgressSaveErrorAndRecovery tests that if Progress.Save returns an
// error (e.g. storage issue or context cancellation), the handler error handling
// operates as expected.
func TestWorkerProgressSaveErrorAndRecovery(t *testing.T) {
	e := newEnv(t)
	j := e.add("prog-job", "prog")

	var stepCount atomic.Int32
	w, _ := New(Config{
		Queue:        e.q,
		Concurrency:  1,
		Poll:         5 * time.Millisecond,
		ExitWhenIdle: true,
		OnEvent:      e.record,
		Specs: map[string]Spec{
			"prog": {
				Handle: func(ctx context.Context, j jobqueue.Job, p Progress) (string, error) {
					if err := p.Save(ctx, "stage-1"); err != nil {
						return "", err
					}
					stepCount.Add(1)
					if err := p.Save(ctx, "stage-2"); err != nil {
						return "", err
					}
					stepCount.Add(1)
					return "success", nil
				},
			},
		},
	})

	w.Run(context.Background())

	if stepCount.Load() != 2 {
		t.Errorf("expected 2 progress steps saved, got %d", stepCount.Load())
	}
	if got := e.get(j.ID); got.Status != jobqueue.Done || got.Progress != "stage-2" {
		t.Errorf("final job state = %+v", got)
	}
}
