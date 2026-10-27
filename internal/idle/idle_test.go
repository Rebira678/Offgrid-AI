package idle

import (
	"bufio"
	"errors"
	"strings"
	"testing"
	"time"
)

func scan(s string) *bufio.Scanner { return bufio.NewScanner(strings.NewReader(s)) }

func TestParseProcStat(t *testing.T) {
	c, err := ParseProcStat(scan("cpu0 1 1 1 1 1 1 1 1\ncpu  100 10 50 800 40 5 5 0 0 0\nintr 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	// idle = idle(800) + iowait(40); total = 100+10+50+800+40+5+5+0
	if c.Idle != 840 || c.Total != 1010 {
		t.Fatalf("%+v", c)
	}
	for _, bad := range []string{"", "cpu 1 2\n", "cpu a b c d e\n", "intr 1\n"} {
		if _, err := ParseProcStat(scan(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestReadProcStatWorksHereOrSaysWhy(t *testing.T) {
	c, err := ReadProcStat()
	if err != nil {
		t.Skip("no /proc/stat on this platform:", err)
	}
	if c.Total == 0 || c.Idle > c.Total {
		t.Fatalf("%+v", c)
	}
}

type fakeCPU struct {
	c   Counters
	now time.Time
	err error
}

func (f *fakeCPU) read() (Counters, error) { return f.c, f.err }
func (f *fakeCPU) advance(d time.Duration, busy, total uint64) {
	f.now = f.now.Add(d)
	f.c.Total += total
	f.c.Idle += total - busy
}

func newSampler(f *fakeCPU) *Sampler {
	return &Sampler{Read: f.read, Now: func() time.Time { return f.now }, MaxBusy: 0.5, Window: time.Second}
}

func TestSamplerSaysQuietOnlyWhenTheCPUWasMostlyIdle(t *testing.T) {
	f := &fakeCPU{now: time.Unix(1000, 0)}
	s := newSampler(f)
	if s.Quiet() {
		t.Fatal("the first look knows nothing about the machine: it must not start slow work")
	}
	f.advance(2*time.Second, 40, 100) // 40% busy
	if !s.Quiet() {
		t.Fatal("40% busy should be quiet")
	}
	f.advance(2*time.Second, 90, 100) // 90% busy
	if s.Quiet() {
		t.Fatal("90% busy should not be quiet")
	}
	if b := s.Busy(); b < 0.89 || b > 0.91 {
		t.Fatalf("busy = %v", b)
	}
	f.advance(2*time.Second, 50, 100) // exactly the limit counts as quiet
	if !s.Quiet() {
		t.Fatal("exactly MaxBusy should be quiet")
	}
}

func TestSamplerRepeatsTheVerdictInsideTheWindow(t *testing.T) {
	f := &fakeCPU{now: time.Unix(1000, 0)}
	s := newSampler(f)
	s.Quiet() // baseline
	f.advance(2*time.Second, 95, 100)
	if s.Quiet() {
		t.Fatal("expected busy")
	}
	// The machine goes quiet, but we look again within the window: the old verdict stands.
	f.advance(100*time.Millisecond, 0, 100)
	if s.Quiet() {
		t.Fatal("a look inside the window must repeat the verdict")
	}
	f.advance(2*time.Second, 0, 100)
	if !s.Quiet() {
		t.Fatal("after the window the new measurement counts")
	}
}

func TestSamplerFailsOpenAndIgnoresCountersThatMakeNoSense(t *testing.T) {
	f := &fakeCPU{now: time.Unix(1000, 0), err: errors.New("no /proc")}
	s := newSampler(f)
	if !s.Quiet() {
		t.Fatal("an unreadable CPU must not block work forever")
	}
	f.err = nil
	f.c = Counters{Idle: 10, Total: 100}
	s.Quiet() // baseline
	f.advance(2*time.Second, 99, 100)
	if s.Quiet() {
		t.Fatal("expected busy")
	}
	// counters reset (reboot / wrap): keep the old verdict instead of inventing one
	f.now = f.now.Add(2 * time.Second)
	f.c = Counters{Idle: 1, Total: 2}
	if s.Quiet() {
		t.Fatal("a counter that went backwards must keep the previous verdict")
	}
	// counters did not move at all
	f.now = f.now.Add(2 * time.Second)
	if s.Quiet() {
		t.Fatal("counters that did not move must keep the previous verdict")
	}
}

func TestSamplerIgnoresACounterPairWhereIdleGrewMoreThanTotal(t *testing.T) {
	f := &fakeCPU{now: time.Unix(1000, 0)}
	s := newSampler(f)
	s.Quiet() // baseline: not yet known
	f.advance(2*time.Second, 90, 100)
	if s.Quiet() {
		t.Fatal("expected busy")
	}
	// idle advanced by 50 while the total advanced by only 10: impossible, so no information
	f.now = f.now.Add(2 * time.Second)
	f.c.Idle += 50
	f.c.Total += 10
	if s.Quiet() {
		t.Fatal("an impossible reading must keep the previous verdict, not report a quiet machine")
	}
}
