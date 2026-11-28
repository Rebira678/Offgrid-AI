package webhook

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWebhookDispatcherRetryAndFastFail(t *testing.T) {
	// 1. Receiver responds 500, then 503, then 200 OK -> retries and succeeds
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		att := attempts.Add(1)
		switch att {
		case 1:
			w.WriteHeader(http.StatusInternalServerError)
		case 2:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	d := &Dispatcher{
		Client:  srv.Client(),
		Backoff: []time.Duration{time.Millisecond, 2 * time.Millisecond},
	}
	hook := Hook{ID: "wh_test1", URL: srv.URL, Secret: hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))}
	ev := Event{ID: "n:1", Type: EventTaskCreated, At: time.Now().UTC()}

	if err := d.Send(context.Background(), hook, ev); err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if attempts.Load() != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts.Load())
	}

	// 2. Receiver responds 400 Bad Request -> fast fail, no retries
	var badAttempts atomic.Int32
	srvBad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		badAttempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srvBad.Close()

	dBad := &Dispatcher{
		Client:  srvBad.Client(),
		Backoff: []time.Duration{time.Millisecond, 2 * time.Millisecond},
	}
	hookBad := Hook{ID: "wh_test2", URL: srvBad.URL, Secret: hook.Secret}
	err := dBad.Send(context.Background(), hookBad, ev)
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("expected fast-fail on 400, got %v", err)
	}
	if badAttempts.Load() != 1 {
		t.Fatalf("expected 1 attempt (no retries for 400), got %d", badAttempts.Load())
	}

	// 3. Receiver responds 429 Too Many Requests -> retries
	var rateAttempts atomic.Int32
	srvRate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		att := rateAttempts.Add(1)
		if att < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srvRate.Close()

	dRate := &Dispatcher{
		Client:  srvRate.Client(),
		Backoff: []time.Duration{time.Millisecond, 2 * time.Millisecond},
	}
	hookRate := Hook{ID: "wh_test3", URL: srvRate.URL, Secret: hook.Secret}
	if err := dRate.Send(context.Background(), hookRate, ev); err != nil {
		t.Fatalf("expected 429 to be retried and succeed, got %v", err)
	}
	if rateAttempts.Load() != 3 {
		t.Fatalf("expected 3 attempts on 429, got %d", rateAttempts.Load())
	}

	// 4. Context cancellation stops retry sleep immediately
	ctx, cancel := context.WithCancel(context.Background())
	var sleepCalled atomic.Bool
	dSleep := &Dispatcher{
		Client:  srvBad.Client(),
		Backoff: []time.Duration{10 * time.Second},
		Sleep: func(ctx context.Context, d time.Duration) {
			sleepCalled.Store(true)
			<-ctx.Done()
		},
	}
	cancel() // pre-cancel
	_ = dSleep.Send(ctx, hook, ev)
}
