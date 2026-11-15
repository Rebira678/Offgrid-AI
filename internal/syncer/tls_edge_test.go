package syncer

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

// TestTLSEdgeReplayCacheHighConcurrency verifies thread safety and atomic semantics
// of the replay protection cache under heavy concurrent load.
func TestTLSEdgeReplayCacheHighConcurrency(t *testing.T) {
	const workers = 30
	const opsPerWorker = 50
	c := newReplayCache(workers*opsPerWorker*2, 5*time.Minute)
	now := t0

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		workerID := w
		go func() {
			defer wg.Done()
			for i := 0; i < opsPerWorker; i++ {
				key := fmt.Sprintf("dev_%d/req_%d", workerID, i)
				if err := c.see(key, now); err != nil {
					t.Errorf("first insert failed: %v", err)
				}
				// Immediate replay must fail
				if err := c.see(key, now); !errors.Is(err, errReplay) {
					t.Errorf("immediate replay of %s did not return errReplay: %v", key, err)
				}
			}
		}()
	}
	wg.Wait()
}

// TestTLSEdgeReplayCacheFullReturns503OnWire verifies that when the replay cache
// reaches its maximum size, subsequent distinct requests receive HTTP 503 Service Unavailable.
func TestTLSEdgeReplayCacheFullReturns503OnWire(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{t0}
	admin, ida := newSignedStore(t, "laptop", clk)
	_, idb := newSignedStore(t, "pi", clk)
	if err := admin.Found(ctx, "laptop"); err != nil {
		t.Fatal(err)
	}
	if err := admin.AddMember(ctx, idb.PublicKey(), "member", "pi"); err != nil {
		t.Fatal(err)
	}

	authz := MembershipAuthorizer(admin, clk.now)
	cfg, err := ServerTLSConfig(ida, authz, clk.now)
	if err != nil {
		t.Fatal(err)
	}

	// Tiny replay cache capacity: 2 entries
	inner := Handler(admin, ServerOptions{
		MutualTLS:  true,
		Authorizer: authz,
		Now:        clk.now,
		ReplayMax:  2,
		Window:     5 * time.Minute,
	})
	srv, addr := startSecure(t, inner, cfg, DefaultTimeouts)
	defer srv.Close()

	clientCfg := ClientTLSConfig(idb, PinDevice(ida.ID()), clk.now)
	hc := NewTLSClient(clientCfg)

	sendReq := func(reqID string) int {
		req, _ := http.NewRequest("GET", "https://"+addr+PathSummary, nil)
		req.Header.Set(HeaderRequest, reqID)
		req.Header.Set(HeaderTime, fmt.Sprint(clk.now().Unix()))
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	// First two distinct requests should succeed (200 OK)
	if st := sendReq("00000000000000000000000000000001"); st != http.StatusOK {
		t.Fatalf("req 1 status = %d, want 200", st)
	}
	if st := sendReq("00000000000000000000000000000002"); st != http.StatusOK {
		t.Fatalf("req 2 status = %d, want 200", st)
	}

	// 3rd distinct request exceeds cache capacity (ReplayMax=2): must get 503 Service Unavailable
	st3 := sendReq("00000000000000000000000000000003")
	if st3 != http.StatusServiceUnavailable {
		t.Fatalf("req 3 status = %d, want 503 Service Unavailable", st3)
	}
}

// TestTLSEdgeExpiredClientCertRefusedAtHandshake verifies that a peer presenting
// a certificate past its NotAfter validity period fails during TLS handshake.
func TestTLSEdgeExpiredClientCertRefusedAtHandshake(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{t0}
	admin, ida := newSignedStore(t, "laptop", clk)
	_, idb := newSignedStore(t, "pi", clk)
	if err := admin.Found(ctx, "laptop"); err != nil {
		t.Fatal(err)
	}
	if err := admin.AddMember(ctx, idb.PublicKey(), "member", "pi"); err != nil {
		t.Fatal(err)
	}

	authz := MembershipAuthorizer(admin, clk.now)
	cfg, err := ServerTLSConfig(ida, authz, clk.now)
	if err != nil {
		t.Fatal(err)
	}

	inner := Handler(admin, ServerOptions{
		MutualTLS:  true,
		Authorizer: authz,
		Now:        clk.now,
	})
	srv, addr := startSecure(t, inner, cfg, DefaultTimeouts)
	defer srv.Close()

	// Generate certificate that was valid yesterday and expired before clk.now()
	expiredCert, err := idb.Certificate(clk.now().Add(-48*time.Hour), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	badClientCfg := &tls.Config{
		Certificates:       []tls.Certificate{expiredCert},
		MinVersion:         tls.VersionTLS13,
		MaxVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // we are specifically checking server's refusal of client cert
	}
	badClient := &http.Client{
		Transport: &http.Transport{TLSClientConfig: badClientCfg},
		Timeout:   3 * time.Second,
	}

	req, _ := http.NewRequest("GET", "https://"+addr+PathSummary, nil)
	signSecure(req, clk.now())
	_, err = badClient.Do(req)
	if err == nil {
		t.Fatal("server accepted an expired client certificate")
	}
}

// TestTLSEdgePlainHTTPToTLSPortFailsGracefully verifies that a plain HTTP request
// sent to the mutual TLS port does not crash or stall the server.
func TestTLSEdgePlainHTTPToTLSPortFailsGracefully(t *testing.T) {
	e := newTLSEnv(t)
	plainClient := &http.Client{Timeout: 3 * time.Second}
	resp, err := plainClient.Get("http://" + e.addr + PathSummary)
	if err == nil {
		defer resp.Body.Close()
		// Go's HTTP server returns 400 Bad Request when receiving HTTP on a TLS listener
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("plain HTTP to TLS port returned status %d, want 400", resp.StatusCode)
		}
	}
	// An error (e.g., connection reset or malformed HTTP response) is also completely acceptable.
}

func TestTLSEdgeRequestIDValidationEdges(t *testing.T) {
	cases := []struct {
		id    string
		valid bool
	}{
		{"0123456789abcdef0123456789abcdef", true},
		{"ABCDEF0123456789ABCDEF0123456789", true},
		{"0123456789abcdef0123456789abcde", false}, // 31 chars
		{"0123456789abcdef0123456789abcdef0", false}, // 33 chars
		{"", false},
		{"0123456789abcdef0123456789abcdeg", false}, // 'g' is non-hex
		{"0123456789abcdef0123456789abcde!", false}, // symbol
		{"                                ", false}, // spaces
	}

	for _, tc := range cases {
		got := validRequestID(tc.id)
		if got != tc.valid {
			t.Errorf("validRequestID(%q) = %v, want %v", tc.id, got, tc.valid)
		}
	}
}

func TestTLSEdgeReplayCacheLazyPruning(t *testing.T) {
	// Capacity 10, ttl 10s
	c := newReplayCache(10, 10*time.Second)
	t0 := time.Now()

	// Fill with 6 entries (>= max/2, triggering lazy pruning on next see)
	for i := 0; i < 6; i++ {
		if err := c.see(fmt.Sprintf("old_%d", i), t0); err != nil {
			t.Fatal(err)
		}
	}

	// Advance time past TTL (15s)
	t1 := t0.Add(15 * time.Second)

	// Inserting a new entry should trigger lazy pruning of the 6 expired entries
	if err := c.see("new_1", t1); err != nil {
		t.Fatal(err)
	}

	c.mu.Lock()
	count := len(c.seen)
	c.mu.Unlock()

	// Only "new_1" should remain in cache
	if count != 1 {
		t.Errorf("expected 1 entry after lazy pruning, got %d", count)
	}
}

func TestTLSEdgeClockSkewBoundaryConditions(t *testing.T) {
	e := newTLSEnv(t)
	now := e.clk.now()
	window := DefaultWindow // 5m

	sendWithTime := func(unix int64, reqID string) int {
		cfg := ClientTLSConfig(e.idb, PinDevice(e.ida.ID()), e.clk.now)
		hc := NewTLSClient(cfg)
		req, _ := http.NewRequest("GET", e.url+PathSummary, nil)
		req.Header.Set(HeaderRequest, reqID)
		req.Header.Set(HeaderTime, fmt.Sprint(unix))
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	// 1. Exactly at current time: 200 OK
	if st := sendWithTime(now.Unix(), "10000000000000000000000000000001"); st != http.StatusOK {
		t.Errorf("status at now = %d, want 200", st)
	}

	// 2. Exactly at +window: 200 OK
	if st := sendWithTime(now.Add(window).Unix(), "10000000000000000000000000000002"); st != http.StatusOK {
		t.Errorf("status at +window = %d, want 200", st)
	}

	// 3. Exactly at -window: 200 OK
	if st := sendWithTime(now.Add(-window).Unix(), "10000000000000000000000000000003"); st != http.StatusOK {
		t.Errorf("status at -window = %d, want 200", st)
	}

	// 4. Outside window (+window + 10s): 401 Unauthorized
	if st := sendWithTime(now.Add(window+10*time.Second).Unix(), "10000000000000000000000000000004"); st != http.StatusUnauthorized {
		t.Errorf("status outside window (+10s) = %d, want 401", st)
	}

	// 5. Outside window (-window - 10s): 401 Unauthorized
	if st := sendWithTime(now.Add(-window-10*time.Second).Unix(), "10000000000000000000000000000005"); st != http.StatusUnauthorized {
		t.Errorf("status outside window (-10s) = %d, want 401", st)
	}
}
