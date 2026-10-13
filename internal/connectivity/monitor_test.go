package connectivity

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// scripted returns a Prober that plays back verdicts ("g" good, "s" slow,
// "f" fail) one per call, and cancels the context when the script runs out so
// Run ends deterministically, with no real waiting.
func scripted(seq string, cancel context.CancelFunc) Prober {
	var mu sync.Mutex
	i := 0
	return ProberFunc(func(ctx context.Context) Result {
		mu.Lock()
		defer mu.Unlock()
		if i >= len(seq) {
			cancel()
			return Result{Err: ctx.Err()}
		}
		c := seq[i]
		i++
		switch c {
		case 'g':
			return Result{OK: true, Latency: 10 * time.Millisecond, Target: "fake"}
		case 's':
			return Result{OK: true, Latency: 2 * time.Second, Target: "fake"}
		default:
			return Result{Err: errors.New("no route"), Latency: 5 * time.Millisecond, Target: "fake"}
		}
	})
}

// instantWait makes the time between probes zero.
func instantWait(ctx context.Context, _ time.Duration) bool { return ctx.Err() == nil }

func collect(t *testing.T, seq string, mut func(*Config)) ([]Event, *Monitor) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := Config{Prober: scripted(seq, cancel), Wait: instantWait, Thresholds: th}
	if mut != nil {
		mut(&cfg)
	}
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	var evs []Event
	for e := range m.Events() { // closes when Run returns
		evs = append(evs, e)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}
	return evs, m
}

func pairs(evs []Event) [][2]State {
	var out [][2]State
	for _, e := range evs {
		out = append(out, [2]State{e.From, e.To})
	}
	return out
}

func TestEventsFollowTheScript(t *testing.T) {
	// online, then the network drops, then it comes back
	evs, m := collect(t, "ggg"+"fff"+"ggg", nil)
	want := [][2]State{{Unknown, Online}, {Online, Degraded}, {Degraded, Offline}, {Offline, Online}}
	got := pairs(evs)
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events = %v, want %v", got, want)
		}
	}
	if m.State() != Online {
		t.Errorf("final State() = %v, want ONLINE", m.State())
	}
	for _, e := range evs {
		if e.Reason == "" || e.At.IsZero() {
			t.Errorf("event %+v is missing a reason or time", e)
		}
	}
}

func TestNoEventWhenNothingChanges(t *testing.T) {
	evs, _ := collect(t, "gggggggfgggg", nil)
	if len(evs) != 1 || evs[0].To != Online {
		t.Fatalf("events = %v, want only the initial ONLINE", pairs(evs))
	}
}

func TestSlowAnswersDegrade(t *testing.T) {
	evs, _ := collect(t, "gsss", nil)
	got := pairs(evs)
	if len(got) != 2 || got[1] != [2]State{Online, Degraded} {
		t.Fatalf("events = %v, want ONLINE then DEGRADED", got)
	}
}

func TestOnProbeSeesEveryProbe(t *testing.T) {
	var seen []Verdict
	collect(t, "gfsg", func(c *Config) {
		c.OnProbe = func(_ Result, v Verdict, _ State) { seen = append(seen, v) }
	})
	want := []Verdict{Good, Fail, Slow, Good}
	if len(seen) != len(want) {
		t.Fatalf("saw %v", seen)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("saw %v, want %v", seen, want)
		}
	}
}

func TestCancelledProbeIsNotEvidence(t *testing.T) {
	// A probe that is cut short by shutdown returns an error. That must not be
	// counted as a failure (it would flip us OFFLINE on every shutdown).
	ctx, cancel := context.WithCancel(context.Background())
	var calls int
	p := ProberFunc(func(ctx context.Context) Result {
		calls++
		if calls == 1 {
			return Result{OK: true, Latency: time.Millisecond}
		}
		cancel()
		return Result{Err: ctx.Err()}
	})
	m, _ := New(Config{Prober: p, Wait: instantWait, Thresholds: Thresholds{DownAfter: 1, DegradeAfter: 1, UpAfter: 1}})
	go m.Run(ctx)
	for range m.Events() {
	}
	if m.State() != Online {
		t.Errorf("State() = %v after shutdown, want ONLINE (cancelled probe must not count)", m.State())
	}
}

func TestProbeTimeoutCountsAsFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := 0
	p := ProberFunc(func(ctx context.Context) Result {
		n++
		if n > 3 {
			cancel()
			return Result{Err: ctx.Err()}
		}
		<-ctx.Done() // a hung network: only the deadline ends it
		return Result{Err: ctx.Err()}
	})
	m, _ := New(Config{Prober: p, Wait: instantWait, ProbeTimeout: 20 * time.Millisecond, SlowAfter: 10 * time.Millisecond, Thresholds: th})
	go m.Run(ctx)
	var last Event
	for e := range m.Events() {
		last = e
	}
	if last.To != Offline {
		t.Errorf("hung probes ended in %v, want OFFLINE", last.To)
	}
}

func TestShutdownIsPromptEvenMidProbeAndMidWait(t *testing.T) {
	for _, mode := range []string{"mid-probe", "mid-wait"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			started := make(chan struct{}, 1)
			p := ProberFunc(func(ctx context.Context) Result {
				if mode == "mid-probe" {
					started <- struct{}{}
					<-ctx.Done()
					return Result{Err: ctx.Err()}
				}
				started <- struct{}{}
				return Result{OK: true, Latency: time.Millisecond}
			})
			cfg := Config{Prober: p, Interval: time.Hour, ProbeTimeout: time.Hour, SlowAfter: time.Minute}
			m, _ := New(cfg)
			done := make(chan error, 1)
			go func() { done <- m.Run(ctx) }()
			<-started
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("Run = %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Run did not stop within 3s of cancel (it is waiting out an interval or probe)")
			}
			// Drain what was published; the channel must then be closed.
			closed := make(chan struct{})
			go func() {
				for range m.Events() {
				}
				close(closed)
			}()
			select {
			case <-closed:
			case <-time.After(3 * time.Second):
				t.Error("Events must be closed after Run returns")
			}
		})
	}
}

func TestRunTwiceRefused(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m, _ := New(Config{Prober: scripted("g", cancel), Wait: instantWait})
	go m.Run(ctx)
	for range m.Events() {
	}
	if err := m.Run(context.Background()); err == nil {
		t.Error("second Run should be refused")
	}
}

func TestSlowReaderNeverBlocksAndKeepsNewest(t *testing.T) {
	// Flip state many times with nobody reading and a tiny buffer.
	seq := "g"
	for i := 0; i < 20; i++ {
		seq += "fff" + "ggg"
	}
	ctx, cancel := context.WithCancel(context.Background())
	m, _ := New(Config{Prober: scripted(seq, cancel), Wait: instantWait, Thresholds: th, Buffer: 2})
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	select {
	case <-done: // finished although nobody read: the monitor did not block
	case <-time.After(5 * time.Second):
		t.Fatal("the monitor blocked on a reader that is not reading")
	}
	var evs []Event
	for e := range m.Events() {
		evs = append(evs, e)
	}
	if len(evs) != 2 {
		t.Fatalf("kept %d events, want the buffer size 2", len(evs))
	}
	if evs[1].To != Online {
		t.Errorf("newest kept event goes to %v, want ONLINE (the final state)", evs[1].To)
	}
	if m.Dropped() == 0 {
		t.Error("Dropped() should count the discarded events")
	}
}

func TestStateReadableWhileRunning(t *testing.T) {
	// Run with -race: many goroutines call State() while the monitor writes it.
	seq := ""
	for i := 0; i < 200; i++ {
		seq += "gggfff"
	}
	ctx, cancel := context.WithCancel(context.Background())
	m, _ := New(Config{Prober: scripted(seq, cancel), Wait: instantWait, Thresholds: th, Buffer: 1})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				_ = m.State()
				_ = m.Dropped()
			}
		}()
	}
	go func() {
		for range m.Events() {
		}
	}()
	m.Run(ctx)
	wg.Wait()
}

func TestNewValidates(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("a Prober is required")
	}
	if _, err := New(Config{Prober: scripted("", func() {}), Thresholds: Thresholds{DownAfter: 0, DegradeAfter: 1, UpAfter: 1}}); err == nil {
		t.Error("zero threshold should be refused")
	}
	if _, err := New(Config{Prober: scripted("", func() {}), SlowAfter: time.Second, ProbeTimeout: time.Second}); err == nil {
		t.Error("SlowAfter >= ProbeTimeout should be refused")
	}
}
