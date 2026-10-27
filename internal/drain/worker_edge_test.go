package drain

import (
	"context"
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
