package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"offgrid/internal/llm"
	"offgrid/internal/netfault"
)

// lockedBuf is a buffer a test can read while a command is still writing to
// it (a plain bytes.Buffer shared like that is a data race).
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func waitFor(t *testing.T, b *lockedBuf, want string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if strings.Contains(b.String(), want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("did not see %q within %s; output so far:\n%s", want, within, b.String())
}

func runFault(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(append([]string{"fault"}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

func TestFaultSetAndShow(t *testing.T) {
	f := filepath.Join(t.TempDir(), "fault")
	t.Setenv("OFFGRID_FAULT", "")

	if code, out, _ := runFault("show", "-file", f); code != 0 || strings.TrimSpace(out) != "ok" {
		t.Errorf("missing file should read as ok: %d %q", code, out)
	}
	code, out, _ := runFault("set", "-file", f, "partition")
	if code != 0 || !strings.Contains(out, "PARTITIONED (injected)") {
		t.Errorf("set partition: %d %q", code, out)
	}
	if _, out, _ := runFault("show", "-file", f); strings.TrimSpace(out) != "partition" {
		t.Errorf("show = %q", out)
	}
	code, out, _ = runFault("set", "-file", f, "ok")
	if code != 0 || !strings.Contains(out, "restored") {
		t.Errorf("set ok: %d %q", code, out)
	}
	if _, out, _ := runFault("show", "-file", f); strings.TrimSpace(out) != "ok" {
		t.Errorf("show = %q", out)
	}
}

func TestFaultUsesEnvFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "fault")
	t.Setenv("OFFGRID_FAULT", f)
	if code, _, e := runFault("set", "partition"); code != 0 {
		t.Fatalf("set: %d %s", code, e)
	}
	if _, out, _ := runFault("show"); strings.TrimSpace(out) != "partition" {
		t.Errorf("show = %q", out)
	}
}

func TestFaultUsageErrors(t *testing.T) {
	t.Setenv("OFFGRID_FAULT", "")
	f := filepath.Join(t.TempDir(), "fault")
	cases := [][]string{
		{},
		{"explode"},
		{"set", "partition"},               // no file
		{"show"},                           // no file
		{"set", "-file", f},                // no state
		{"set", "-file", f, "maybe"},       // bad state
		{"set", "-file", f, "ok", "ok"},    // too many
		{"serve", "extra"},                 // serve takes only flags
		{"set", "-file", f + "/x/y", "ok"}, // exit 1 is also fine, handled below
	}
	for _, c := range cases[:8] {
		if code, _, _ := runFault(c...); code != 2 {
			t.Errorf("fault %v: exit %d, want 2", c, code)
		}
	}
	if code, _, _ := runFault(cases[8]...); code == 0 {
		t.Error("set into a missing folder succeeded")
	}
}

func TestFaultShowRefusesGarbage(t *testing.T) {
	f := filepath.Join(t.TempDir(), "fault")
	putFile(t, f, "partitioned")
	if code, _, e := runFault("show", "-file", f); code != 1 || !strings.Contains(e, "not") {
		t.Errorf("garbage control file: %d %q", code, e)
	}
}

// startServe runs `offgrid fault serve` and returns the URL it printed.
func startServe(t *testing.T, d time.Duration) string {
	t.Helper()
	out, errb := &lockedBuf{}, &lockedBuf{}
	done := make(chan int, 1)
	go func() { done <- run([]string{"fault", "serve", "-for", d.String()}, out, errb) }()
	waitFor(t, out, "listening on http://", 3*time.Second)
	line := strings.TrimSpace(out.String())
	url := strings.TrimPrefix(line, "listening on ")
	t.Cleanup(func() {
		select {
		case code := <-done:
			if code != 0 {
				t.Errorf("serve exit %d: %s", code, errb.String())
			}
		case <-time.After(d + 3*time.Second):
			t.Error("serve did not stop")
		}
	})
	return url
}

func TestFaultServeAnswers204(t *testing.T) {
	url := startServe(t, 600*time.Millisecond)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Errorf("status %d, want 204", resp.StatusCode)
	}
}

func TestFaultServeBadAddress(t *testing.T) {
	if code, _, e := runFault("serve", "-addr", "256.0.0.1:99999"); code != 1 || e == "" {
		t.Errorf("code %d err %q", code, e)
	}
}

func TestNetOnceHealthyThenPartitioned(t *testing.T) {
	s := probeServer(204)
	defer s.Close()
	t.Setenv("OFFGRID_FAULT", "")

	code, out, _ := runNet("-once", "-url", s.URL, "-fault-latency", "1ms")
	if code != 0 || !strings.Contains(out, "FAULT INJECTION ON") {
		t.Errorf("healthy with injection on: %d %q", code, out)
	}
	code, out, _ = runNet("-once", "-url", s.URL, "-fault-partition")
	if code != 1 || !strings.Contains(out, "injected") || !strings.Contains(out, "network is unreachable") {
		t.Errorf("partitioned: %d %q", code, out)
	}
	if !strings.Contains(out, "1 refused or swallowed by the partition") {
		t.Errorf("stats line missing: %q", out)
	}
}

func TestNetWithoutFaultFlagsPrintsNoBanner(t *testing.T) {
	s := probeServer(204)
	defer s.Close()
	_, out, _ := runNet("-once", "-url", s.URL)
	if strings.Contains(out, "FAULT") {
		t.Errorf("banner without fault flags: %q", out)
	}
}

// The environment variable alone must not turn injection on: a leftover
// export from yesterday should not make today's real probes fail.
func TestEnvVarAloneDoesNotInjectAnything(t *testing.T) {
	s := probeServer(204)
	defer s.Close()
	f := filepath.Join(t.TempDir(), "fault")
	putFile(t, f, "partition")
	t.Setenv("OFFGRID_FAULT", f)
	code, out, _ := runNet("-once", "-url", s.URL)
	if code != 0 || strings.Contains(out, "FAULT") {
		t.Errorf("env var alone injected: %d %q", code, out)
	}
	// Naming the file on the command line does.
	code, out, _ = runNet("-once", "-url", s.URL, "-fault-file", f)
	if code != 1 {
		t.Errorf("explicit -fault-file ignored: %d %q", code, out)
	}
}

func TestNetFaultFlagErrors(t *testing.T) {
	s := probeServer(204)
	defer s.Close()
	cases := map[string][]string{
		"bad mode":          {"-fault-mode", "sideways", "-fault-partition"},
		"bad schedule":      {"-fault-schedule", "ok:0s"},
		"drop over 100":     {"-fault-drop", "101", "-fault-latency", "1ms"},
		"negative latency":  {"-fault-latency", "-1s"},
		"injects nothing":   {"-fault-seed", "3"},
		"malformed percent": {"-fault-drop", "lots"},
	}
	for name, extra := range cases {
		code, _, errOut := runNet(append([]string{"-once", "-url", s.URL}, extra...)...)
		if code != 2 || errOut == "" {
			t.Errorf("%s: exit %d, stderr %q (want 2 with a message)", name, code, errOut)
		}
	}
}

func TestNetDropEverythingTimesOut(t *testing.T) {
	s := probeServer(204)
	defer s.Close()
	start := time.Now()
	code, out, _ := runNet("-once", "-url", s.URL, "-timeout", "100ms", "-fault-drop", "100")
	if code != 1 || !strings.Contains(out, "no answer") {
		t.Errorf("%d %q", code, out)
	}
	if time.Since(start) > 3*time.Second {
		t.Error("a dropped probe ignored its timeout")
	}
	if !strings.Contains(out, "1 dropped") {
		t.Errorf("stats: %q", out)
	}
}

// Day 11 "done when": something flips a partition on and off on a timer and
// the system notices, both ways. Real monitor, real probes over a real local
// socket, the control file flipped from outside, the way the demo script does.
func TestFlippingTheControlFileMovesTheMonitor(t *testing.T) {
	s := probeServer(204)
	defer s.Close()
	t.Setenv("OFFGRID_FAULT", "")
	f := filepath.Join(t.TempDir(), "fault")

	out := &lockedBuf{}
	done := make(chan int, 1)
	go func() {
		done <- run([]string{"net", "-url", s.URL, "-interval", "10ms", "-timeout", "200ms", "-slow", "100ms",
			"-down", "3", "-up", "3", "-for", "2s", "-fault-file", f}, out, &lockedBuf{})
	}()
	waitFor(t, out, "-> ONLINE", 2*time.Second)

	if err := netfault.WriteState(f, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, out, "-> OFFLINE", 2*time.Second)

	if err := netfault.WriteState(f, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, out, "OFFLINE -> ONLINE", 2*time.Second)

	if code := <-done; code != 0 {
		t.Errorf("exit %d", code)
	}
	if !strings.Contains(out.String(), "refused or swallowed by the partition") {
		t.Errorf("no stats line:\n%s", out.String())
	}
}

func TestScheduleFlipsByItself(t *testing.T) {
	s := probeServer(204)
	defer s.Close()
	out := &lockedBuf{}
	done := make(chan int, 1)
	go func() {
		done <- run([]string{"net", "-url", s.URL, "-interval", "10ms", "-timeout", "200ms", "-slow", "100ms",
			"-down", "3", "-up", "3", "-for", "2s", "-fault-schedule", "ok:400ms,partition:400ms"}, out, &lockedBuf{})
	}()
	waitFor(t, out, "-> OFFLINE", 2*time.Second)
	waitFor(t, out, "OFFLINE -> ONLINE", 2*time.Second)
	<-done
}

func TestLatencyAboveSlowThresholdDegrades(t *testing.T) {
	s := probeServer(204)
	defer s.Close()
	out := &lockedBuf{}
	done := make(chan int, 1)
	go func() {
		done <- run([]string{"net", "-url", s.URL, "-interval", "10ms", "-timeout", "1s", "-slow", "40ms",
			"-degrade", "2", "-for", "1s", "-fault-latency", "80ms"}, out, &lockedBuf{})
	}()
	waitFor(t, out, "-> DEGRADED", 2*time.Second)
	<-done
	if strings.Contains(out.String(), "-> OFFLINE") {
		t.Errorf("latency alone must degrade, not take the network down:\n%s", out.String())
	}
}

func TestAgentFaultFlagsNeedAutoRouting(t *testing.T) {
	q := routeQueue(t)
	for _, args := range [][]string{
		{"-no-model", "-fault-partition", "hello"},                                           // no -route
		{"-no-model", "-route", "-net", "offline", "-queue", q, "-fault-partition", "hello"}, // forced net
	} {
		code, _, e := runAgentRoute(t, "", args...)
		if code != 2 || !strings.Contains(e, "-fault-") {
			t.Errorf("%v: exit %d, stderr %q", args, code, e)
		}
	}
}

// The whole point: with the network "partitioned" by the switch, the real
// probes see it, the router sees OFFLINE, and the hard request waits.
func TestAgentRoutesOnInjectedPartition(t *testing.T) {
	s := probeServer(204)
	defer s.Close()
	q := routeQueue(t)
	t.Setenv("OFFGRID_FAULT", "")

	code, out, e := runAgentRoute(t, "", "-seed", "-no-model", "-route", "-net", "auto", "-url", s.URL,
		"-queue", q, "-fault-partition",
		"summarize this week's incidents and suggest priorities")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, e)
	}
	for _, want := range []string{"FAULT INJECTION ON", "connectivity: probing", "[deferred to queue,", "You are offline", "partitioned from the start"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func putFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Once injection is on (any -fault-* flag), the control file named by the
// environment is honoured, as the flag's help text says.
func TestEnvControlFileHonouredOnceInjectionIsOn(t *testing.T) {
	s := probeServer(204)
	defer s.Close()
	f := filepath.Join(t.TempDir(), "fault")
	putFile(t, f, "partition")
	t.Setenv("OFFGRID_FAULT", f)
	code, out, _ := runNet("-once", "-url", s.URL, "-fault-latency", "1ms")
	if code != 1 || !strings.Contains(out, "partition set by file") {
		t.Errorf("env control file ignored: %d %q", code, out)
	}
}

func TestBlackholeModeReachesTheCommandLine(t *testing.T) {
	s := probeServer(204)
	defer s.Close()
	code, out, _ := runNet("-once", "-url", s.URL, "-timeout", "150ms", "-fault-partition", "-fault-mode", "blackhole")
	if code != 1 || !strings.Contains(out, "deadline exceeded") || strings.Contains(out, "unreachable") {
		t.Errorf("blackhole should end at the probe's own timeout: %d %q", code, out)
	}
	code, out, _ = runNet("-once", "-url", s.URL, "-timeout", "150ms", "-fault-partition", "-fault-mode", "refuse")
	if code != 1 || !strings.Contains(out, "unreachable") {
		t.Errorf("refuse should fail at once: %d %q", code, out)
	}
}

// builtFrom parses -fault-* flags the way a command does.
func builtFrom(t *testing.T, args ...string) *netfault.Transport {
	t.Helper()
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	o := addFaultFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	tr, _, err := o.build()
	if err != nil || tr == nil {
		t.Fatalf("build: %v %v", tr, err)
	}
	return tr
}

func TestSeedFlagReachesTheTransport(t *testing.T) {
	s := probeServer(204)
	defer s.Close()
	pattern := func(seed string) string {
		tr := builtFrom(t, "-fault-drop", "50", "-fault-seed", seed, "-fault-hang", "1ns")
		c := &http.Client{Transport: tr}
		var b strings.Builder
		for i := 0; i < 64; i++ {
			resp, err := c.Get(s.URL)
			if err != nil {
				b.WriteByte('x')
				continue
			}
			resp.Body.Close()
			b.WriteByte('.')
		}
		return b.String()
	}
	if a, b := pattern("1"), pattern("1"); a != b {
		t.Errorf("same seed, different faults:\n%s\n%s", a, b)
	}
	if a, b := pattern("1"), pattern("2"); a == b {
		t.Errorf("seed flag has no effect: %s", a)
	}
}

// A captive portal answers with a redirect to a login page. The probe client
// must report that redirect, not follow it to wherever it leads.
func TestProbeClientDoesNotFollowRedirects(t *testing.T) {
	target := probeServer(204)
	defer target.Close()
	portal := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusFound))
	defer portal.Close()
	resp, err := probeClient(nil).Get(portal.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("status %d: the redirect was followed", resp.StatusCode)
	}
}

func startModelServer(t *testing.T, delay string, d time.Duration) string {
	t.Helper()
	out, errb := &lockedBuf{}, &lockedBuf{}
	done := make(chan int, 1)
	go func() { done <- run([]string{"fault", "model", "-delay", delay, "-for", d.String()}, out, errb) }()
	waitFor(t, out, "listening on http://", 3*time.Second)
	line := out.String()
	i := strings.Index(line, "http://")
	url := strings.Fields(line[i:])[0]
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(d + 3*time.Second):
			t.Error("model server did not stop")
		}
	})
	return url
}

// The stand-in must speak the protocol the real client speaks, or the demo
// proves nothing. It is exercised with the real llm.Ollama client.
func TestStandInModelSpeaksTheOllamaProtocol(t *testing.T) {
	url := startModelServer(t, "5ms", 1500*time.Millisecond)
	c := newChatClient(url)
	resp, err := c.Chat(context.Background(), llm.Request{Model: "x", Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}}}, nil)
	if err != nil || !strings.Contains(resp.Text, "STAND-IN ANSWER") || !strings.Contains(resp.Text, "not a real language model") {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	// a request that asks for a schema gets valid JSON, so interpret jobs can run in the demo
	resp, err = c.Chat(context.Background(), llm.Request{Model: "x", Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		ResponseSchema: []byte(`{"type":"object"}`)}, nil)
	if err != nil || !json.Valid([]byte(resp.Text)) {
		t.Errorf("schema reply %q err=%v", resp.Text, err)
	}
}

func TestStandInModelStopsWorkWhenTheCallerGoesAway(t *testing.T) {
	url := startModelServer(t, "30s", 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := newChatClient(url).Chat(ctx, llm.Request{Model: "x", Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}}}, nil)
	if err == nil || time.Since(start) > 2*time.Second {
		t.Errorf("err=%v after %s", err, time.Since(start))
	}
}

func TestStandInModelUsageErrors(t *testing.T) {
	for _, args := range [][]string{{"model", "extra"}, {"model", "-delay", "-1s"}} {
		if code, _, e := runFault(args...); code != 2 || e == "" {
			t.Errorf("%v: %d %q", args, code, e)
		}
	}
	if code, _, _ := runFault("model", "-addr", "256.0.0.1:99999"); code != 1 {
		t.Errorf("bad address exit %d", code)
	}
}
