package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"offgrid/internal/governor"
	"offgrid/internal/idle"
	"offgrid/internal/llm"
)

func startNode(t *testing.T, args ...string) (stop func(), code int, stderr string) {
	t.Helper()
	old := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(old) })
	os.Unsetenv("GOMEMLIMIT")
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	nf := addNodeFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	stop, code = nf.start(&b, "test")
	if stop != nil {
		t.Cleanup(stop)
	}
	return stop, code, b.String()
}

func chatOnce(t *testing.T, host string) error {
	t.Helper()
	_, err := chatClient(host).Chat(context.Background(), llm.Request{Model: "m", Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}}}, nil)
	return err
}

func TestTheLightProfileRefusesALocalModelAndSaysWhereToGo(t *testing.T) {
	srv, bodies := classifyServer(t, []string{"m"}, func(string) string { return "{}" })
	stop, code, out := startNode(t, "-profile", "light")
	if code != 0 || !strings.Contains(out, "profile light") || !strings.Contains(out, "no local model") || !strings.Contains(out, "memory limit 96 MiB") {
		t.Fatalf("%d %s", code, out)
	}
	err := chatOnce(t, srv.URL)
	k, ok := governor.IsRefusal(err)
	if !ok || k != governor.KindModelOff || !errors.Is(err, llm.ErrUnavailable) || !strings.Contains(err.Error(), "llamacpp@URL") || len(drain(bodies)) != 0 {
		t.Fatalf("%v", err)
	}
	stop()
	if activeGov.Load() != nil {
		t.Fatal("stop must remove the governor")
	}
	if err := chatOnce(t, srv.URL); err != nil {
		t.Fatalf("a one-shot command with no governor is unchanged: %v", err)
	}
}

func TestTheFullProfileLetsALocalModelThroughAndHasAMemoryLimit(t *testing.T) {
	srv, bodies := classifyServer(t, []string{"m"}, func(string) string { return `{"intent":"unknown"}` })
	_, code, out := startNode(t, "-profile", "full", "-mem-limit", "300", "-model-slots", "2")
	if code != 0 || !strings.Contains(out, "memory limit 300 MiB (profile full)") || !strings.Contains(out, "2 local and 2 remote") {
		t.Fatalf("%d %s", code, out)
	}
	if debug.SetMemoryLimit(-1) != 300<<20 {
		t.Fatal("the limit was not applied")
	}
	if err := chatOnce(t, srv.URL); err != nil || len(drain(bodies)) != 1 {
		t.Fatalf("%v", err)
	}
}

func TestProfileFlagErrorsAndTheEnvironmentDefault(t *testing.T) {
	if _, code, e := startNode(t, "-profile", "heavy"); code != 2 || !strings.Contains(e, "unknown profile") {
		t.Fatalf("%d %s", code, e)
	}
	if _, code, e := startNode(t, "-mem-limit", "-5"); code != 2 || !strings.Contains(e, "must not be negative") {
		t.Fatalf("%d %s", code, e)
	}
	if _, code, e := startNode(t, "-pprof", "0.0.0.0:6060"); code != 2 || !strings.Contains(e, "loopback") || activeGov.Load() != nil {
		t.Fatalf("%d %s", code, e)
	}
	t.Setenv("OFFGRID_PROFILE", "light")
	if _, code, out := startNode(t); code != 0 || !strings.Contains(out, "profile light") {
		t.Fatalf("%d %s", code, out)
	}
	if _, code, out := startNode(t, "-profile", "light", "-model-slots", "3"); code != 0 || !strings.Contains(out, "ignored on the light profile") {
		t.Fatalf("%d %s", code, out)
	}
}

func TestPprofIsOffByDefaultAndOnlyOnLoopbackWhenAsked(t *testing.T) {
	_, code, out := startNode(t)
	if code != 0 || strings.Contains(out, "pprof") {
		t.Fatalf("pprof must be off by default: %s", out)
	}
	stop, code, out := startNode(t, "-pprof", "127.0.0.1:0")
	if code != 0 || !strings.Contains(out, "pprof on http://127.0.0.1:") {
		t.Fatalf("%d %s", code, out)
	}
	addr := strings.TrimSuffix(strings.TrimPrefix(out[strings.Index(out, "http://"):], "http://"), "/debug/pprof/ (this machine only)\n")
	resp, err := http.Get("http://" + addr + "/debug/pprof/goroutine?debug=1")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v", err)
	}
	resp.Body.Close()
	stop()
	if _, err := http.Get("http://" + addr + "/debug/pprof/"); err == nil {
		t.Fatal("stop must close the profiler")
	}
}

func TestTheDrainGateWaitsForTheChargerUnlessAllowed(t *testing.T) {
	// A governor whose power source says "on battery": the gate is shut whatever the CPU does.
	cfg := governor.Defaults(governor.Full)
	g := governor.New(cfg, governor.FixedPower(true), nil)
	activeGov.Store(g)
	t.Cleanup(func() { activeGov.Store(nil) })
	gate := idleGate(1)
	if gate() {
		t.Fatal("background work must wait on battery")
	}
	cfg.AllowOnBattery = true
	allowed := governor.New(cfg, governor.FixedPower(true), nil)
	activeGov.Store(allowed)
	if allowed.OnBatteryBlocked() {
		t.Fatal("-allow-battery must lift the battery rule")
	}
	// With the battery rule lifted only the CPU decides: a fake CPU that is idle, then busy.
	now := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	var c idle.Counters
	busy := false
	gate = gateOf(&idle.Sampler{MaxBusy: 0.5, Window: time.Second,
		Now: func() time.Time { return now },
		Read: func() (idle.Counters, error) {
			c.Total += 100
			if !busy {
				c.Idle += 100
			}
			return c, nil
		}})
	gate() // the first look only takes a baseline
	now = now.Add(2 * time.Second)
	if !gate() {
		t.Fatal("an idle CPU and the battery rule lifted: the gate must open")
	}
	busy = true
	now = now.Add(2 * time.Second)
	if gate() {
		t.Fatal("a busy CPU keeps the gate shut")
	}
	activeGov.Store(g) // and with the battery rule back, an idle CPU is still not enough
	busy = false
	now = now.Add(2 * time.Second)
	if gate() {
		t.Fatal("on battery the gate is shut however idle the CPU is")
	}
}

func TestSyncStatsTakesAMemoryBudget(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.stats")
	var b strings.Builder
	t0 := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&b, `{"at":%q,"role":"serve","goroutines":9,"heap_inuse":1000,"rss_kb":%d}`+"\n", t0.Add(time.Duration(i)*10*time.Second).Format(time.RFC3339), 40000+map[bool]int{true: 60000, false: 0}[i == 2])
	}
	os.WriteFile(p, []byte(b.String()), 0o644)
	var out, errOut bytes.Buffer
	if code := runIn([]string{"sync", "stats", "-max-rss", "100", p}, strings.NewReader(""), &out, &errOut); code != 0 || !strings.Contains(out.String(), "budget      102400 KB") {
		t.Fatalf("%d\n%s%s", code, out.String(), errOut.String())
	}
	out.Reset()
	if code := runIn([]string{"sync", "stats", "-max-rss", "90", p}, strings.NewReader(""), &out, &errOut); code != 1 || !strings.Contains(out.String(), "RSS peaked at 100000 KB, over the budget of 92160 KB") {
		t.Fatalf("%d\n%s", code, out.String())
	}
	if code := runIn([]string{"sync", "stats", "-max-rss", "-1", p}, strings.NewReader(""), &out, &errOut); code != 2 {
		t.Fatal(code)
	}
}
