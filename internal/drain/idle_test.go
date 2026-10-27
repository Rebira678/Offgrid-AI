package drain

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"offgrid/internal/jobqueue"
)

func TestABackgroundJobWaitsForAQuietMachineWithoutUsingAnAttempt(t *testing.T) {
	e := newEnv(t)
	slow := e.add("s1", "slow")
	fast := e.add("f1", "fast")
	var idle atomic.Bool
	e.start(Config{Concurrency: 2, Idle: idle.Load, Specs: map[string]Spec{
		"slow": {Background: true, Handle: okHandler("thought hard").Handle},
		"fast": okHandler("quick"),
	}})
	eventually(t, "foreground job runs on a busy machine", 3*time.Second, allDone(e, fast.ID))
	time.Sleep(150 * time.Millisecond)
	if got := e.get(slow.ID); got.Status != jobqueue.Queued || got.Attempts != 0 {
		t.Fatalf("the background job was touched on a busy machine: %+v", got)
	}
	if !e.has(EvGate, "machine is busy") {
		t.Error("nothing told the person why the slow job is waiting")
	}
	idle.Store(true)
	eventually(t, "background job runs once idle", 3*time.Second, allDone(e, slow.ID))
	if got := e.get(slow.ID); got.Attempts != 1 {
		t.Fatalf("attempts = %d: waiting for idle must not use attempts", got.Attempts)
	}
	if !e.has(EvGate, "machine is quiet") {
		t.Error("no event when the machine went quiet again")
	}
}

func TestABackgroundJobNeverStartsWhileAForegroundJobRuns(t *testing.T) {
	e := newEnv(t)
	fg := e.add("f", "fast")
	bgJob := e.add("b", "slow")
	release := make(chan struct{})
	started := make(chan struct{})
	e.start(Config{Concurrency: 4, Specs: map[string]Spec{
		"fast": {Handle: func(ctx context.Context, j jobqueue.Job, p Progress) (string, error) {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
			}
			return "done", nil
		}},
		"slow": {Background: true, Handle: okHandler("late").Handle},
	}})
	<-started
	time.Sleep(150 * time.Millisecond)
	if got := e.get(bgJob.ID); got.Status != jobqueue.Queued {
		t.Fatalf("background job started beside a foreground job: %+v", got)
	}
	close(release)
	eventually(t, "both finish", 3*time.Second, allDone(e, fg.ID, bgJob.ID))
}

func TestOnlyOneBackgroundJobRunsAtATime(t *testing.T) {
	e := newEnv(t)
	a := e.add("a", "slow")
	b := e.add("b", "slow")
	var cur, peak atomic.Int64
	e.start(Config{Concurrency: 4, Specs: map[string]Spec{
		"slow": {Background: true, Handle: func(ctx context.Context, j jobqueue.Job, p Progress) (string, error) {
			n := cur.Add(1)
			for {
				old := peak.Load()
				if n <= old || peak.CompareAndSwap(old, n) {
					break
				}
			}
			time.Sleep(60 * time.Millisecond)
			cur.Add(-1)
			return "ok", nil
		}},
	}})
	eventually(t, "both done", 5*time.Second, allDone(e, a.ID, b.ID))
	if peak.Load() != 1 {
		t.Fatalf("%d background jobs ran at once, want 1", peak.Load())
	}
}

func TestAForegroundJobQueuedLaterWaitsNotForTheBackgroundGate(t *testing.T) {
	// Idle=false must never hold back foreground work.
	e := newEnv(t)
	f := e.add("f", "fast")
	e.start(Config{Idle: func() bool { return false }, Specs: map[string]Spec{"fast": okHandler("x")}})
	eventually(t, "foreground done", 3*time.Second, allDone(e, f.ID))
}

func TestOnceWaitsForTheMachineToQuietDownThenExitsWithTheJobDone(t *testing.T) {
	e := newEnv(t)
	slow := e.add("s", "slow")
	var quiet atomic.Bool
	go func() { time.Sleep(200 * time.Millisecond); quiet.Store(true) }()
	w, err := New(Config{Queue: e.q, ExitWhenIdle: true, IdleWait: 5 * time.Second, Poll: 10 * time.Millisecond, Idle: quiet.Load,
		OnEvent: e.record, Specs: map[string]Spec{"slow": {Background: true, Handle: okHandler("done").Handle}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	w.Run(ctx)
	if e.get(slow.ID).Status != jobqueue.Done {
		t.Fatal("-once left the slow job behind instead of waiting for the machine to go quiet")
	}
}

func TestOnceGivesUpWaitingForQuietAfterIdleWaitAndLeavesTheJobQueued(t *testing.T) {
	e := newEnv(t)
	slow := e.add("s", "slow")
	w, _ := New(Config{Queue: e.q, ExitWhenIdle: true, IdleWait: 150 * time.Millisecond, Poll: 10 * time.Millisecond,
		Idle: func() bool { return false }, Specs: map[string]Spec{"slow": {Background: true, Handle: okHandler("x").Handle}}})
	start := time.Now()
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	w.Run(ctx)
	if d := time.Since(start); d < 120*time.Millisecond || d > 3*time.Second {
		t.Fatalf("waited %v, want about IdleWait", d)
	}
	if got := e.get(slow.ID); got.Status != jobqueue.Queued || got.Attempts != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestOnceDoesNotWaitWhenNoBackgroundJobIsQueued(t *testing.T) {
	e := newEnv(t)
	e.add("f", "fast")
	w, _ := New(Config{Queue: e.q, ExitWhenIdle: true, IdleWait: time.Minute, Poll: 10 * time.Millisecond,
		Idle: func() bool { return false }, Specs: map[string]Spec{"fast": okHandler("x"), "slow": {Background: true, Handle: okHandler("x").Handle}}})
	start := time.Now()
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	w.Run(ctx)
	if time.Since(start) > 3*time.Second {
		t.Fatal("waited for a quiet machine with nothing slow to run")
	}
}

func TestOnceDoesNotWaitForASlowJobThatIsStillInBackoff(t *testing.T) {
	e := newEnv(t)
	e.addWith("later", "slow", jobqueue.EnqueueOptions{RunAt: time.Now().Add(time.Hour)})
	w, _ := New(Config{Queue: e.q, ExitWhenIdle: true, IdleWait: time.Minute, Poll: 10 * time.Millisecond,
		Idle: func() bool { return false }, Specs: map[string]Spec{"slow": {Background: true, Handle: okHandler("x").Handle}}})
	start := time.Now()
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	w.Run(ctx)
	if time.Since(start) > 3*time.Second {
		t.Fatal("waited for a job that is not due for an hour")
	}
}

func TestAQuietMachineSaysNothingAboutBeingQuiet(t *testing.T) {
	e := newEnv(t)
	s := e.add("s", "slow")
	e.start(Config{Idle: func() bool { return true }, Specs: map[string]Spec{"slow": {Background: true, Handle: okHandler("x").Handle}}})
	eventually(t, "done", 3*time.Second, allDone(e, s.ID))
	if e.has(EvGate, "machine is") {
		t.Error("an always-quiet machine produced idle-gate chatter")
	}
}
