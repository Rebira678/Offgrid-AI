package main

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// buildBinary compiles the real offgrid binary so the test can run two
// separate OS processes. It is skipped where the Go toolchain is not on PATH.
func buildBinary(t *testing.T) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not found")
	}
	bin := filepath.Join(t.TempDir(), "offgrid")
	cmd := exec.Command(goBin, "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

func runBin(t *testing.T, bin string, env []string, args ...string) (string, string, error) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), env...)
	var o, e strings.Builder
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	return o.String(), e.String(), err
}

var hashRe = regexp.MustCompile(`state hash:\s+([0-9a-f]+)`)

func stateHashOf(t *testing.T, bin, db string) string {
	t.Helper()
	out, errOut, err := runBin(t, bin, nil, "sync", "summary", "-db", db)
	if err != nil {
		t.Fatalf("summary: %v\n%s", err, errOut)
	}
	m := hashRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no state hash in:\n%s", out)
	}
	return m[1]
}

func TestTwoOffgridProcessesExchangeOpsOverLocalhostHTTP(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	bin := buildBinary(t)
	dir := t.TempDir()
	dbA, dbB := filepath.Join(dir, "a.db"), filepath.Join(dir, "b.db")
	keyFile := filepath.Join(dir, "key")
	os.WriteFile(keyFile, []byte("process-test-key-0123456789abcdef\n"), 0o600)
	otherKey := filepath.Join(dir, "other")
	os.WriteFile(otherKey, []byte("some-other-key-0123456789abcdef\n"), 0o600)

	exec1 := func(db, js string) {
		t.Helper()
		if out, errOut, err := runBin(t, bin, nil, "exec", "-db", db, js); err != nil {
			t.Fatalf("exec %s: %v\n%s%s", js, err, out, errOut)
		}
	}

	// Replica A has some work. Replica B is a brand-new database.
	exec1(dbA, `{"intent":"create_task","title":"Leaking pump","site":"4","priority":"high"}`)
	exec1(dbA, `{"intent":"create_task","title":"Replace filter","priority":"low"}`)

	// B runs as a server: a separate process listening on a free port.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := exec.CommandContext(ctx, bin, "sync", "serve", "-db", dbB, "-addr", "127.0.0.1:0", "-key-file", keyFile)
	stdout, _ := srv.StdoutPipe()
	var srvErr lockedBuf
	srv.Stderr = &srvErr
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); srv.Wait() }()
	lines := make(chan string, 4)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	var url string
	select {
	case l := <-lines:
		m := regexp.MustCompile(`serving on (http://\S+)`).FindStringSubmatch(l)
		if m == nil {
			t.Fatalf("unexpected first line %q (stderr: %s)", l, srvErr.String())
		}
		url = m[1]
	case <-time.After(20 * time.Second):
		t.Fatalf("the server did not start; stderr: %s", srvErr.String())
	}

	// A wrong key is refused and changes nothing.
	_, errOut, err := runBin(t, bin, nil, "sync", "once", "-db", dbA, "-peer", url, "-key-file", otherKey)
	if err == nil || !strings.Contains(errOut, "unauthorized") {
		t.Fatalf("wrong key: err=%v stderr=%q, want an unauthorized failure", err, errOut)
	}

	// Round 1: A pushes its two tasks to B.
	out, errOut, err := runBin(t, bin, nil, "sync", "once", "-db", dbA, "-peer", url, "-key-file", keyFile)
	if err != nil || !strings.Contains(out, "pulled 0, pushed 2") {
		t.Fatalf("round 1: %v\n%s%s", err, out, errOut)
	}
	if stateHashOf(t, bin, dbA) != stateHashOf(t, bin, dbB) {
		t.Fatal("hashes differ after round 1")
	}

	// Both now change things independently (the server process is still running).
	exec1(dbA, `{"intent":"update_task","task_ref":"T1","status":"done"}`)
	exec1(dbA, `{"intent":"create_task","title":"Fuel line","priority":"medium"}`)
	exec1(dbB, `{"intent":"add_note","task_ref":"T2","note":"waiting for parts"}`)
	exec1(dbB, `{"intent":"create_task","title":"Check valve","priority":"high"}`)
	if stateHashOf(t, bin, dbA) == stateHashOf(t, bin, dbB) {
		t.Fatal("the replicas should differ before the second round")
	}

	// Round 2: one round brings both sides level.
	out, errOut, err = runBin(t, bin, nil, "sync", "once", "-db", dbA, "-peer", url, "-key-file", keyFile)
	if err != nil || !strings.Contains(out, "pulled 2, pushed 2") {
		t.Fatalf("round 2: %v\n%s%s", err, out, errOut)
	}
	ha, hb := stateHashOf(t, bin, dbA), stateHashOf(t, bin, dbB)
	if ha != hb {
		t.Fatalf("hashes differ after round 2:\n A %s\n B %s", ha, hb)
	}

	// Both databases verify against their own logs.
	for _, db := range []string{dbA, dbB} {
		if out, errOut, err := runBin(t, bin, nil, "store", "-db", db, "verify"); err != nil {
			t.Fatalf("verify %s: %v\n%s%s", db, err, out, errOut)
		}
	}
	// And a third round has nothing to do.
	out, _, err = runBin(t, bin, nil, "sync", "once", "-db", dbA, "-peer", url, "-key-file", keyFile)
	if err != nil || !strings.Contains(out, "pulled 0, pushed 0") {
		t.Fatalf("round 3: %v\n%s", err, out)
	}
	// The key never appears in anything the server printed.
	if strings.Contains(srvErr.String(), "process-test-key") {
		t.Fatal("the key was logged")
	}
}

func TestSyncCommandsRefuseMissingInputs(t *testing.T) {
	var o, e strings.Builder
	t.Setenv("OFFGRID_SYNC_KEY", "")
	if code := run([]string{"sync", "once", "-db", filepath.Join(t.TempDir(), "x.db")}, &o, &e); code != 2 || !strings.Contains(e.String(), "-peer") {
		t.Errorf("no peer: code %d, %q", code, e.String())
	}
	e.Reset()
	if code := run([]string{"sync", "once", "-peer", "http://127.0.0.1:1", "-db", filepath.Join(t.TempDir(), "x.db")}, &o, &e); code != 2 || !strings.Contains(e.String(), "key") {
		t.Errorf("no key: code %d, %q", code, e.String())
	}
	e.Reset()
	if code := run([]string{"sync", "bogus"}, &o, &e); code != 2 {
		t.Errorf("unknown subcommand: code %d", code)
	}
	e.Reset()
	t.Setenv("OFFGRID_SYNC_KEY", "0123456789abcdef0123")
	if code := run([]string{"sync", "once", "-peer", "http://127.0.0.1:1", "-timeout", "3s", "-db", filepath.Join(t.TempDir(), "x.db")}, &o, &e); code != 1 || !strings.Contains(e.String(), "cannot reach the peer") {
		t.Errorf("nothing listening: code %d, %q", code, e.String())
	}
}

func TestSyncServeGracefulShutdown(t *testing.T) {
	dir := t.TempDir()
	dbFile := filepath.Join(dir, "test.db")
	keyFile := filepath.Join(dir, "key")
	if err := os.WriteFile(keyFile, []byte("0123456789abcdef0123"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr lockedBuf

	cmdDone := make(chan int, 1)
	go func() {
		code := cmdSyncServeContext(ctx, []string{"-db", dbFile, "-addr", "127.0.0.1:0", "-key-file", keyFile}, &stdout, &stderr)
		cmdDone <- code
	}()

	waitFor(t, &stdout, "serving on http://", 2*time.Second)
	if !strings.Contains(stdout.String(), "serving on http://") {
		t.Fatalf("server failed to start listening: stderr: %s", stderr.String())
	}

	// Cancel context to trigger graceful shutdown
	cancel()

	select {
	case code := <-cmdDone:
		if code != 0 {
			t.Fatalf("expected exit code 0 on graceful shutdown, got %d; stderr: %s", code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "offgrid sync: stopped") {
			t.Fatalf("expected 'offgrid sync: stopped' in stdout: %s", stdout.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for sync serve to stop")
	}
}
