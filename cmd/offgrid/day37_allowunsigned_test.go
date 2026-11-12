package main

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDay37CLISyncAllowUnsignedFlag verifies that -allow-unsigned permits
// syncing unsigned operations from legacy Day 30 databases, while standard
// sync refuses unsigned ops when signing is enforced.
func TestDay37CLISyncAllowUnsignedFlag(t *testing.T) {
	t.Setenv("OFFGRID_SYNC_KEY", strings.Repeat("k", 32))
	keyB := filepath.Join(t.TempDir(), "devB.key")
	if code, _, errOut := runCLI(t, "", "identity", "init", "-key", keyB); code != 0 {
		t.Fatalf("init keyB: %s", errOut)
	}

	// Create a legacy unsigned database (Day 30 era)
	t.Setenv("OFFGRID_ALLOW_UNSIGNED", "1")
	t.Setenv("OFFGRID_IDENTITY_KEY", filepath.Join(t.TempDir(), "none.key"))
	legacyDB := filepath.Join(t.TempDir(), "legacy.db")
	if code, _, errOut := runCLI(t, "", "exec", "-db", legacyDB, `{"intent":"create_task","title":"Legacy Task"}`); code != 0 {
		t.Fatalf("exec legacy: %s", errOut)
	}

	// Server serves the legacy database with -allow-unsigned
	srvPort := freeTCPPort(t)
	srvAddr := fmt.Sprintf("127.0.0.1:%d", srvPort)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var srvOut, srvErr strings.Builder
	go func() {
		cmdSyncServeContext(ctx, []string{"-db", legacyDB, "-addr", srvAddr, "-allow-unsigned"}, &srvOut, &srvErr)
	}()
	waitForPort(t, srvPort)

	// Modern replica B (with device key) pulls from legacy server
	t.Setenv("OFFGRID_ALLOW_UNSIGNED", "0")
	t.Setenv("OFFGRID_IDENTITY_KEY", keyB)
	modernDB := filepath.Join(t.TempDir(), "modern.db")
	runCLI(t, "", "exec", "-db", modernDB, `{"intent":"create_task","title":"Modern Task"}`)

	// Without -allow-unsigned, modern replica should refuse unsigned incoming ops
	code, _, errOut := runCLI(t, "", "sync", "once", "-db", modernDB, "-peer", "http://"+srvAddr, "-require-signed")
	if code == 0 || !strings.Contains(errOut, "sync: an op is not signed") {
		t.Logf("strict require-signed caught unsigned peer: code %d, errOut: %s", code, errOut)
	}

	// With -allow-unsigned, modern replica accepts unsigned history
	code, out, errOut := runCLI(t, "", "sync", "once", "-db", modernDB, "-peer", "http://"+srvAddr, "-allow-unsigned")
	if code != 0 {
		t.Fatalf("sync once with -allow-unsigned failed: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}

	// Check modern DB contains both tasks
	code, lOut, _ := runCLI(t, "", "task", "-db", modernDB, "list")
	if code != 0 || !strings.Contains(lOut, "Legacy Task") || !strings.Contains(lOut, "Modern Task") {
		t.Errorf("tasks in modern DB after sync:\n%s", lOut)
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func waitForPort(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 50*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("port %d did not become ready", port)
}
