package main

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"offgrid/internal/identity"
)

func TestHubCLIHelpAndSubcommandRouting(t *testing.T) {
	var out, errOut bytes.Buffer

	// 1. No arguments -> exit 2, prints help to stderr
	if code := cmdHub(nil, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "offgrid hub: a relay run by the site owner") {
		t.Fatalf("no args: code=%d errOut=%q", code, errOut.String())
	}

	// 2. Help flags -> exit 0, prints help to stderr
	for _, flag := range []string{"help", "-h", "--help"} {
		out.Reset()
		errOut.Reset()
		if code := cmdHub([]string{flag}, &out, &errOut); code != 0 || !strings.Contains(errOut.String(), "offgrid hub: a relay run by the site owner") {
			t.Fatalf("help flag %q: code=%d errOut=%q", flag, code, errOut.String())
		}
	}

	// 3. Unknown subcommands -> exit 2
	out.Reset()
	errOut.Reset()
	if code := cmdHub([]string{"badcmd"}, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "unknown hub command") {
		t.Fatalf("unknown subcommand: code=%d errOut=%q", code, errOut.String())
	}
}

func TestHubGroupCLIFlagValidationEdgeCases(t *testing.T) {
	hubKey(t)
	dir := t.TempDir()
	founder, _ := identity.Generate()

	// 1. hub group with no arguments or wrong subcommands
	var out, errOut bytes.Buffer
	if code := cmdHub([]string{"group"}, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "use  hub group add  or  hub group list") {
		t.Fatalf("group no args: code=%d errOut=%q", code, errOut.String())
	}

	errOut.Reset()
	if code := cmdHub([]string{"group", "unknown"}, &out, &errOut); code != 2 {
		t.Fatalf("group unknown: code=%d", code)
	}

	// 2. hub group add missing flags
	errOut.Reset()
	if code := cmdHub([]string{"group", "add", "-group", "crew", "-founder", founder.ID()}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "-dir is required") {
		t.Fatalf("add missing dir: code=%d errOut=%q", code, errOut.String())
	}

	errOut.Reset()
	if code := cmdHub([]string{"group", "add", "-dir", dir, "-founder", founder.ID()}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "group name") {
		t.Fatalf("add missing group: code=%d errOut=%q", code, errOut.String())
	}

	errOut.Reset()
	if code := cmdHub([]string{"group", "add", "-dir", dir, "-group", "crew"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "founder") {
		t.Fatalf("add missing founder: code=%d errOut=%q", code, errOut.String())
	}

	// 3. hub group add duplicate group
	out.Reset()
	errOut.Reset()
	if code := cmdHub([]string{"group", "add", "-dir", dir, "-group", "team-alpha", "-founder", founder.ID()}, &out, &errOut); code != 0 {
		t.Fatalf("first add: code=%d errOut=%q", code, errOut.String())
	}

	out.Reset()
	errOut.Reset()
	if code := cmdHub([]string{"group", "add", "-dir", dir, "-group", "team-alpha", "-founder", founder.ID()}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "already exists") {
		t.Fatalf("duplicate add: code=%d errOut=%q", code, errOut.String())
	}

	// 4. hub group list empty directory
	emptyDir := t.TempDir()
	out.Reset()
	errOut.Reset()
	if code := cmdHub([]string{"group", "list", "-dir", emptyDir}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "no groups") {
		t.Fatalf("list empty dir: code=%d out=%q errOut=%q", code, out.String(), errOut.String())
	}
}

func TestHubServeCLIFlagValidationAndAddressCollision(t *testing.T) {
	hubKey(t)
	dir := t.TempDir()

	// 1. Missing -dir flag
	var out, errOut bytes.Buffer
	if code := cmdHub([]string{"serve"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "-dir is required") {
		t.Fatalf("serve missing dir: code=%d errOut=%q", code, errOut.String())
	}

	// 2. Unknown / malformed flags
	out.Reset()
	errOut.Reset()
	if code := cmdHub([]string{"serve", "-dir", dir, "-invalid-flag-xyz"}, &out, &errOut); code != 2 {
		t.Fatalf("serve unknown flag: code=%d", code)
	}

	// 3. Port collision (address already bound)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	out.Reset()
	errOut.Reset()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	code := runHubServe(ctx, []string{"-dir", dir, "-addr", ln.Addr().String()}, &out, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "bind") && !strings.Contains(errOut.String(), "address already in use") {
		t.Fatalf("serve port collision: code=%d errOut=%q", code, errOut.String())
	}
}
