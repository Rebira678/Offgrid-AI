package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"offgrid/internal/identity"
	"offgrid/internal/store/sqlstore"
	"offgrid/internal/syncer"
)

// syncBuf is a bytes.Buffer that the serving goroutine and the test can use at once.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// hubKey gives the hub its own device key through the same environment variable the CLI reads.
func hubKey(t *testing.T) {
	t.Helper()
	t.Setenv("OFFGRID_IDENTITY_KEY", filepath.Join(t.TempDir(), "hub.key"))
	var out, errOut bytes.Buffer
	if code := cmdIdentity([]string{"init"}, &out, &errOut); code != 0 {
		t.Fatalf("identity init: %d %s", code, errOut.String())
	}
}

func waitMatch(t *testing.T, s *syncBuf, re *regexp.Regexp) []string {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if m := re.FindStringSubmatch(s.String()); m != nil {
			return m
		}
	}
	t.Fatalf("never saw %v in:\n%s", re, s.String())
	return nil
}

func TestHubGroupAndServeCommands(t *testing.T) {
	hubKey(t)
	dir := filepath.Join(t.TempDir(), "hubdir")
	founder, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := cmdHub([]string{"group", "add", "-dir", dir, "-group", "crew", "-founder", founder.ID()}, &out, &errOut); code != 0 {
		t.Fatalf("group add: %d %s", code, errOut.String())
	}
	out.Reset()
	if code := cmdHub([]string{"group", "list", "-dir", dir}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "crew") || !strings.Contains(out.String(), "waiting for its founder") {
		t.Fatalf("group list: %d %q %s", code, out.String(), errOut.String())
	}

	ctx, cancel := context.WithCancel(context.Background())
	stdout, stderr := &syncBuf{}, &syncBuf{}
	done := make(chan int, 1)
	go func() { done <- runHubServe(ctx, []string{"-dir", dir, "-addr", "127.0.0.1:0"}, stdout, stderr) }()
	m := waitMatch(t, stdout, regexp.MustCompile(`serving on https://(\S+) \(mutual TLS 1\.3, device (dev_[0-9a-f]{32})\)`))
	addr, hubID := m[1], m[2]

	// the founder founds the group and syncs through the running hub
	st, err := sqlstore.Open(context.Background(), filepath.Join(t.TempDir(), "f.db"), sqlstore.Options{NodeID: "founder", Signer: founder})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Found(context.Background(), "founder"); err != nil {
		t.Fatal(err)
	}
	cl := &syncer.Client{Peer: "https://" + addr + "/g/crew", Node: "founder", Mutual: true,
		HTTP: syncer.NewTLSClient(syncer.ClientTLSConfig(founder, syncer.PinDevice(hubID), time.Now))}
	if rep, err := cl.Once(context.Background(), st); err != nil || rep.Pushed == 0 {
		t.Fatalf("founder through the running hub: %+v, %v", rep, err)
	}

	// a group added while it runs is served after SIGHUP
	if runtime.GOOS != "windows" {
		other, _ := identity.Generate()
		if code := cmdHub([]string{"group", "add", "-dir", dir, "-group", "farm", "-founder", other.ID()}, &out, &errOut); code != 0 {
			t.Fatalf("second group: %s", errOut.String())
		}
		if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
			t.Fatal(err)
		}
		waitMatch(t, stderr, regexp.MustCompile(`reload: 1 new groups`))
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the hub did not stop when its context ended")
	}
	if !strings.Contains(stdout.String(), "offgrid hub: stopped") {
		t.Fatalf("no stop line:\n%s", stdout.String())
	}
}

func TestHubCommandsExplainWhatIsMissing(t *testing.T) {
	var out, errOut bytes.Buffer
	t.Setenv("OFFGRID_IDENTITY_KEY", filepath.Join(t.TempDir(), "none.key"))
	if code := cmdHub([]string{"group", "list", "-dir", t.TempDir()}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "offgrid identity init") {
		t.Fatalf("no key: %d %q", code, errOut.String())
	}
	hubKey(t)
	errOut.Reset()
	if code := cmdHub([]string{"group", "list"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "-dir is required") {
		t.Fatalf("no dir: %d %q", code, errOut.String())
	}
	errOut.Reset()
	if code := cmdHub([]string{"group", "add", "-dir", t.TempDir(), "-group", "Bad Name", "-founder", "x"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "group name") {
		t.Fatalf("bad name: %d %q", code, errOut.String())
	}
	if code := cmdHub([]string{"nonsense"}, &out, &errOut); code != 2 {
		t.Fatalf("unknown subcommand: %d", code)
	}
	if code := cmdHub(nil, &out, &errOut); code != 2 {
		t.Fatalf("no subcommand: %d", code)
	}
}
