package connectivity

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func status(code int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
	}))
}

func TestHTTPProberAccepts204(t *testing.T) {
	s := status(204)
	defer s.Close()
	r := HTTPProber{URL: s.URL}.Probe(context.Background())
	if !r.OK || r.Err != nil || r.Latency <= 0 || r.Target != s.URL {
		t.Errorf("got %+v", r)
	}
}

func TestHTTPProberRejectsWrongAnswers(t *testing.T) {
	cases := []struct {
		code int
		hint bool
	}{{200, true}, {302, true}, {500, false}, {404, false}, {403, false}}
	for _, c := range cases {
		s := status(c.code)
		r := HTTPProber{URL: s.URL}.Probe(context.Background())
		s.Close()
		if r.OK || r.Err == nil {
			t.Errorf("status %d should fail, got %+v", c.code, r)
			continue
		}
		if got := strings.Contains(r.Err.Error(), "captive portal"); got != c.hint {
			t.Errorf("status %d: captive-portal hint = %v, want %v (%v)", c.code, got, c.hint, r.Err)
		}
	}
}

func TestHTTPProberDoesNotFollowRedirects(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(204)
	}))
	defer target.Close()
	portal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer portal.Close()
	if r := (HTTPProber{URL: portal.URL}).Probe(context.Background()); r.OK {
		t.Error("a redirect to a 204 must not count: it is how captive portals behave")
	}
	if hits.Load() != 0 {
		t.Error("the redirect was followed")
	}
}

func TestHTTPProberExpectOverride(t *testing.T) {
	s := status(200)
	defer s.Close()
	if r := (HTTPProber{URL: s.URL, Expect: 200}).Probe(context.Background()); !r.OK {
		t.Errorf("Expect:200 should accept 200: %+v", r)
	}
}

func TestHTTPProberUnreachable(t *testing.T) {
	s := status(204)
	url := s.URL
	s.Close() // nothing is listening any more
	r := HTTPProber{URL: url}.Probe(context.Background())
	if r.OK || r.Err == nil {
		t.Errorf("a closed port must fail: %+v", r)
	}
}

func TestHTTPProberBadURL(t *testing.T) {
	if r := (HTTPProber{URL: "://nope"}).Probe(context.Background()); r.OK || r.Err == nil {
		t.Errorf("got %+v", r)
	}
}

func TestHTTPProberHonoursDeadline(t *testing.T) {
	release := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer s.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	r := HTTPProber{URL: s.URL}.Probe(ctx)
	if r.OK || !errors.Is(r.Err, context.DeadlineExceeded) {
		t.Errorf("got %+v, want deadline exceeded", r)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %v to give up on a 100ms deadline", d)
	}
}

type fakeResolver struct {
	addrs []string
	err   error
}

func (f fakeResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	return f.addrs, f.err
}

func TestDNSProber(t *testing.T) {
	if r := (DNSProber{Host: "example.com", Resolver: fakeResolver{addrs: []string{"1.2.3.4"}}}).Probe(context.Background()); !r.OK || r.Target != "dns:example.com" {
		t.Errorf("good lookup: %+v", r)
	}
	if r := (DNSProber{Host: "x", Resolver: fakeResolver{err: errors.New("no such host")}}).Probe(context.Background()); r.OK || r.Err == nil {
		t.Errorf("failed lookup: %+v", r)
	}
	if r := (DNSProber{Host: "x", Resolver: fakeResolver{}}).Probe(context.Background()); r.OK || r.Err == nil {
		t.Errorf("empty answer must fail: %+v", r)
	}
}

func okProber(d time.Duration) Prober {
	return ProberFunc(func(ctx context.Context) Result {
		select {
		case <-time.After(d):
			return Result{OK: true, Latency: d, Target: "ok"}
		case <-ctx.Done():
			return Result{Err: ctx.Err(), Target: "ok"}
		}
	})
}

func failProber(msg string) Prober {
	return ProberFunc(func(ctx context.Context) Result { return Result{Err: errors.New(msg), Target: msg} })
}

func TestAnySucceedsIfOneDoes(t *testing.T) {
	r := Any(failProber("a down"), okProber(10*time.Millisecond), failProber("c down")).Probe(context.Background())
	if !r.OK || r.Target != "ok" {
		t.Errorf("got %+v", r)
	}
}

func TestAnyFailsWithEveryReason(t *testing.T) {
	r := Any(failProber("a down"), failProber("b down")).Probe(context.Background())
	if r.OK || r.Err == nil || !strings.Contains(r.Err.Error(), "a down") || !strings.Contains(r.Err.Error(), "b down") {
		t.Errorf("got %+v, want both reasons", r)
	}
}

func TestAnyStopsTheLosers(t *testing.T) {
	stopped := make(chan struct{})
	slow := ProberFunc(func(ctx context.Context) Result {
		<-ctx.Done() // only returns when Any cancels it
		close(stopped)
		return Result{Err: ctx.Err()}
	})
	start := time.Now()
	r := Any(slow, okProber(5*time.Millisecond)).Probe(context.Background())
	if !r.OK {
		t.Fatalf("got %+v", r)
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("the losing probe was left running")
	}
	if time.Since(start) > time.Second {
		t.Error("Any waited for the slow target instead of returning the first success")
	}
}

func TestAnyWithNoTargets(t *testing.T) {
	if r := Any().Probe(context.Background()); r.OK || r.Err == nil {
		t.Errorf("got %+v", r)
	}
}

// End to end with a real server and real time: the server answers, then it
// is shut down (as if the network vanished), then a new one appears on the
// same address (the network returns). No fakes except the thresholds, which
// are tight so the test is quick.
func TestEndToEndAgainstARealServer(t *testing.T) {
	var up atomic.Bool
	up.Store(true)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(204)
	}))
	defer s.Close()

	m, err := New(Config{
		Prober:       HTTPProber{URL: s.URL},
		Interval:     15 * time.Millisecond,
		ProbeTimeout: 500 * time.Millisecond,
		SlowAfter:    200 * time.Millisecond,
		Thresholds:   Thresholds{DownAfter: 3, DegradeAfter: 2, UpAfter: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()

	waitFor := func(want State) {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for {
			select {
			case e, ok := <-m.Events():
				if !ok {
					t.Fatalf("events closed while waiting for %v", want)
				}
				if e.To == want {
					return
				}
			case <-deadline:
				t.Fatalf("never reached %v (state is %v)", want, m.State())
			}
		}
	}
	waitFor(Online)
	up.Store(false)
	waitFor(Offline)
	up.Store(true)
	waitFor(Online)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("monitor did not stop")
	}
}
