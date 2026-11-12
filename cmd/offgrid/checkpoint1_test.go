package main

import (
	"bufio"
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// Two separate OS processes, each with its OWN device key, sync over a real TCP
// socket: the sandbox rehearsal of Checkpoint 1. It is not WiFi and not two
// machines, and docs/day-37.md says so.
func TestTwoProcessesWithTheirOwnKeysSyncSignedOpsAndSampleThemselves(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	bin := buildBinary(t)
	dir := t.TempDir()
	dbA, dbB := filepath.Join(dir, "a.db"), filepath.Join(dir, "b.db")
	shared := filepath.Join(dir, "key")
	os.WriteFile(shared, []byte("checkpoint1-key-0123456789abcdef\n"), 0o600)
	keyA, keyB := filepath.Join(dir, "a.key"), filepath.Join(dir, "b.key")
	for _, k := range []string{keyA, keyB} {
		if out, errOut, err := runBin(t, bin, nil, "identity", "init", "-key", k); err != nil {
			t.Fatalf("identity init: %v\n%s%s", err, out, errOut)
		}
	}
	envA, envB := []string{"OFFGRID_IDENTITY_KEY=" + keyA, "OFFGRID_NODE=laptop"}, []string{"OFFGRID_IDENTITY_KEY=" + keyB, "OFFGRID_NODE=pi"}
	write := func(env []string, db, js string) {
		t.Helper()
		if out, errOut, err := runBin(t, bin, env, "exec", "-db", db, js); err != nil || !strings.Contains(out, "0 refused") {
			t.Fatalf("exec: %v\n%s%s", err, out, errOut)
		}
	}
	write(envA, dbA, `{"intent":"create_task","title":"Check inverter"}`)
	write(envB, dbB, `{"intent":"create_task","title":"Replace filter"}`)

	statsB := filepath.Join(dir, "serve.stats")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := exec.CommandContext(ctx, bin, "sync", "serve", "-db", dbB, "-addr", "127.0.0.1:0", "-key-file", shared, "-stats", statsB, "-stats-every", "100ms")
	srv.Env = append(os.Environ(), envB...)
	stdout, _ := srv.StdoutPipe()
	var srvErr lockedBuf
	srv.Stderr = &srvErr
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			srv.Process.Signal(os.Interrupt) // a clean stop writes the last sample
			srv.Wait()
		}
	}
	defer stop()
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

	// The shared key is deprecated: the server says so as it starts.
	if !strings.Contains(srvErr.String(), "shared sync key is deprecated") {
		t.Fatalf("no deprecation warning from serve:\n%s", srvErr.String())
	}

	// A keeps syncing for a couple of seconds while both sides write.
	statsA := filepath.Join(dir, "run.stats")
	var runOut, runErr strings.Builder
	run := exec.Command(bin, "sync", "run", "-db", dbA, "-peer", url, "-key-file", shared, "-every", "150ms", "-for", "3s", "-stats", statsA, "-stats-every", "100ms")
	run.Env = append(os.Environ(), envA...)
	run.Stdout, run.Stderr = &runOut, &runErr
	if err := run.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond)
	write(envA, dbA, `{"intent":"create_task","title":"Fuel line"}`)
	write(envB, dbB, `{"intent":"create_task","title":"Check valve"}`)
	if err := run.Wait(); err != nil {
		t.Fatalf("sync run: %v\n%s%s", err, runOut.String(), runErr.String())
	}
	if !strings.Contains(runErr.String(), "shared sync key is deprecated") {
		t.Fatalf("no deprecation warning from run:\n%s", runErr.String())
	}
	// One more round so nothing written in the last moment is left behind.
	if out, errOut, err := runBin(t, bin, envA, "sync", "once", "-db", dbA, "-peer", url, "-key-file", shared); err != nil {
		t.Fatalf("final round: %v\n%s%s", err, out, errOut)
	}
	stop()

	if ha, hb := stateHashOf(t, bin, dbA), stateHashOf(t, bin, dbB); ha != hb {
		t.Fatalf("hashes differ:\n A %s\n B %s", ha, hb)
	}
	// Every op on both sides is signed, by two different devices, and verifies.
	for _, db := range []string{dbA, dbB} {
		out, errOut, err := runBin(t, bin, nil, "store", "-db", db, "verify")
		if err != nil || !strings.Contains(out, "ok: 4 of 4 ops are signed, by 2 device(s)") || strings.Contains(out, "WARNING") {
			t.Fatalf("verify %s: %v\n%s%s", db, err, out, errOut)
		}
	}

	// Both sampled themselves, and the verdict command reads the files (a 3 s run is too short to judge).
	for _, f := range []string{statsA, statsB} {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(string(raw), "\n"); n < 5 {
			t.Fatalf("%s: %d samples, want several", f, n)
		}
		if fi, _ := os.Stat(f); fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: mode %v, want 0600", f, fi.Mode().Perm())
		}
		out, _, err := runBin(t, bin, nil, "sync", "stats", f)
		if err == nil || !strings.Contains(out, "RESULT: TOO SHORT") {
			t.Fatalf("a 3 second run must give no verdict: %v\n%s", err, out)
		}
		out, _, err = runBin(t, bin, nil, "sync", "stats", "-min-samples", "3", "-min-duration", "1s", f)
		if err != nil || !strings.Contains(out, "RESULT: PASS") {
			t.Fatalf("%s: %v\n%s", f, err, out)
		}
	}
}

func TestSyncStatsExitCodesAndUsage(t *testing.T) {
	good := filepath.Join(t.TempDir(), "good.jsonl")
	var b strings.Builder
	for i := 0; i < 30; i++ {
		b.WriteString(`{"at":"2026-11-12T09:` + twoDigits(i) + `:00Z","goroutines":` + itoa(10+i) + `,"heap_inuse":1,"rss_kb":30000}` + "\n")
	}
	os.WriteFile(good, []byte(b.String()), 0o600)
	code, out, _ := runCLI(t, "", "sync", "stats", good)
	if code != 1 || !strings.Contains(out, "RESULT: FAIL") || !strings.Contains(out, "goroutines grew") {
		t.Fatalf("a leaking file must exit 1: %d\n%s", code, out)
	}
	if code, _, errOut := runCLI(t, "", "sync", "stats"); code != 2 || !strings.Contains(errOut, "usage") {
		t.Fatalf("%d %q", code, errOut)
	}
	if code, _, _ := runCLI(t, "", "sync", "stats", filepath.Join(t.TempDir(), "missing")); code != 2 {
		t.Fatalf("a missing file is a usage error: %d", code)
	}
}

func twoDigits(i int) string { return string(rune('0'+i/10)) + string(rune('0'+i%10)) }
func itoa(i int) string      { return twoDigits(i) }

// A sync server and a sync loop, each with the sampler on, must leave nothing
// running once they stop: no sampler, no connection, no round in flight.
func TestServeAndRunLeaveNoGoroutinesBehind(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	dir := t.TempDir()
	shared := filepath.Join(dir, "key")
	os.WriteFile(shared, []byte("leak-test-key-0123456789abcdef\n"), 0o600)
	dbA, dbB := filepath.Join(dir, "a.db"), filepath.Join(dir, "b.db")
	for _, db := range []string{dbA, dbB} {
		if code, _, errOut := runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"x `+filepath.Base(db)+`"}`); code != 0 {
			t.Fatal(errOut)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	var out, errOut lockedBuf
	done := make(chan int, 1)
	go func() {
		done <- cmdSyncServeContext(ctx, []string{"-db", dbB, "-addr", "127.0.0.1:0", "-key-file", shared, "-stats", filepath.Join(dir, "s.stats"), "-stats-every", "20ms"}, &out, &errOut)
	}()
	var url string
	for i := 0; i < 200 && url == ""; i++ {
		if m := regexp.MustCompile(`serving on (http://\S+)`).FindStringSubmatch(out.String()); m != nil {
			url = m[1]
		}
		time.Sleep(10 * time.Millisecond)
	}
	if url == "" {
		t.Fatalf("server did not start: %s", errOut.String())
	}
	var rout, rerr lockedBuf
	if code := cmdSyncRun([]string{"-db", dbA, "-peer", url, "-key-file", shared, "-every", "50ms", "-for", "600ms", "-stats", filepath.Join(dir, "r.stats"), "-stats-every", "20ms"}, &rout, &rerr); code != 0 {
		t.Fatalf("run: %d %s", code, rerr.String())
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("serve: %d %s", code, errOut.String())
	}
	http.DefaultTransport.(*http.Transport).CloseIdleConnections() // the client's keep-alive, which is the test's, not the node's
}
