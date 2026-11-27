package governor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"offgrid/internal/llm"
	"offgrid/internal/sysinfo"
)

func TestLimiterHighConcurrencyBurstAndZeroLeaks(t *testing.T) {
	// 2 slots, max queue 5, wait 100ms
	l := newLimiter("stress", 2, 5, 100*time.Millisecond)

	const totalGoroutines = 50
	var wg sync.WaitGroup
	var active, peakActive atomic.Int64
	var succeeded, refusedBusy, canceled atomic.Int64

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	for i := 0; i < totalGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			// Stagger slightly so some get slots, some queue, some get refused
			if id%3 == 0 {
				time.Sleep(time.Duration(id%10) * time.Millisecond)
			}
			release, err := l.acquire(ctx)
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
					canceled.Add(1)
					return
				}
				k, ok := IsRefusal(err)
				if ok && k == KindBusy {
					refusedBusy.Add(1)
					return
				}
				t.Errorf("unexpected error on acquire: %v", err)
				return
			}
			defer release()

			// Concurrency verification inside critical section
			cur := active.Add(1)
			defer active.Add(-1)
			for {
				p := peakActive.Load()
				if cur <= p || peakActive.CompareAndSwap(p, cur) {
					break
				}
			}
			if cur > 2 {
				t.Errorf("critical invariant violated: active %d > slots 2", cur)
			}
			time.Sleep(10 * time.Millisecond)
			succeeded.Add(1)
		}(i)
	}

	wg.Wait()

	if peakActive.Load() > 2 {
		t.Fatalf("peak active %d exceeded slot capacity 2", peakActive.Load())
	}
	if succeeded.Load() == 0 {
		t.Fatal("expected at least some goroutines to succeed")
	}
	if refusedBusy.Load() == 0 {
		t.Fatal("expected queue overflow refusals under 50 callers")
	}

	// Invariant: after all goroutines finish, limiter must be completely free
	if len(l.slots) != 0 {
		t.Errorf("slot channel leaked: len = %d", len(l.slots))
	}
	if w := l.waiting.Load(); w != 0 {
		t.Errorf("waiting counter leaked: waiting = %d", w)
	}

	// Immediate next acquire must succeed instantly with no wait
	rel, err := l.acquire(context.Background())
	if err != nil {
		t.Fatalf("immediate acquire after burst failed: %v", err)
	}
	rel()
}

func TestLimiterImmediateCancellation(t *testing.T) {
	l := newLimiter("cancel", 1, 1, time.Second)

	// Take the only slot
	rel, err := l.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rel()

	// Pre-canceled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = l.acquire(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	// Ensure waiting counter was not leaked
	if w := l.waiting.Load(); w != 0 {
		t.Fatalf("waiting counter leaked on pre-canceled context: %d", w)
	}
}

func TestLimiterNegativeAndZeroSlots(t *testing.T) {
	for _, slots := range []int{-5, 0} {
		l := newLimiter("none", slots, 2, time.Second)
		_, err := l.acquire(context.Background())
		k, ok := IsRefusal(err)
		if !ok || k != KindNoSlots {
			t.Fatalf("slots=%d: expected KindNoSlots, got %v (%v)", slots, k, err)
		}
	}
}

func TestGovernorIsRefusalWrappedVariations(t *testing.T) {
	ref := &Refusal{Kind: KindBusy, Reason: "overloaded"}

	// Direct
	if k, ok := IsRefusal(ref); !ok || k != KindBusy {
		t.Fatalf("direct refusal check failed: %v %v", k, ok)
	}

	// Single wrap
	wrap1 := fmt.Errorf("wrap 1: %w", ref)
	if k, ok := IsRefusal(wrap1); !ok || k != KindBusy {
		t.Fatalf("single wrapped refusal check failed: %v %v", k, ok)
	}

	// Deep wrap
	wrap2 := fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", ref))
	if k, ok := IsRefusal(wrap2); !ok || k != KindBusy {
		t.Fatalf("deeply wrapped refusal check failed: %v %v", k, ok)
	}

	// Unwrap to llm.ErrUnavailable
	if !errors.Is(wrap2, llm.ErrUnavailable) {
		t.Fatalf("wrapped refusal must unwrap to llm.ErrUnavailable")
	}

	// Non-refusal error
	if _, ok := IsRefusal(errors.New("plain error")); ok {
		t.Fatal("plain error should not be recognized as refusal")
	}
}

func TestSysfsPowerCorruptedAndMixedSources(t *testing.T) {
	dir := t.TempDir()

	// 1. Non-existent root returns false (not on battery)
	sMissing := Sysfs{Root: filepath.Join(dir, "no_such_dir")}
	if sMissing.OnBattery() {
		t.Error("non-existent sysfs directory must report OnBattery = false")
	}

	// 2. Empty directory returns false
	emptyDir := filepath.Join(dir, "empty")
	_ = os.MkdirAll(emptyDir, 0o755)
	if (Sysfs{Root: emptyDir}).OnBattery() {
		t.Error("empty sysfs directory must report OnBattery = false")
	}

	// 3. Mixed sources: Discharging battery BUT USB-PD charger is online -> NOT on battery
	mixedDir := filepath.Join(dir, "mixed")
	_ = os.MkdirAll(filepath.Join(mixedDir, "BAT0"), 0o755)
	_ = os.MkdirAll(filepath.Join(mixedDir, "AC0"), 0o755)

	_ = os.WriteFile(filepath.Join(mixedDir, "BAT0", "type"), []byte("Battery\n"), 0o644)
	_ = os.WriteFile(filepath.Join(mixedDir, "BAT0", "status"), []byte("Discharging\n"), 0o644)

	_ = os.WriteFile(filepath.Join(mixedDir, "AC0", "type"), []byte("USB_PD\n"), 0o644)
	_ = os.WriteFile(filepath.Join(mixedDir, "AC0", "online"), []byte("1\n"), 0o644)

	if (Sysfs{Root: mixedDir}).OnBattery() {
		t.Error("online USB_PD charger must prevent OnBattery from being true")
	}

	// 4. Charger unplugged (online = 0) -> now ON battery
	_ = os.WriteFile(filepath.Join(mixedDir, "AC0", "online"), []byte("0\n"), 0o644)
	if !(Sysfs{Root: mixedDir}).OnBattery() {
		t.Error("discharging battery with offline charger must report OnBattery = true")
	}

	// 5. Battery charged/full (status = "Full") with offline charger -> NOT on battery
	_ = os.WriteFile(filepath.Join(mixedDir, "BAT0", "status"), []byte("Full\n"), 0o644)
	if (Sysfs{Root: mixedDir}).OnBattery() {
		t.Error("battery in Full status must report OnBattery = false")
	}

	// 6. Wireless charger online
	_ = os.WriteFile(filepath.Join(mixedDir, "BAT0", "status"), []byte("Discharging\n"), 0o644)
	_ = os.WriteFile(filepath.Join(mixedDir, "AC0", "type"), []byte("Wireless\n"), 0o644)
	_ = os.WriteFile(filepath.Join(mixedDir, "AC0", "online"), []byte("1\n"), 0o644)
	if (Sysfs{Root: mixedDir}).OnBattery() {
		t.Error("online Wireless charger must report OnBattery = false")
	}
}

func TestPprofSecurityRejections(t *testing.T) {
	// Rejection matrix
	insecureAddrs := []string{
		"0.0.0.0:6060",
		"192.168.1.50:6060",
		"10.0.0.1:6060",
		"172.16.0.5:6060",
		"example.com:6060",
		"not-a-port",
		":6060",
	}
	for _, addr := range insecureAddrs {
		if err := RequireLoopback(addr); err == nil {
			t.Errorf("address %q must be rejected as non-loopback", addr)
		}
		if _, err := ServePprof(addr); err == nil {
			t.Errorf("ServePprof(%q) must be rejected", addr)
		}
	}

	// Allowed loopbacks
	validAddrs := []string{
		"localhost:0",
		"127.0.0.1:0",
		"127.0.0.2:0",
		"[::1]:0",
	}
	for _, addr := range validAddrs {
		if err := RequireLoopback(addr); err != nil {
			t.Errorf("valid loopback %q was rejected: %v", addr, err)
		}
	}
}

func TestProfileAutoExtendedMatrix(t *testing.T) {
	tests := []struct {
		name   string
		info   sysinfo.Info
		arch   string
		expect Profile
	}{
		{"armv7l-board", sysinfo.Info{MemGiB: 4, Threads: 4}, "armv7l", Light},
		{"arm64-pi4", sysinfo.Info{MemGiB: 8, Threads: 4}, "arm64", Light},
		{"arm64-pi5-16gb", sysinfo.Info{MemGiB: 16, Threads: 4}, "arm64", Light},
		{"amd64-boundary-below-8gb", sysinfo.Info{MemGiB: 7.99, Threads: 8}, "amd64", Light},
		{"amd64-boundary-exact-8gb", sysinfo.Info{MemGiB: 8.00, Threads: 4}, "amd64", Full},
		{"amd64-boundary-threads-3", sysinfo.Info{MemGiB: 32.0, Threads: 3}, "amd64", Light},
		{"amd64-boundary-threads-4", sysinfo.Info{MemGiB: 16.0, Threads: 4}, "amd64", Full},
		{"riscv64-small", sysinfo.Info{MemGiB: 4, Threads: 2}, "riscv64", Light},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Auto(tt.info, tt.arch)
			if got != tt.expect {
				t.Errorf("Auto(%+v, %q) = %s, want %s", tt.info, tt.arch, got, tt.expect)
			}
		})
	}
}

func TestLimiterZeroQueueImmediateRejection(t *testing.T) {
	// slots = 1, queue = 0 (no waiters allowed)
	l := newLimiter("local", 1, 0, 10*time.Second)

	rel1, err := l.acquire(context.Background())
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}

	// Second acquire must fail immediately with KindBusy (queue is full at 0)
	_, err2 := l.acquire(context.Background())
	if k, ok := IsRefusal(err2); !ok || k != KindBusy {
		t.Fatalf("expected KindBusy on zero-queue limiter, got %v (%v)", k, err2)
	}
	if w := l.waiting.Load(); w != 0 {
		t.Fatalf("waiting count must remain 0, got %d", w)
	}

	rel1()

	// Third acquire succeeds immediately
	rel3, err3 := l.acquire(context.Background())
	if err3 != nil {
		t.Fatalf("acquire after release failed: %v", err3)
	}
	rel3()
}

func TestLimiterContextDoneRacingWithSlotFree(t *testing.T) {
	l := newLimiter("racing", 1, 10, 1*time.Second)

	// Hold the slot
	rel, err := l.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		go func(c context.Context, cf context.CancelFunc) {
			defer wg.Done()
			defer cf()
			_, _ = l.acquire(c)
		}(ctx, cancel)
	}

	time.Sleep(10 * time.Millisecond)
	rel() // free slot while contexts are canceling
	wg.Wait()

	if w := l.waiting.Load(); w != 0 {
		t.Fatalf("waiting counter drifted to %d, want 0", w)
	}
}

func TestGovernorBackgroundContextPropagation(t *testing.T) {
	type testCtxKey struct{}
	parent := context.WithValue(context.Background(), testCtxKey{}, "custom_val")
	bgCtx := Background(parent)

	if !isBackground(bgCtx) {
		t.Fatal("expected isBackground to be true")
	}
	if val, ok := bgCtx.Value(testCtxKey{}).(string); !ok || val != "custom_val" {
		t.Fatalf("parent context value lost: got %v", val)
	}
	if isBackground(parent) {
		t.Fatal("parent context must not report isBackground")
	}
}

type panicMockClient struct{}

func (panicMockClient) Chat(ctx context.Context, req llm.Request, onToken func(string)) (llm.Response, error) {
	if onToken != nil {
		onToken("partial token")
	}
	panic("simulated client panic")
}

func TestGovernorWrapChatStreamingPanicSafety(t *testing.T) {
	cfg := Defaults(Full)
	cfg.LocalSlots = 1
	g := New(cfg, nil, nil)
	c := g.Wrap(panicMockClient{}, false)

	func() {
		defer func() {
			_ = recover()
		}()
		_, _ = c.Chat(context.Background(), llm.Request{}, nil)
	}()

	// The slot MUST be released even if Chat panicked
	okClient := &slow{hold: make(chan struct{}, 1)}
	okClient.hold <- struct{}{}
	cOk := g.Wrap(okClient, false)

	done := make(chan struct{})
	go func() {
		_, err := cOk.Chat(context.Background(), llm.Request{}, nil)
		if err != nil {
			t.Errorf("chat failed: %v", err)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("slot leaked after client panic: next call blocked")
	}
}

func TestPprofIdempotentCloseAndConcurrentClose(t *testing.T) {
	p, err := ServePprof("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- p.Close()
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil && !errors.Is(err, net.ErrClosed) {
			t.Logf("close error: %v", err)
		}
	}
}

func TestProfileConfigMutationSafety(t *testing.T) {
	c1 := Defaults(Light)
	c1.MemLimit = 12345
	c1.LocalSlots = 99

	c2 := Defaults(Light)
	if c2.MemLimit != 96*mib || c2.LocalSlots != 0 {
		t.Fatalf("Defaults(Light) was mutated by caller: %+v", c2)
	}
}

func TestParseProfileBoundaryWhitespaceAndUnicode(t *testing.T) {
	valid := []struct {
		input  string
		expect Profile
	}{
		{"  light  \n", Light},
		{"\t\rFULL\n", Full},
		{"   ", Full}, // empty/whitespace resolves to Auto -> on host Full or Light
		{"auto", Full},
	}
	hostProfile := Auto(sysinfo.Info{MemGiB: 16, Threads: 8}, "amd64")
	for _, tc := range valid {
		p, err := ParseProfile(tc.input, sysinfo.Info{MemGiB: 16, Threads: 8})
		if err != nil {
			t.Errorf("unexpected error for %q: %v", tc.input, err)
		}
		if tc.input == "   " || tc.input == "auto" {
			if p != hostProfile {
				t.Errorf("ParseProfile(%q) = %s, want %s", tc.input, p, hostProfile)
			}
		} else if p != tc.expect {
			t.Errorf("ParseProfile(%q) = %s, want %s", tc.input, p, tc.expect)
		}
	}

	invalid := []string{"superlight", "ultrafull", "pi", "pi4", "server", "123", "none"}
	for _, bad := range invalid {
		if _, err := ParseProfile(bad, sysinfo.Info{}); err == nil {
			t.Errorf("expected error for invalid profile %q", bad)
		}
	}
}
