package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type script struct {
	errs  []error // one per call; nil means succeed
	calls int
	pre   string // pieces sent before failing, to simulate a cut stream
}

func (s *script) Chat(ctx context.Context, req Request, onToken func(string)) (Response, error) {
	i := s.calls
	s.calls++
	if i < len(s.errs) && s.errs[i] != nil {
		if s.pre != "" && onToken != nil {
			onToken(s.pre)
		}
		return Response{}, s.errs[i]
	}
	if onToken != nil {
		onToken("ok")
	}
	return Response{Text: "ok"}, nil
}

var down = fmt.Errorf("%w: connection refused", ErrUnavailable)

func fastPolicy(waits *[]time.Duration) RetryPolicy {
	return RetryPolicy{Attempts: 3, Base: 100 * time.Millisecond, Max: 400 * time.Millisecond,
		Rand:  func() float64 { return 0.5 },
		Sleep: func(_ context.Context, d time.Duration) error { *waits = append(*waits, d); return nil }}
}

func TestRetryRecoversFromABriefOutageAndWaitsWithJitterUnderACap(t *testing.T) {
	var waits []time.Duration
	s := &script{errs: []error{down, down}}
	resp, err := WithRetry(s, fastPolicy(&waits)).Chat(context.Background(), validRequest(), nil)
	if err != nil || resp.Text != "ok" || s.calls != 3 {
		t.Fatalf("%v %v calls %d", resp, err, s.calls)
	}
	// Rand is 0.5, so the waits are half of 100ms and half of 200ms.
	if len(waits) != 2 || waits[0] != 50*time.Millisecond || waits[1] != 100*time.Millisecond {
		t.Fatalf("waits %v", waits)
	}
}

func TestRetryGivesUpAfterTheLimitAndReturnsTheLastError(t *testing.T) {
	var waits []time.Duration
	s := &script{errs: []error{down, down, down, down}}
	_, err := WithRetry(s, fastPolicy(&waits)).Chat(context.Background(), validRequest(), nil)
	if !errors.Is(err, ErrUnavailable) || s.calls != 3 || len(waits) != 2 {
		t.Fatalf("%v calls %d waits %v", err, s.calls, waits)
	}
}

func TestRetryNeverRepeatsWhatCannotSucceed(t *testing.T) {
	for name, e := range map[string]error{
		"model missing": &ModelNotFoundError{Model: "m"},
		"key rejected":  ErrRejected,
		"bad request":   ErrInvalidRequest,
		"plain failure": errors.New("llm: HTTP 400"),
	} {
		var waits []time.Duration
		s := &script{errs: []error{e, e, e}}
		_, err := WithRetry(s, fastPolicy(&waits)).Chat(context.Background(), validRequest(), nil)
		if !errors.Is(err, e) || s.calls != 1 || len(waits) != 0 {
			t.Errorf("%s: %v calls %d", name, err, s.calls)
		}
	}
}

func TestRetryDoesNotGlueASecondAnswerOntoAStreamThatStarted(t *testing.T) {
	var waits []time.Duration
	s := &script{errs: []error{down}, pre: "Two pu"}
	var got []string
	_, err := WithRetry(s, fastPolicy(&waits)).Chat(context.Background(), validRequest(), func(p string) { got = append(got, p) })
	if !errors.Is(err, ErrUnavailable) || s.calls != 1 || strings.Join(got, "") != "Two pu" {
		t.Fatalf("%v calls %d %v", err, s.calls, got)
	}
}

func TestRetryStopsWaitingWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &script{errs: []error{down, down, down}}
	p := RetryPolicy{Attempts: 3, Base: time.Hour, Max: time.Hour, Rand: func() float64 { return 0.999 }}
	done := make(chan error, 1)
	go func() { _, err := WithRetry(s, p).Chat(ctx, validRequest(), nil); done <- err }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the retry wait did not end with the context")
	}
}

func TestWaitIsBoundedByTheCapAndNeverOverflows(t *testing.T) {
	p := RetryPolicy{Base: time.Second, Max: 5 * time.Second, Rand: func() float64 { return 0.999999 }}
	for n := 0; n < 200; n++ {
		if w := p.Wait(n); w < 0 || w > 5*time.Second {
			t.Fatalf("wait(%d) = %v", n, w)
		}
	}
	if w := (RetryPolicy{Base: time.Second, Max: time.Minute, Rand: func() float64 { return 0.999999 }}).Wait(2); w < 3900*time.Millisecond || w > 4*time.Second {
		t.Fatalf("exponential growth: %v", w)
	}
	if w := (RetryPolicy{Rand: func() float64 { return 0 }}).Wait(5); w != 0 {
		t.Fatalf("full jitter can be zero: %v", w)
	}
}

func TestTheDefaultIsThreeTriesAndACancelledCallIsNotRepeated(t *testing.T) {
	var waits []time.Duration
	p := fastPolicy(&waits)
	p.Attempts = 0
	s := &script{errs: []error{down, down, down, down, down}}
	if _, err := WithRetry(s, p).Chat(context.Background(), validRequest(), nil); !errors.Is(err, ErrUnavailable) || s.calls != 3 {
		t.Fatalf("%v calls %d: zero must mean three tries", err, s.calls)
	}
	// The backend fails because the caller gave up; trying again would be work nobody wants.
	ctx, cancel := context.WithCancel(context.Background())
	s = &script{errs: []error{down, down, down}}
	waits = nil
	c := &cancelling{script: s, cancel: cancel}
	_, err := WithRetry(c, fastPolicy(&waits)).Chat(ctx, validRequest(), nil)
	if !errors.Is(err, ErrUnavailable) || s.calls != 1 || len(waits) != 0 {
		t.Fatalf("%v calls %d waits %v", err, s.calls, waits)
	}
}

type cancelling struct {
	*script
	cancel func()
}

func (c *cancelling) Chat(ctx context.Context, r Request, f func(string)) (Response, error) {
	resp, err := c.script.Chat(ctx, r, f)
	c.cancel()
	return resp, err
}
