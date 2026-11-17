package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type threadSafeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *threadSafeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *threadSafeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestCLISyncRunNegativeBackoffMaxRefused(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, []byte("0123456789abcdef0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, badVal := range []string{"0s", "-5s", "-100ms"} {
		var stdout, stderr bytes.Buffer
		code := cmdSyncRun([]string{
			"-create",
			"-db", filepath.Join(dir, "test.db"),
			"-peer", "http://127.0.0.1:1",
			"-key-file", key,
			"-backoff-max", badVal,
		}, &stdout, &stderr)

		if code != 2 {
			t.Errorf("backoff-max %s: exit code %d, want 2 (stderr: %s)", badVal, code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "-backoff-max must be positive") {
			t.Errorf("backoff-max %s: stderr missing positive error: %s", badVal, stderr.String())
		}
	}
}

func TestCLISyncRunContextCancellationDuringBackoffWait(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, []byte("0123456789abcdef0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr threadSafeBuffer

	done := make(chan int, 1)
	go func() {
		code := cmdSyncRunContext(ctx, []string{
			"-create",
			"-db", filepath.Join(dir, "cancel.db"),
			"-peer", "http://127.0.0.1:1",
			"-key-file", key,
			"-every", "10ms",
			"-backoff-max", "10s", // Large backoff wait
		}, &stdout, &stderr)
		done <- code
	}()

	// Wait until sync run has started its loop and printed startup banner
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(stdout.String(), "syncing with") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	// Wait for the sync loop to fail once and enter backoff wait timer
	time.Sleep(50 * time.Millisecond)

	// Cancel context during the long 10s backoff wait
	cancel()

	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("expected clean exit code 0 on context cancel, got %d", code)
		}
		if !strings.Contains(stdout.String(), "stopped") {
			t.Errorf("expected 'stopped' output on cancel, got:\n%s", stdout.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cmdSyncRunContext hung during backoff wait timer after cancel")
	}
}
