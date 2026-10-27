package drain

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"offgrid/internal/jobqueue"
)

func TestWorkerBackgroundJobGracefulCancellation(t *testing.T) {
	e := newEnv(t)
	job := e.add("k-bg-cancel", KindDeepThink)

	jobStarted := make(chan struct{})
	spec := Spec{
		Background: true,
		Handle: func(ctx context.Context, j jobqueue.Job, p Progress) (string, error) {
			close(jobStarted)
			// Hang until context is cancelled
			<-ctx.Done()
			return "", ctx.Err()
		},
	}

	w, err := New(Config{
		Queue:        e.q,
		Specs:        map[string]Spec{KindDeepThink: spec},
		Concurrency:  2,
		Poll:         10 * time.Millisecond,
		Lease:        time.Minute,
		ExitWhenIdle: false,
		OnEvent:      e.record,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		_ = w.Run(ctx)
	}()

	// Wait until the background job is claimed and running
	select {
	case <-jobStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for background job to start")
	}

	// Now stop the worker
	cancel()

	select {
	case <-workerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for worker to shut down gracefully")
	}

	// The job must have been released back to the queue (not dead, not permanently claimed)
	eventually(t, "job released back to queued status", 3*time.Second, func() bool {
		j := e.get(job.ID)
		return j.Status == jobqueue.Queued && e.count(EvReleased) == 1
	})
}

func TestWorkerIdleGateAllowsForegroundJobsWhenBusy(t *testing.T) {
	e := newEnv(t)
	fg1 := e.add("k-fg-1", KindInterpret)
	fg2 := e.add("k-fg-2", KindInterpret)
	bgJob := e.add("k-bg-slow", KindDeepThink)

	fgHandled := 0
	bgHandled := 0

	specs := map[string]Spec{
		KindInterpret: {
			Background: false,
			Handle: func(ctx context.Context, j jobqueue.Job, p Progress) (string, error) {
				fgHandled++
				return "fast answer", nil
			},
		},
		KindDeepThink: {
			Background: true,
			Handle: func(ctx context.Context, j jobqueue.Job, p Progress) (string, error) {
				bgHandled++
				return "slow answer", nil
			},
		},
	}

	w, err := New(Config{
		Queue:        e.q,
		Specs:        specs,
		Concurrency:  2,
		Poll:         10 * time.Millisecond,
		Lease:        time.Minute,
		Idle:         func() bool { return false }, // HOST IS BUSY
		IdleWait:     100 * time.Millisecond,
		ExitWhenIdle: true,
		OnEvent:      e.record,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("worker failed: %v", err)
	}

	// Foreground jobs must be completed
	if fgHandled != 2 {
		t.Fatalf("expected 2 foreground jobs to complete, got %d", fgHandled)
	}
	if e.get(fg1.ID).Status != jobqueue.Done || e.get(fg2.ID).Status != jobqueue.Done {
		t.Fatalf("foreground jobs not marked Done: fg1=%s fg2=%s", e.get(fg1.ID).Status, e.get(fg2.ID).Status)
	}

	// Background job must NOT have run and must still be Queued with 0 attempts
	if bgHandled != 0 {
		t.Fatalf("background job ran despite busy host: %d", bgHandled)
	}
	bj := e.get(bgJob.ID)
	if bj.Status != jobqueue.Queued || bj.Attempts != 0 {
		t.Fatalf("background job state corrupted: status=%s attempts=%d", bj.Status, bj.Attempts)
	}
}

func TestWorkerBackgroundSingleFlightEnforcement(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 4; i++ {
		e.add(string(rune('A'+i)), KindDeepThink)
	}

	var currentFlight atomic.Int32
	var maxObservedFlight atomic.Int32
	var totalCompleted atomic.Int32

	spec := Spec{
		Background: true,
		Handle: func(ctx context.Context, j jobqueue.Job, p Progress) (string, error) {
			cur := currentFlight.Add(1)
			for {
				max := maxObservedFlight.Load()
				if cur <= max || maxObservedFlight.CompareAndSwap(max, cur) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond) // simulate slow inference
			currentFlight.Add(-1)
			totalCompleted.Add(1)
			return "done", nil
		},
	}

	w, err := New(Config{
		Queue:        e.q,
		Specs:        map[string]Spec{KindDeepThink: spec},
		Concurrency:  4, // High worker concurrency, but background jobs MUST run 1 at a time!
		Poll:         10 * time.Millisecond,
		Lease:        time.Minute,
		Idle:         func() bool { return true }, // Quiet machine
		ExitWhenIdle: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("worker failed: %v", err)
	}

	if totalCompleted.Load() != 4 {
		t.Fatalf("expected 4 jobs completed, got %d", totalCompleted.Load())
	}
	if max := maxObservedFlight.Load(); max != 1 {
		t.Fatalf("CRITICAL: background flight exceeded 1! Max observed simultaneous slow jobs: %d", max)
	}
}
