package hub

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"offgrid/internal/identity"
	"offgrid/internal/syncer"
)

func TestLimiterEdgeCases(t *testing.T) {
	c := &clock{time.Unix(1000, 0)}

	// 1. NaN, Inf, and negative rate/burst fallback to defaults
	lNaN := NewLimiter(math.NaN(), -10, c.now)
	if ok, _ := lNaN.Allow(); !ok {
		t.Fatal("NaN limiter should allow first token with default burst 1")
	}
	if ok, retry := lNaN.Allow(); ok || retry < 500*time.Millisecond {
		t.Fatalf("NaN limiter second allow: ok=%v retry=%v", ok, retry)
	}

	lPosInf := NewLimiter(math.Inf(1), 0, c.now)
	if ok, _ := lPosInf.Allow(); !ok {
		t.Fatal("+Inf limiter should allow first token with default burst 1")
	}

	lNegInf := NewLimiter(math.Inf(-1), 5, c.now)
	if ok, _ := lNegInf.Allow(); !ok {
		t.Fatal("-Inf limiter should allow first token with rate=1, burst=5")
	}

	// 2. Extremely small rate (1e-12) to test duration overflow protection
	lMicro := NewLimiter(1e-12, 1, c.now)
	if ok, _ := lMicro.Allow(); !ok {
		t.Fatal("first token should be allowed")
	}
	ok, retry := lMicro.Allow()
	if ok {
		t.Fatal("second token should be denied")
	}
	if retry <= 0 {
		t.Fatalf("retry duration should be positive and bounded, got %v", retry)
	}

	// 3. Huge clock jumps backwards (100 years in the past)
	lNormal := NewLimiter(5, 5, c.now)
	for i := 0; i < 5; i++ {
		if ok, _ := lNormal.Allow(); !ok {
			t.Fatalf("burst token %d refused", i)
		}
	}
	c.t = c.t.Add(-100 * 365 * 24 * time.Hour)
	if ok, _ := lNormal.Allow(); ok {
		t.Fatal("clock jump to past should not refill tokens")
	}
}

func TestLimiterConcurrentStress(t *testing.T) {
	c := &safeClock{t: time.Unix(2000, 0)}
	rate := 10.0
	burst := 20
	l := NewLimiter(rate, burst, c.now)

	var allowed, denied int64
	var wg sync.WaitGroup
	workers := 50
	callsPerWorker := 100

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < callsPerWorker; j++ {
				ok, _ := l.Allow()
				if ok {
					atomic.AddInt64(&allowed, 1)
				} else {
					atomic.AddInt64(&denied, 1)
				}
			}
		}()
	}
	wg.Wait()

	total := allowed + denied
	if total != int64(workers*callsPerWorker) {
		t.Fatalf("total calls = %d, want %d", total, workers*callsPerWorker)
	}
	if allowed > int64(burst) {
		t.Fatalf("allowed = %d, exceeded burst capacity %d with frozen clock", allowed, burst)
	}
}

func TestHubOptionDefaults(t *testing.T) {
	ctx := context.Background()
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}

	// 1. Missing Dir
	if _, err := New(ctx, Options{Identity: id}); err == nil {
		t.Fatal("expected error for missing Dir")
	}

	// 2. Missing Identity
	if _, err := New(ctx, Options{Dir: t.TempDir()}); err == nil {
		t.Fatal("expected error for missing Identity")
	}

	// 3. Dir is a regular file
	tmpFile := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(tmpFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(ctx, Options{Dir: tmpFile, Identity: id}); err == nil {
		t.Fatal("expected error when Dir is an existing regular file")
	}

	// 4. Default options fallback
	dir := filepath.Join(t.TempDir(), "defaults-hub")
	h, err := New(ctx, Options{
		Dir:           dir,
		Identity:      id,
		RatePerSecond: -10,
		RateBurst:     -5,
		MaxBody:       0,
		MaxGroups:     -1,
	})
	if err != nil {
		t.Fatalf("New with zero/negative options: %v", err)
	}
	defer h.Close()

	if h.opt.RatePerSecond != 20 {
		t.Errorf("RatePerSecond default = %v, want 20", h.opt.RatePerSecond)
	}
	if h.opt.RateBurst != 40 {
		t.Errorf("RateBurst default = %d, want 40", h.opt.RateBurst)
	}
	if h.opt.MaxBody != 16<<20 {
		t.Errorf("MaxBody default = %d, want 16MiB", h.opt.MaxBody)
	}
	if h.opt.MaxGroups != 64 {
		t.Errorf("MaxGroups default = %d, want 64", h.opt.MaxGroups)
	}
	if h.opt.Now == nil {
		t.Error("Now should default to non-nil")
	}
}

func TestHubAddGroupValidationExtremes(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, func(o *Options) { o.MaxGroups = 50 })
	founder := newID(t).ID()

	validNames := []string{
		"a",
		"0",
		"a-1",
		"group-42-test",
		"a" + strings.Repeat("x", 39), // exactly 40 chars
	}
	for _, name := range validNames {
		if err := e.hub.AddGroup(ctx, name, founder); err != nil {
			t.Errorf("valid group name %q rejected: %v", name, err)
		}
	}

	invalidNames := []string{
		"",
		"-start-with-hyphen",
		"Uppercase",
		"spaces in name",
		"my_group",
		"dots.not.allowed",
		"slash/not/allowed",
		"a" + strings.Repeat("x", 40), // 41 chars
	}
	for _, name := range invalidNames {
		if err := e.hub.AddGroup(ctx, name, founder); err == nil {
			t.Errorf("invalid group name %q should be rejected", name)
		}
	}

	validFounders := []string{
		founder,
		"dev_" + strings.Repeat("0", 32),
		"dev_" + strings.Repeat("f", 32),
		"dev_0123456789abcdef0123456789abcdef",
	}
	for i, f := range validFounders {
		gname := fmt.Sprintf("valfound-%d", i)
		e.hub.opt.MaxGroups = 100 // ensure room
		if err := e.hub.AddGroup(ctx, gname, f); err != nil {
			t.Errorf("valid founder %q rejected: %v", f, err)
		}
	}

	invalidFounders := []string{
		"",
		"dev_",
		"node_0123456789abcdef0123456789abcdef",
		"DEV_" + strings.Repeat("a", 32),
		"dev_" + strings.Repeat("a", 31),
		"dev_" + strings.Repeat("a", 33),
		"dev_" + strings.Repeat("g", 32),
		"dev_ 123456789abcdef0123456789abcde",
	}
	for _, f := range invalidFounders {
		if err := e.hub.AddGroup(ctx, "any-valid-name", f); err == nil {
			t.Errorf("invalid founder %q should be rejected", f)
		}
	}
}

func TestHubGroupsSortingAndEmptyState(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)

	// 1. Empty state
	gs, err := e.hub.Groups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if gs == nil || len(gs) != 0 {
		t.Fatalf("expected non-nil empty slice for empty hub, got %+v", gs)
	}

	// 2. Out of order additions
	f := newID(t).ID()
	names := []string{"zebra", "alpha", "middle", "beta"}
	for _, n := range names {
		if err := e.hub.AddGroup(ctx, n, f); err != nil {
			t.Fatal(err)
		}
	}

	gs, err = e.hub.Groups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(gs) != 4 {
		t.Fatalf("got %d groups, want 4", len(gs))
	}
	expected := []string{"alpha", "beta", "middle", "zebra"}
	for i, want := range expected {
		if gs[i].ID != want {
			t.Errorf("group %d: got %s, want %s", i, gs[i].ID, want)
		}
	}
}

func TestHubRoutingSecurityEdgeCases(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	founder := newID(t)
	if err := e.hub.AddGroup(ctx, "target", founder.ID()); err != nil {
		t.Fatal(err)
	}

	// 1. Direct ServeHTTP call with nil TLS
	rec := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/g/target/sync/summary", nil)
	e.hub.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("nil TLS status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), CodeNoSuchGroup) {
		t.Fatalf("body missing %s: %s", CodeNoSuchGroup, rec.Body.String())
	}

	// 2. Direct ServeHTTP with empty PeerCertificates
	rec = httptest.NewRecorder()
	req = req.Clone(context.Background())
	req.TLS = &tls.ConnectionState{}
	e.hub.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("empty certs status = %d, want 404", rec.Code)
	}

	// 3. Unknown paths and prefix variations
	testPaths := []string{
		"/",
		"/g",
		"/g/",
		"/sync/summary",
		"/g/target/../../etc/passwd",
		"/g/unknown-group/sync/summary",
		"/other/target/sync/summary",
	}
	for _, p := range testPaths {
		rec = httptest.NewRecorder()
		r, _ := http.NewRequest("GET", p, nil)
		e.hub.ServeHTTP(rec, r)
		if rec.Code != http.StatusNotFound {
			t.Errorf("path %q status = %d, want 404", p, rec.Code)
		}
	}

	// 4. Rate limit check for non-existent group
	if n := e.hub.Limited("does-not-exist"); n != 0 {
		t.Fatalf("Limited on non-existent group returned %d, want 0", n)
	}
}

func TestHubIdempotentAndConcurrentClose(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	id := newID(t)
	h, err := New(ctx, Options{Dir: dir, Identity: id})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.AddGroup(ctx, "closetest", id.ID()); err != nil {
		t.Fatal(err)
	}

	// Calling Close multiple times in sequence should be completely safe
	h.Close()
	h.Close()

	// Concurrent close stress test
	h2, err := New(ctx, Options{Dir: t.TempDir(), Identity: id})
	if err != nil {
		t.Fatal(err)
	}
	if err := h2.AddGroup(ctx, "conclose", id.ID()); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h2.Close()
		}()
	}
	wg.Wait()
}

func TestHubRateLimiterNoteThrottle(t *testing.T) {
	var logBuf bytes.Buffer
	lg := newSafeLogger(&logBuf)

	e := newEnv(t, func(o *Options) {
		o.RatePerSecond = 1
		o.RateBurst = 1
		o.Log = lg
	})
	a, _ := e.twoSites()

	// Send 10 rapid requests that exceed the rate limit
	for i := 0; i < 10; i++ {
		resp, err := e.client(a, "x", nil).HTTP.Get("https://" + e.addr + Prefix + "crew" + syncer.PathSummary)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	// In the same minute, the log should only be written once
	logs := strings.Split(strings.TrimSpace(logBuf.String()), "\n")
	if len(logs) != 1 {
		t.Fatalf("expected exactly 1 log line within 60s, got %d: %q", len(logs), logBuf.String())
	}

	// Advance time past 60s
	e.clk.advance(65 * time.Second)

	// Exceed limit again
	for i := 0; i < 5; i++ {
		resp, err := e.client(a, "x", nil).HTTP.Get("https://" + e.addr + Prefix + "crew" + syncer.PathSummary)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	logsAfter := strings.Split(strings.TrimSpace(logBuf.String()), "\n")
	if len(logsAfter) != 2 {
		t.Fatalf("expected 2 log lines after advancing past 60s, got %d: %q", len(logsAfter), logBuf.String())
	}
}

type safeWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *safeWriter) Write(p []byte) (n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func newSafeLogger(w io.Writer) *log.Logger {
	return log.New(&safeWriter{w: w}, "hub: ", 0)
}
