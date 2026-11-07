package ollama

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// A host that accepts the connection and then says nothing is what a half-dead
// WiFi link or a wedged Ollama on another node looks like. Day 32: Version and
// ListModels used to wait forever when the caller passed a context with no
// deadline (`offgrid models` does), because the client has no overall Timeout
// on purpose. They now carry their own short deadline.
func TestQuickCallsGiveUpOnASilentHost(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // hold it open, never answer
		}
	}()

	old := QuickCallTimeout
	QuickCallTimeout = 200 * time.Millisecond
	defer func() { QuickCallTimeout = old }()

	c := NewClient("http://" + ln.Addr().String())
	calls := map[string]func(context.Context) error{
		"Version":    func(ctx context.Context) error { _, err := c.Version(ctx); return err },
		"ListModels": func(ctx context.Context) error { _, err := c.ListModels(ctx); return err },
	}
	for name, call := range calls {
		done := make(chan error, 1)
		go func() { done <- call(context.Background()) }()
		select {
		case err := <-done:
			if !errors.Is(err, ErrUnreachable) {
				t.Errorf("%s: error = %v, want ErrUnreachable", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("%s: still waiting after 5s; it has no deadline of its own", name)
		}
	}
}

// A caller's own, longer deadline is respected: the quick default only fills a gap.
func TestQuickCallsKeepTheCallersDeadline(t *testing.T) {
	old := QuickCallTimeout
	QuickCallTimeout = time.Millisecond
	defer func() { QuickCallTimeout = old }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, cancel2 := quickDeadline(ctx)
	defer cancel2()
	dl, _ := got.Deadline()
	if time.Until(dl) < 2*time.Second {
		t.Errorf("deadline shortened to %v", time.Until(dl))
	}
}

// Connecting is bounded too, so a host that drops packets fails in seconds,
// not after the operating system's own two-minute wait.
func TestClientDialsWithATimeout(t *testing.T) {
	if got := dialTimeout; got <= 0 || got > 30*time.Second {
		t.Errorf("dialTimeout = %v", got)
	}
}
