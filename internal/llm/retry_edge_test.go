package llm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryExponentialBackoffLimitsAndJitter(t *testing.T) {
	// Base: 100ms, Max: 1s
	p := RetryPolicy{
		Base: 100 * time.Millisecond,
		Max:  1 * time.Second,
		Rand: func() float64 { return 1.0 }, // max bound test
	}

	// n = 0: ceil = 100ms
	if w := p.Wait(0); w != 100*time.Millisecond {
		t.Errorf("n=0: got %v, want 100ms", w)
	}

	// n = 1: ceil = 200ms
	if w := p.Wait(1); w != 200*time.Millisecond {
		t.Errorf("n=1: got %v, want 200ms", w)
	}

	// n = 2: ceil = 400ms
	if w := p.Wait(2); w != 400*time.Millisecond {
		t.Errorf("n=2: got %v, want 400ms", w)
	}

	// n = 3: ceil = 800ms
	if w := p.Wait(3); w != 800*time.Millisecond {
		t.Errorf("n=3: got %v, want 800ms", w)
	}

	// n = 4: ceil = 1600ms > Max(1s) -> clamped to 1s
	if w := p.Wait(4); w != 1*time.Second {
		t.Errorf("n=4: got %v, want 1s (capped)", w)
	}

	// Extreme shifts: n = 30, 50, 100 (must not overflow or produce negative duration)
	for _, n := range []int{30, 50, 100, 500} {
		w := p.Wait(n)
		if w < 0 || w > 1*time.Second {
			t.Errorf("n=%d: invalid duration %v", n, w)
		}
	}

	// Jitter with Rand = 0.5
	p.Rand = func() float64 { return 0.5 }
	if w := p.Wait(0); w != 50*time.Millisecond {
		t.Errorf("jitter 0.5 at n=0: got %v, want 50ms", w)
	}
}

func TestRetryContextCancellationDuringSleep(t *testing.T) {
	fake := &countingClient{errs: []error{ErrUnavailable, ErrUnavailable}}

	ctx, cancel := context.WithCancel(context.Background())
	// Custom sleep function that checks context cancellation
	sleepCalled := make(chan struct{})
	policy := RetryPolicy{
		Attempts: 3,
		Base:     time.Hour, // long sleep
		Sleep: func(ctx context.Context, d time.Duration) error {
			close(sleepCalled)
			<-ctx.Done()
			return ctx.Err()
		},
	}

	client := WithRetry(fake, policy)

	go func() {
		<-sleepCalled
		cancel() // Cancel while sleeping before attempt 2
	}()

	_, err := client.Chat(ctx, validRequest(), nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}

	// Should have executed only the first attempt
	if fake.calls != 1 {
		t.Errorf("expected 1 call before cancel, got %d", fake.calls)
	}
}

func TestRetryDoesNotRetryAfterFirstTokenStreamed(t *testing.T) {
	// A server that yields 1 token then fails with ErrUnavailable
	streamingFailClient := &tokenThenFailClient{token: "partial-token", err: ErrUnavailable}

	var receivedTokens []string
	policy := RetryPolicy{
		Attempts: 3,
		Base:     time.Millisecond,
		Sleep:    func(ctx context.Context, d time.Duration) error { return nil },
	}

	client := WithRetry(streamingFailClient, policy)
	_, err := client.Chat(context.Background(), validRequest(), func(tok string) {
		receivedTokens = append(receivedTokens, tok)
	})

	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got: %v", err)
	}
	if streamingFailClient.calls != 1 {
		t.Errorf("must NOT retry after tokens streamed; calls = %d, want 1", streamingFailClient.calls)
	}
	if len(receivedTokens) != 1 || receivedTokens[0] != "partial-token" {
		t.Errorf("unexpected tokens: %v", receivedTokens)
	}
}

func TestRetryNonUnavailableErrorsAreImmediate(t *testing.T) {
	badReqErr := errors.New("llm: HTTP 400 Bad Request")
	fake := &countingClient{errs: []error{badReqErr}}

	policy := RetryPolicy{
		Attempts: 5,
		Base:     time.Millisecond,
		Sleep:    func(ctx context.Context, d time.Duration) error { return nil },
	}

	client := WithRetry(fake, policy)
	_, err := client.Chat(context.Background(), validRequest(), nil)
	if !errors.Is(err, badReqErr) {
		t.Fatalf("expected badReqErr, got: %v", err)
	}
	if fake.calls != 1 {
		t.Errorf("non-unavailable errors must fail immediately without retry; calls = %d", fake.calls)
	}
}

func TestRetryConcurrencyUnderLoad(t *testing.T) {
	// Client that fails once with ErrUnavailable, then succeeds
	flaky := &flakyClient{}
	policy := RetryPolicy{
		Attempts: 3,
		Base:     time.Millisecond,
		Max:      5 * time.Millisecond,
	}

	client := WithRetry(flaky, policy)
	const concurrentRoutines = 20
	var wg sync.WaitGroup

	for i := 0; i < concurrentRoutines; i++ {
		wg.Add(1)
		id := fmt.Sprintf("req-%d", i)
		go func(content string) {
			defer wg.Done()
			req := validRequest()
			req.Messages = []Message{{Role: "user", Content: content}}
			resp, err := client.Chat(context.Background(), req, nil)
			if err != nil || resp.Text != "ok" {
				t.Errorf("concurrent chat error: %v, resp: %+v", err, resp)
			}
		}(id)
	}
	wg.Wait()
}

type countingClient struct {
	mu    sync.Mutex
	calls int
	errs  []error
}

func (c *countingClient) Chat(ctx context.Context, req Request, onToken func(string)) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	idx := c.calls
	c.calls++
	if idx < len(c.errs) {
		return Response{}, c.errs[idx]
	}
	return Response{Text: "done"}, nil
}

type tokenThenFailClient struct {
	calls int
	token string
	err   error
}

func (c *tokenThenFailClient) Chat(ctx context.Context, req Request, onToken func(string)) (Response, error) {
	c.calls++
	if onToken != nil {
		onToken(c.token)
	}
	return Response{}, c.err
}

type flakyClient struct {
	attempts sync.Map
}

func (c *flakyClient) Chat(ctx context.Context, req Request, onToken func(string)) (Response, error) {
	key := ""
	if len(req.Messages) > 0 {
		key = req.Messages[0].Content
	}
	v, _ := c.attempts.LoadOrStore(key, new(int32))
	ptr := v.(*int32)
	att := atomic.AddInt32(ptr, 1)
	if att == 1 {
		return Response{}, ErrUnavailable // First attempt fails
	}
	return Response{Text: "ok"}, nil // Retry succeeds
}
