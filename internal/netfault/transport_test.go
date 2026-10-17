package netfault

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// okBase answers 204 instantly and counts how many requests really got through.
func okBase(count *atomic.Int64) http.RoundTripper {
	return rtFunc(func(r *http.Request) (*http.Response, error) {
		count.Add(1)
		return &http.Response{StatusCode: 204, Body: http.NoBody, Request: r}, nil
	})
}

// recorder is a fake Sleep: it records the delays and never waits.
type recorder struct {
	mu sync.Mutex
	ds []time.Duration
}

func (r *recorder) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	r.ds = append(r.ds, d)
	r.mu.Unlock()
	return nil
}

func mustNew(t *testing.T, cfg Config) *Transport {
	t.Helper()
	tr, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func get(t *testing.T, rt http.RoundTripper, ctx context.Context) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, "GET", "http://example.invalid/", nil)
	if err != nil {
		t.Fatal(err)
	}
	return rt.RoundTrip(req)
}

func TestZeroConfigInjectsNothing(t *testing.T) {
	var n atomic.Int64
	tr := mustNew(t, Config{Base: okBase(&n)})
	for i := 0; i < 50; i++ {
		resp, err := get(t, tr, context.Background())
		if err != nil || resp.StatusCode != 204 {
			t.Fatalf("request %d: %v %v", i, resp, err)
		}
	}
	if s := tr.Stats(); s.Requests != 50 || s.Passed != 50 || s.Dropped != 0 || s.Partitioned != 0 {
		t.Errorf("stats = %+v", s)
	}
	if n.Load() != 50 {
		t.Errorf("base saw %d", n.Load())
	}
}

func TestRefusePartitionFailsFastAndNeverReachesTheNetwork(t *testing.T) {
	var n atomic.Int64
	var sw Switch
	sw.Set(true)
	tr := mustNew(t, Config{Base: okBase(&n), Partitioned: sw.Source()})

	start := time.Now()
	_, err := get(t, tr, context.Background())
	if time.Since(start) > 200*time.Millisecond {
		t.Errorf("refuse mode took %s; it must fail at once", time.Since(start))
	}
	if !errors.Is(err, ErrPartitioned) {
		t.Fatalf("err = %v, want ErrPartitioned", err)
	}
	if !strings.Contains(err.Error(), "injected") {
		t.Errorf("error %q must say it is injected, so nobody blames the real network", err)
	}
	var ne net.Error
	if !errors.As(err, &ne) || ne.Timeout() {
		t.Errorf("refused partition should be a non-timeout net.Error, got %v", err)
	}
	if n.Load() != 0 {
		t.Errorf("a partitioned request reached the real transport")
	}
	if s := tr.Stats(); s.Partitioned != 1 || s.Passed != 0 {
		t.Errorf("stats = %+v", s)
	}
}

func TestPartitionCanBeFlippedWhileRunning(t *testing.T) {
	var n atomic.Int64
	var sw Switch
	tr := mustNew(t, Config{Base: okBase(&n), Partitioned: sw.Source()})
	ok := func() bool { _, err := get(t, tr, context.Background()); return err == nil }

	if !ok() {
		t.Fatal("should pass before the partition")
	}
	sw.Set(true)
	if ok() {
		t.Fatal("should fail during the partition")
	}
	sw.Set(false)
	if !ok() {
		t.Fatal("should pass after the partition heals")
	}
}

func TestBlackholeEndsOnlyAtTheCallersDeadline(t *testing.T) {
	var n atomic.Int64
	var sw Switch
	sw.Set(true)
	tr := mustNew(t, Config{Base: okBase(&n), Partitioned: sw.Source(), Mode: Blackhole})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := get(t, tr, ctx)
	el := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller's deadline", err)
	}
	if el < 50*time.Millisecond || el > 500*time.Millisecond {
		t.Errorf("blackhole held for %s, want about 60ms", el)
	}
	if n.Load() != 0 {
		t.Error("blackholed request reached the network")
	}
}

func TestHangIsASafetyNetForCallersWithoutADeadline(t *testing.T) {
	var sw Switch
	sw.Set(true)
	rec := &recorder{}
	tr := mustNew(t, Config{Partitioned: sw.Source(), Mode: Blackhole, Hang: 30 * time.Second, Sleep: rec.sleep})
	_, err := get(t, tr, context.Background())
	var ne net.Error
	if !errors.Is(err, ErrPartitioned) || !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("err = %v, want a timeout-flavoured ErrPartitioned", err)
	}
	if len(rec.ds) != 1 || rec.ds[0] != 30*time.Second {
		t.Errorf("held for %v, want [30s]", rec.ds)
	}
}

func TestLatencyAndJitterAreAddedAndBounded(t *testing.T) {
	var n atomic.Int64
	rec := &recorder{}
	tr := mustNew(t, Config{Base: okBase(&n), Latency: 100 * time.Millisecond, Jitter: 40 * time.Millisecond, Seed: 7, Sleep: rec.sleep})
	for i := 0; i < 200; i++ {
		if _, err := get(t, tr, context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	lo, hi := time.Hour, time.Duration(0)
	for _, d := range rec.ds {
		lo, hi = min(lo, d), max(hi, d)
	}
	if lo < 100*time.Millisecond || hi > 140*time.Millisecond {
		t.Errorf("delays span %s..%s, want within 100ms..140ms", lo, hi)
	}
	if hi-lo < 20*time.Millisecond {
		t.Errorf("jitter barely varies (%s..%s): is it applied?", lo, hi)
	}
}

func TestLatencyWithoutJitterIsExact(t *testing.T) {
	rec := &recorder{}
	var n atomic.Int64
	tr := mustNew(t, Config{Base: okBase(&n), Latency: 250 * time.Millisecond, Sleep: rec.sleep})
	get(t, tr, context.Background())
	if len(rec.ds) != 1 || rec.ds[0] != 250*time.Millisecond {
		t.Errorf("delays = %v", rec.ds)
	}
}

func TestNoLatencyMeansNoSleep(t *testing.T) {
	rec := &recorder{}
	var n atomic.Int64
	tr := mustNew(t, Config{Base: okBase(&n), Sleep: rec.sleep})
	get(t, tr, context.Background())
	if len(rec.ds) != 0 {
		t.Errorf("slept %v with no latency configured", rec.ds)
	}
}

func TestCancelDuringLatencyStopsBeforeTheNetwork(t *testing.T) {
	var n atomic.Int64
	tr := mustNew(t, Config{Base: okBase(&n), Latency: 5 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := get(t, tr, ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > time.Second {
		t.Error("latency sleep ignored the context")
	}
	if n.Load() != 0 {
		t.Error("request reached the network after the caller gave up")
	}
}

func TestAlreadyCancelledRequestFailsAtOnce(t *testing.T) {
	var n atomic.Int64
	tr := mustNew(t, Config{Base: okBase(&n)})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := get(t, tr, ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
	if n.Load() != 0 {
		t.Error("cancelled request was sent")
	}
}

// dropPattern runs n requests and returns which were dropped.
func dropPattern(t *testing.T, cfg Config, n int) []bool {
	t.Helper()
	var c atomic.Int64
	cfg.Base = okBase(&c)
	cfg.Hang = time.Second
	cfg.Sleep = func(ctx context.Context, d time.Duration) error { return nil }
	tr := mustNew(t, cfg)
	out := make([]bool, n)
	for i := range out {
		_, err := get(t, tr, context.Background())
		out[i] = errors.Is(err, ErrDropped)
	}
	return out
}

func count(b []bool) (n int) {
	for _, v := range b {
		if v {
			n++
		}
	}
	return
}

func TestDropExtremes(t *testing.T) {
	if c := count(dropPattern(t, Config{DropPercent: 0, Seed: 1}, 1000)); c != 0 {
		t.Errorf("0%% dropped %d", c)
	}
	if c := count(dropPattern(t, Config{DropPercent: 100, Seed: 1}, 1000)); c != 1000 {
		t.Errorf("100%% dropped %d, want all", c)
	}
}

func TestDropRateIsAboutRight(t *testing.T) {
	const n = 20000
	for _, pct := range []float64{5, 30, 70} {
		got := float64(count(dropPattern(t, Config{DropPercent: pct, Seed: 42}, n))) / n * 100
		if got < pct-2 || got > pct+2 {
			t.Errorf("drop %v%%: measured %.1f%%", pct, got)
		}
	}
}

func TestSameSeedSamePatternDifferentSeedDiffers(t *testing.T) {
	a := dropPattern(t, Config{DropPercent: 30, Seed: 5}, 500)
	b := dropPattern(t, Config{DropPercent: 30, Seed: 5}, 500)
	c := dropPattern(t, Config{DropPercent: 30, Seed: 6}, 500)
	same := func(x, y []bool) bool {
		for i := range x {
			if x[i] != y[i] {
				return false
			}
		}
		return true
	}
	if !same(a, b) {
		t.Error("same seed produced different drops: runs are not reproducible")
	}
	if same(a, c) {
		t.Error("different seeds produced identical drops")
	}
}

func TestChangingLatencyDoesNotChangeWhichRequestsDrop(t *testing.T) {
	base := dropPattern(t, Config{DropPercent: 30, Seed: 9}, 500)
	withLatency := dropPattern(t, Config{DropPercent: 30, Seed: 9, Latency: time.Second, Jitter: time.Second}, 500)
	for i := range base {
		if base[i] != withLatency[i] {
			t.Fatalf("request %d: drop decision changed when only latency changed", i)
		}
	}
}

func TestPartitionWinsOverDropAndCountsSeparately(t *testing.T) {
	var sw Switch
	sw.Set(true)
	var n atomic.Int64
	tr := mustNew(t, Config{Base: okBase(&n), Partitioned: sw.Source(), DropPercent: 100})
	_, err := get(t, tr, context.Background())
	if !errors.Is(err, ErrPartitioned) {
		t.Errorf("err = %v", err)
	}
	if s := tr.Stats(); s.Partitioned != 1 || s.Dropped != 0 {
		t.Errorf("stats = %+v", s)
	}
}

func TestBrokenControlSourceFailsLoudly(t *testing.T) {
	var n atomic.Int64
	tr := mustNew(t, Config{Base: okBase(&n), Partitioned: func() (bool, error) { return false, errors.New("boom") }})
	_, err := get(t, tr, context.Background())
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	if n.Load() != 0 {
		t.Error("request went through even though the control source was broken")
	}
}

type closeTracker struct {
	io.Reader
	closed atomic.Bool
}

func (c *closeTracker) Close() error { c.closed.Store(true); return nil }

func TestRequestBodyIsClosedWhenNotSent(t *testing.T) {
	var sw Switch
	sw.Set(true)
	tr := mustNew(t, Config{Partitioned: sw.Source()})
	body := &closeTracker{Reader: strings.NewReader("x")}
	req, _ := http.NewRequest("POST", "http://example.invalid/", body)
	tr.RoundTrip(req)
	if !body.closed.Load() {
		t.Error("RoundTripper contract: the body must be closed even when the request is not sent")
	}
}

func TestValidation(t *testing.T) {
	bad := []Config{
		{Latency: -1}, {Jitter: -1}, {Hang: -1}, {DropPercent: -1}, {DropPercent: 100.5},
	}
	for _, c := range bad {
		if _, err := New(c); err == nil {
			t.Errorf("New(%+v) accepted", c)
		}
	}
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]PartitionMode{"": Refuse, "refuse": Refuse, "blackhole": Blackhole} {
		if got, err := ParseMode(in); err != nil || got != want {
			t.Errorf("ParseMode(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := ParseMode("hang"); err == nil {
		t.Error("accepted unknown mode")
	}
}

func TestConcurrentUse(t *testing.T) {
	var n atomic.Int64
	var sw Switch
	tr := mustNew(t, Config{Base: okBase(&n), Partitioned: sw.Source(), DropPercent: 20, Hang: time.Millisecond, Jitter: time.Microsecond, Latency: time.Microsecond, Seed: 3})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if i%50 == 0 {
					sw.Set(g%2 == 0)
				}
				get(t, tr, context.Background())
			}
		}(g)
	}
	wg.Wait()
	s := tr.Stats()
	if s.Requests != 1600 || s.Passed+s.Dropped+s.Partitioned != 1600 {
		t.Errorf("stats do not add up: %+v", s)
	}
}

// With a real http.Client and a real server, an injected fault surfaces the
// way a real one would, and a healthy network is untouched.
func TestWithRealClientAndServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer srv.Close()
	var sw Switch
	tr := mustNew(t, Config{Partitioned: sw.Source()})
	c := &http.Client{Transport: tr, Timeout: 2 * time.Second}

	resp, err := c.Get(srv.URL)
	if err != nil || resp.StatusCode != 204 {
		t.Fatalf("healthy: %v %v", resp, err)
	}
	resp.Body.Close()

	sw.Set(true)
	_, err = c.Get(srv.URL)
	if !errors.Is(err, ErrPartitioned) {
		t.Fatalf("partitioned: err = %v (errors.Is must see through the client's url.Error)", err)
	}
	sw.Set(false)
	resp, err = c.Get(srv.URL)
	if err != nil {
		t.Fatalf("healed: %v", err)
	}
	resp.Body.Close()
}
