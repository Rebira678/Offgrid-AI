package governor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"offgrid/internal/llm"
	"offgrid/internal/sysinfo"
)

func TestMain(m *testing.M) {
	old := debug.SetMemoryLimit(-1)
	code := m.Run()
	debug.SetMemoryLimit(old)
	os.Exit(code)
}

func req() llm.Request {
	return llm.Request{Model: "m", Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}}}
}

// slow is a model that holds a call until released and counts how many are inside at once.
type slow struct {
	in, peak atomic.Int64
	hold     chan struct{}
}

func (s *slow) Chat(ctx context.Context, _ llm.Request, _ func(string)) (llm.Response, error) {
	n := s.in.Add(1)
	defer s.in.Add(-1)
	for {
		p := s.peak.Load()
		if n <= p || s.peak.CompareAndSwap(p, n) {
			break
		}
	}
	select {
	case <-s.hold:
	case <-ctx.Done():
		return llm.Response{}, ctx.Err()
	}
	return llm.Response{Text: "ok"}, nil
}

func TestProfilesAndAuto(t *testing.T) {
	l, f := Defaults(Light), Defaults(Full)
	if l.LocalSlots != 0 || l.RemoteSlots < 1 || f.LocalSlots != 1 || l.MemLimit >= f.MemLimit || l.Profile != Light || f.Profile != Full {
		t.Fatalf("%+v %+v", l, f)
	}
	cases := []struct {
		info sysinfo.Info
		arch string
		want Profile
	}{
		{sysinfo.Info{MemGiB: 16, Threads: 8}, "amd64", Full},
		{sysinfo.Info{MemGiB: 8, Threads: 4}, "amd64", Full},
		{sysinfo.Info{MemGiB: 7.9, Threads: 8}, "amd64", Light},
		{sysinfo.Info{MemGiB: 16, Threads: 3}, "amd64", Light},
		{sysinfo.Info{MemGiB: 16, Threads: 8}, "arm64", Light}, // a Pi 5 with 16 GiB is still not a model host
		{sysinfo.Info{MemGiB: 16, Threads: 8}, "arm", Light},
		{sysinfo.Info{Threads: 8}, "amd64", Light}, // unknown memory counts as small
	}
	for _, c := range cases {
		if got := Auto(c.info, c.arch); got != c.want {
			t.Errorf("%+v %s: %s, want %s", c.info, c.arch, got, c.want)
		}
	}
	for s, want := range map[string]Profile{"light": Light, "FULL": Full, " light ": Light} {
		if p, err := ParseProfile(s, sysinfo.Info{}); err != nil || p != want {
			t.Errorf("%q: %v %v", s, p, err)
		}
	}
	if p, err := ParseProfile("auto", sysinfo.Info{MemGiB: 64, Threads: 16}); err != nil || (p != Full && p != Light) {
		t.Fatal(p, err)
	}
	if _, err := ParseProfile("heavy", sysinfo.Info{}); err == nil {
		t.Fatal("unknown profile")
	}
}

func TestALightNodeDoesNotRunALocalModelButAsksARemoteOne(t *testing.T) {
	g := New(Defaults(Light), nil, nil)
	m := &slow{hold: make(chan struct{})}
	close(m.hold)
	_, err := g.Wrap(m, false).Chat(context.Background(), req(), nil)
	k, ok := IsRefusal(err)
	if !ok || k != KindModelOff || !errors.Is(err, llm.ErrUnavailable) || !strings.Contains(err.Error(), "full node") || m.peak.Load() != 0 {
		t.Fatalf("%v %v calls %d", k, err, m.peak.Load())
	}
	if r, err := g.Wrap(m, true).Chat(context.Background(), req(), nil); err != nil || r.Text != "ok" {
		t.Fatalf("a remote model is allowed on a light node: %v", err)
	}
}

func TestAtMostTheConfiguredCallsRunAtOnceAndTheRestWaitThenGetThrough(t *testing.T) {
	cfg := Defaults(Full)
	cfg.LocalSlots, cfg.MaxQueue, cfg.MaxWait = 2, 10, 5*time.Second
	g := New(cfg, nil, nil)
	m := &slow{hold: make(chan struct{})}
	c := g.Wrap(m, false)
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := c.Chat(context.Background(), req(), nil); errs <- err }()
	}
	waitFor(t, func() bool { return m.in.Load() == 2 })
	time.Sleep(30 * time.Millisecond)
	if m.in.Load() != 2 {
		t.Fatalf("%d inside, the limit is 2", m.in.Load())
	}
	for i := 0; i < 6; i++ {
		m.hold <- struct{}{}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if m.peak.Load() != 2 {
		t.Fatalf("peak %d, want exactly 2 (the limit must be both respected and used)", m.peak.Load())
	}
}

func TestAFullQueueRefusesAtOnceAndAWaitThatRunsOutRefuses(t *testing.T) {
	cfg := Defaults(Full)
	cfg.LocalSlots, cfg.MaxQueue, cfg.MaxWait = 1, 1, 80*time.Millisecond
	g := New(cfg, nil, nil)
	m := &slow{hold: make(chan struct{})}
	c := g.Wrap(m, false)
	bg := context.Background()
	go c.Chat(bg, req(), nil) // takes the slot and holds it
	waitFor(t, func() bool { return m.in.Load() == 1 })
	waited := make(chan error, 1)
	go func() { _, err := c.Chat(bg, req(), nil); waited <- err }() // takes the one queue place
	time.Sleep(20 * time.Millisecond)
	start := time.Now()
	_, err := c.Chat(bg, req(), nil) // no place left
	if k, _ := IsRefusal(err); k != KindBusy || !strings.Contains(err.Error(), "queue is full") || time.Since(start) > 50*time.Millisecond {
		t.Fatalf("%v after %v", err, time.Since(start))
	}
	err = <-waited // the queued one waits its 80 ms and gives up
	if k, _ := IsRefusal(err); k != KindBusy || !strings.Contains(err.Error(), "no slot freed up within 80ms") || !errors.Is(err, llm.ErrUnavailable) {
		t.Fatalf("%v", err)
	}
	close(m.hold)
	// and after the pressure is gone everything works again, with no slot lost
	waitFor(t, func() bool { return m.in.Load() == 0 })
	for i := 0; i < 3; i++ {
		if _, err := c.Chat(bg, req(), nil); err != nil {
			t.Fatalf("call %d after the burst: %v", i, err)
		}
	}
}

func TestAWaitingCallStopsWhenItsContextEndsAndFreesItsPlace(t *testing.T) {
	cfg := Defaults(Full)
	cfg.LocalSlots, cfg.MaxQueue, cfg.MaxWait = 1, 1, time.Minute
	g := New(cfg, nil, nil)
	m := &slow{hold: make(chan struct{})}
	c := g.Wrap(m, false)
	go c.Chat(context.Background(), req(), nil)
	waitFor(t, func() bool { return m.in.Load() == 1 })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := c.Chat(ctx, req(), nil); done <- err }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	// the queue place came back: another waiter is accepted, not refused as "queue full"
	ctx2, cancel2 := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel2()
	if _, err := c.Chat(ctx2, req(), nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v: the cancelled waiter must free its place", err)
	}
	close(m.hold)
}

func TestAReleasedSlotIsGivenBackWhenTheModelFails(t *testing.T) {
	cfg := Defaults(Full)
	cfg.LocalSlots, cfg.MaxQueue, cfg.MaxWait = 1, 0, time.Second
	g := New(cfg, nil, nil)
	boom := errors.New("boom")
	c := g.Wrap(&llm.Fake{Handler: func(context.Context, llm.Request) (string, error) { return "", boom }}, false)
	for i := 0; i < 5; i++ {
		if _, err := c.Chat(context.Background(), req(), nil); !errors.Is(err, boom) {
			t.Fatalf("call %d: %v: a failing model must not leak the slot", i, err)
		}
	}
}

func TestBackgroundWorkWaitsForTheChargerAndForQuietButAPersonDoesNot(t *testing.T) {
	var onBattery, quiet atomic.Bool
	quiet.Store(true)
	cfg := Defaults(Full)
	g := New(cfg, powerFunc(onBattery.Load), quiet.Load)
	ok := llm.Client(&llm.Fake{Handler: func(context.Context, llm.Request) (string, error) { return "ok", nil }})
	c := g.Wrap(ok, false)
	bg := Background(context.Background())
	if _, err := c.Chat(bg, req(), nil); err != nil {
		t.Fatal(err)
	}
	onBattery.Store(true)
	_, err := c.Chat(bg, req(), nil)
	if k, _ := IsRefusal(err); k != KindBackoff || !strings.Contains(err.Error(), "battery") || !errors.Is(err, llm.ErrUnavailable) {
		t.Fatalf("%v", err)
	}
	if _, err := c.Chat(context.Background(), req(), nil); err != nil {
		t.Fatalf("a person at the keyboard is never held back by the battery rule: %v", err)
	}
	onBattery.Store(false)
	quiet.Store(false)
	if _, err := c.Chat(bg, req(), nil); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("%v", err)
	}
	if _, err := c.Chat(context.Background(), req(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Wrap(ok, true).Chat(bg, req(), nil); err != nil {
		t.Fatalf("background work against a remote model costs this machine nothing: %v", err)
	}
	cfg.AllowOnBattery = true
	onBattery.Store(true)
	quiet.Store(true)
	if _, err := New(cfg, powerFunc(onBattery.Load), quiet.Load).Wrap(ok, false).Chat(bg, req(), nil); err != nil {
		t.Fatalf("AllowOnBattery: %v", err)
	}
	if okk, why := g.BackgroundOK(); okk || why == "" {
		t.Fatalf("%v %q", okk, why)
	}
}

type powerFunc func() bool

func (f powerFunc) OnBattery() bool { return f() }

func TestSysfsPower(t *testing.T) {
	mk := func(files map[string]string) Sysfs {
		root := t.TempDir()
		for p, v := range files {
			full := filepath.Join(root, p)
			os.MkdirAll(filepath.Dir(full), 0o755)
			os.WriteFile(full, []byte(v+"\n"), 0o644)
		}
		return Sysfs{Root: root}
	}
	bat := func(status string) map[string]string {
		return map[string]string{"BAT0/type": "Battery", "BAT0/status": status}
	}
	cases := []struct {
		name  string
		files map[string]string
		want  bool
	}{
		{"discharging, no charger", bat("Discharging"), true},
		{"charging", bat("Charging"), false},
		{"full", bat("Full"), false},
		{"discharging but the charger is online", map[string]string{"BAT0/type": "Battery", "BAT0/status": "Discharging", "AC/type": "Mains", "AC/online": "1"}, false},
		{"charger listed but offline", map[string]string{"BAT0/type": "Battery", "BAT0/status": "Discharging", "AC/type": "Mains", "AC/online": "0"}, true},
		{"a Pi: only a USB supply, online", map[string]string{"usb/type": "USB", "usb/online": "1"}, false},
		{"a laptop on a USB-C charger", map[string]string{"BAT0/type": "Battery", "BAT0/status": "Discharging", "ucsi/type": "USB_C", "ucsi/online": "1"}, false},
		{"a laptop on a USB-PD charger", map[string]string{"BAT0/type": "Battery", "BAT0/status": "Discharging", "pd/type": "USB_PD", "pd/online": "1"}, false},
		{"a wireless charger", map[string]string{"BAT0/type": "Battery", "BAT0/status": "Discharging", "w/type": "Wireless", "w/online": "1"}, false},
		{"a desktop: nothing", map[string]string{}, false},
		{"status in odd case", bat("DISCHARGING"), true},
	}
	for _, c := range cases {
		if got := mk(c.files).OnBattery(); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	if (Sysfs{Root: filepath.Join(t.TempDir(), "missing")}).OnBattery() {
		t.Error("an unreadable power state is not battery")
	}
	if !FixedPower(true).OnBattery() || FixedPower(false).OnBattery() {
		t.Error("FixedPower")
	}
}

func TestTheMemoryLimitIsSetFromTheProfileButAnEnvironmentLimitWins(t *testing.T) {
	old := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(old) })
	t.Setenv("GOMEMLIMIT", "")
	os.Unsetenv("GOMEMLIMIT")
	g := New(Defaults(Light), nil, nil)
	if lim, src := g.Apply(); lim != 96<<20 || src != "profile light" || debug.SetMemoryLimit(-1) != 96<<20 {
		t.Fatalf("%d %s", lim, src)
	}
	debug.SetMemoryLimit(123 << 20)
	t.Setenv("GOMEMLIMIT", "123MiB")
	if lim, src := g.Apply(); src != "GOMEMLIMIT" || lim != 123<<20 || debug.SetMemoryLimit(-1) != 123<<20 {
		t.Fatalf("%d %s: the environment must win", lim, src)
	}
	line := New(Defaults(Full), nil, nil).Start()
	if !strings.Contains(line, "profile full") || !strings.Contains(line, "GOMEMLIMIT") || !strings.Contains(line, "1 local and 2 remote") {
		t.Fatal(line)
	}
	os.Unsetenv("GOMEMLIMIT")
	if line := New(Defaults(Light), nil, nil).Start(); !strings.Contains(line, "memory limit 96 MiB (profile light)") || !strings.Contains(line, "no local model") {
		t.Fatal(line)
	}
}

func TestThePprofEndpointIsLoopbackOnlyAndServesProfiles(t *testing.T) {
	for _, bad := range []string{"0.0.0.0:6060", ":6060", "192.168.1.5:6060", "example.com:80", "6060", "10.0.0.1:1"} {
		if _, err := ServePprof(bad); err == nil {
			t.Errorf("%s must be refused", bad)
		}
	}
	p, err := ServePprof("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/debug/pprof/", "/debug/pprof/goroutine?debug=1", "/debug/pprof/heap", "/debug/pprof/cmdline"} {
		resp, err := http.Get("http://" + p.Addr + path)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s: %v %v", path, err, resp)
		}
		resp.Body.Close()
	}
	// the default mux must stay clean: importing net/http/pprof registers there, and nothing else may serve it
	if h, pat := http.DefaultServeMux.Handler(mustReq("/debug/pprof/")); pat == "/debug/pprof/" {
		_ = h
		t.Log("net/http/pprof registered itself on the default mux (it does that on import); the product must not serve DefaultServeMux")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := http.Get("http://" + p.Addr + "/debug/pprof/"); err == nil {
		t.Fatal("closed means closed")
	}
	for _, ok := range []string{"127.0.0.1:0", "localhost:0", "[::1]:0"} {
		if err := RequireLoopback(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
}

func mustReq(path string) *http.Request {
	r, err := http.NewRequest("GET", "http://x"+path, nil)
	if err != nil {
		panic(fmt.Sprint(err))
	}
	return r
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

func TestRefusedCallsDoNotUseUpQueuePlaces(t *testing.T) {
	cfg := Defaults(Full)
	cfg.LocalSlots, cfg.MaxQueue, cfg.MaxWait = 1, 2, time.Minute
	g := New(cfg, nil, nil)
	m := &slow{hold: make(chan struct{})}
	c := g.Wrap(m, false)
	go c.Chat(context.Background(), req(), nil)
	waitFor(t, func() bool { return m.in.Load() == 1 })
	ctx, cancel := context.WithCancel(context.Background())
	for i := 0; i < 2; i++ {
		go c.Chat(ctx, req(), nil) // both queue places taken
	}
	time.Sleep(40 * time.Millisecond)
	for i := 0; i < 5; i++ {
		if _, err := c.Chat(context.Background(), req(), nil); !errors.Is(err, llm.ErrUnavailable) {
			t.Fatalf("refusal %d: %v", i, err)
		}
	}
	cancel() // the two waiters leave
	time.Sleep(40 * time.Millisecond)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel2()
	if _, err := c.Chat(ctx2, req(), nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v: after a burst of refusals the queue must be as empty as before", err)
	}
	close(m.hold)
}

func TestAZeroSlotLimitSaysSoAtOnceInsteadOfWaiting(t *testing.T) {
	cfg := Defaults(Full)
	cfg.LocalSlots, cfg.MaxWait = 0, time.Minute
	g := New(cfg, nil, nil)
	start := time.Now()
	_, err := g.Wrap(&llm.Fake{}, false).Chat(context.Background(), req(), nil)
	if k, _ := IsRefusal(err); k != KindNoSlots || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("%v after %v", err, time.Since(start))
	}
}
