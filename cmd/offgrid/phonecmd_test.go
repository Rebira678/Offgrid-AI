package main

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"offgrid/internal/phone"
	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
)

func TestPhoneCLIUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer

	// Empty args -> code 2 and help output
	code := run([]string{"phone"}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("expected exit code 2, got %d", code)
	}
	if !strings.Contains(stderr.String(), "offgrid phone manages the phone PWA client") {
		t.Errorf("expected usage output, got %q", stderr.String())
	}

	// Unknown subcommand -> code 2
	stderr.Reset()
	code = run([]string{"phone", "unknown"}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("expected exit code 2, got %d", code)
	}
}

func TestPhoneCLINoDatabase(t *testing.T) {
	var stdout, stderr bytes.Buffer
	nonExistentDB := filepath.Join(t.TempDir(), "ghost.db")

	code := run([]string{"phone", "pair", "-db", nonExistentDB}, &stdout, &stderr)
	if code != exitNoDatabase {
		t.Errorf("expected exitNoDatabase (%d), got %d (stderr: %s)", exitNoDatabase, code, stderr.String())
	}
}

func TestPhoneCLIPairListAndRevoke(t *testing.T) {
	ctx := context.Background()
	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, "tasks.db")

	// Initialize database
	st, err := sqlstore.Open(ctx, dbPath, sqlstore.Options{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	// Seed a task
	_, _, _ = st.Create(ctx, task.NewTask{Title: "Initial Task", Priority: task.PriorityHigh})
	st.Close()

	var stdout, stderr bytes.Buffer

	// 1. Generate pairing code
	code := run([]string{"phone", "pair", "-db", dbPath, "-label", "Field Tech 1", "-ttl", "15m"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("phone pair failed with %d: %s", code, stderr.String())
	}

	outStr := stdout.String()
	if !strings.Contains(outStr, "Pairing code:") || !strings.Contains(outStr, "Field Tech 1") {
		t.Fatalf("unexpected pair output: %s", outStr)
	}

	// Extract pairing code
	var pairCode string
	for _, line := range strings.Split(outStr, "\n") {
		if strings.HasPrefix(line, "Pairing code:") {
			pairCode = strings.TrimSpace(strings.TrimPrefix(line, "Pairing code:"))
			break
		}
	}
	if len(pairCode) != 7 || pairCode[3] != '-' {
		t.Fatalf("extracted invalid pairing code: %q", pairCode)
	}

	// 2. Simulate phone redeeming pairing code
	stReopen, err := sqlstore.Open(ctx, dbPath, sqlstore.Options{})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	sessStore, err := phone.NewSessionStore(stReopen.DB(), time.Now)
	if err != nil {
		t.Fatalf("session store: %v", err)
	}
	sess, err := sessStore.RedeemPairingCode(ctx, pairCode, "Pixel 8")
	if err != nil {
		t.Fatalf("redeem code failed: %v", err)
	}
	stReopen.Close()

	// 3. List enrolled sessions via CLI
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"phone", "list", "-db", dbPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("phone list failed: %d (%s)", code, stderr.String())
	}

	listOut := stdout.String()
	if !strings.Contains(listOut, "TOKEN PREFIX") || !strings.Contains(listOut, "Field Tech 1 (Pixel 8)") {
		t.Errorf("list output missing session info: %s", listOut)
	}

	// 4. Revoke session via CLI
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"phone", "revoke", "-db", dbPath, "-token", sess.Token[:8]}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("phone revoke failed: %d (%s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Revoked 1 phone session") {
		t.Errorf("expected confirmation of revocation, got: %s", stdout.String())
	}

	// 5. List again: status should now read "revoked"
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"phone", "list", "-db", dbPath}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "revoked") {
		t.Errorf("expected revoked status in list, got: %s", stdout.String())
	}
}

func TestPhoneCLIServeSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping binary test in short mode")
	}

	bin := buildBinary(t)
	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, "tasks.db")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, err := sqlstore.Open(ctx, dbPath, sqlstore.Options{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	st.Close()

	cmd := exec.CommandContext(ctx, bin, "phone", "serve", "-db", dbPath, "-addr", "127.0.0.1:0")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start binary: %v", err)
	}
	defer func() {
		cancel()
		_ = cmd.Wait()
	}()

	lines := make(chan string, 5)
	go func() {
		var buf [512]byte
		n, _ := stdout.Read(buf[:])
		if n > 0 {
			lines <- string(buf[:n])
		}
	}()

	select {
	case out := <-lines:
		if !strings.Contains(out, "Offgrid Phone PWA active at") {
			t.Errorf("expected server startup banner, got: %s", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("phone serve timed out")
	}
}
