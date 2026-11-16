package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Against a peer that is not there, `sync run` retries with growing waits (and says so), instead of every -every.
func TestSyncRunBacksOffWhilePeerIsDown(t *testing.T) {
	t.Setenv("OFFGRID_DB", "")
	t.Setenv("OFFGRID_NODE", "")
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, []byte("0123456789abcdef0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCLI(t, "", "sync", "run", "-create", "-db", filepath.Join(dir, "a.db"), "-peer", "http://127.0.0.1:1",
		"-key-file", key, "-every", "20ms", "-backoff-max", "160ms", "-for", "1500ms")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
	waits := regexp.MustCompile(`Next try in ([0-9.]+)(ms|s)`).FindAllStringSubmatch(out, -1)
	if len(waits) < 3 {
		t.Fatalf("expected the waits to be announced as they grow:\n%s", out)
	}
	var last time.Duration
	for _, m := range waits {
		d, _ := time.ParseDuration(m[1] + m[2])
		if d > 160*time.Millisecond {
			t.Errorf("a wait of %v is beyond -backoff-max 160ms", d)
		}
		last = d
	}
	if last < 80*time.Millisecond {
		t.Errorf("the waits never grew: %v", waits)
	}
	if !strings.Contains(out, "sync failed") {
		t.Errorf("the failure was not reported:\n%s", out)
	}
}
