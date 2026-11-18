package main

import (
	"bytes"
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"offgrid/internal/identity"
	"offgrid/internal/store/sqlstore"
	"offgrid/internal/syncer"
)

func TestHubMultiGroupIsolationCLIEndToEnd(t *testing.T) {
	hubKey(t)
	dir := filepath.Join(t.TempDir(), "hub-multi")
	ctx := context.Background()

	founderA, _ := identity.Generate()
	founderB, _ := identity.Generate()

	var out, errOut bytes.Buffer
	// Add group alpha
	if code := cmdHub([]string{"group", "add", "-dir", dir, "-group", "group-alpha", "-founder", founderA.ID()}, &out, &errOut); code != 0 {
		t.Fatalf("add alpha: %d %s", code, errOut.String())
	}
	// Add group beta
	out.Reset()
	errOut.Reset()
	if code := cmdHub([]string{"group", "add", "-dir", dir, "-group", "group-beta", "-founder", founderB.ID()}, &out, &errOut); code != 0 {
		t.Fatalf("add beta: %d %s", code, errOut.String())
	}

	// Start hub server
	serveCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stdout, stderr := &syncBuf{}, &syncBuf{}
	done := make(chan int, 1)
	go func() {
		done <- runHubServe(serveCtx, []string{"-dir", dir, "-addr", "127.0.0.1:0"}, stdout, stderr)
	}()

	m := waitMatch(t, stdout, regexp.MustCompile(`serving on https://(\S+) \(mutual TLS 1\.3, device (dev_[0-9a-f]{32})\)`))
	addr, hubID := m[1], m[2]

	// 1. Founder A founds group-alpha
	stA, err := sqlstore.Open(ctx, filepath.Join(t.TempDir(), "fa.db"), sqlstore.Options{NodeID: "founder-a", Signer: founderA})
	if err != nil {
		t.Fatal(err)
	}
	defer stA.Close()
	if err := stA.Found(ctx, "founder a"); err != nil {
		t.Fatal(err)
	}

	clA := &syncer.Client{
		Peer:   "https://" + addr + "/g/group-alpha",
		Node:   "founder-a",
		Mutual: true,
		HTTP:   syncer.NewTLSClient(syncer.ClientTLSConfig(founderA, syncer.PinDevice(hubID), time.Now)),
	}
	if rep, err := clA.Once(ctx, stA); err != nil || rep.Pushed == 0 {
		t.Fatalf("founder A sync alpha: %+v, %v", rep, err)
	}

	// 2. Founder B founds group-beta
	stB, err := sqlstore.Open(ctx, filepath.Join(t.TempDir(), "fb.db"), sqlstore.Options{NodeID: "founder-b", Signer: founderB})
	if err != nil {
		t.Fatal(err)
	}
	defer stB.Close()
	if err := stB.Found(ctx, "founder b"); err != nil {
		t.Fatal(err)
	}

	clB := &syncer.Client{
		Peer:   "https://" + addr + "/g/group-beta",
		Node:   "founder-b",
		Mutual: true,
		HTTP:   syncer.NewTLSClient(syncer.ClientTLSConfig(founderB, syncer.PinDevice(hubID), time.Now)),
	}
	if rep, err := clB.Once(ctx, stB); err != nil || rep.Pushed == 0 {
		t.Fatalf("founder B sync beta: %+v, %v", rep, err)
	}

	// 3. Founder A attempts to access group-beta -> rejected with 404 no_such_group
	clCross := &syncer.Client{
		Peer:   "https://" + addr + "/g/group-beta",
		Node:   "founder-a",
		Mutual: true,
		HTTP:   syncer.NewTLSClient(syncer.ClientTLSConfig(founderA, syncer.PinDevice(hubID), time.Now)),
	}
	if _, err := clCross.Once(ctx, stA); err == nil {
		t.Fatal("cross-group unauthorized sync should fail")
	}

	// 4. Group list inspects both groups
	out.Reset()
	errOut.Reset()
	if code := cmdHub([]string{"group", "list", "-dir", dir}, &out, &errOut); code != 0 {
		t.Fatalf("group list: %d %s", code, errOut.String())
	}
	listOutput := out.String()
	if !strings.Contains(listOutput, "group-alpha") || !strings.Contains(listOutput, "1 members") {
		t.Fatalf("expected group-alpha with 1 members, got:\n%s", listOutput)
	}
	if !strings.Contains(listOutput, "group-beta") || !strings.Contains(listOutput, "1 members") {
		t.Fatalf("expected group-beta with 1 members, got:\n%s", listOutput)
	}

	// Graceful shutdown
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("serve exited with %d: %s", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hub serve failed to stop within timeout")
	}
}
