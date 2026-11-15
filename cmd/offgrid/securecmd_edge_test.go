package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSecureCLIMutualTLSNeedsDeviceKey(t *testing.T) {
	db := dbPath(t)
	// Seed a task
	if code, _, e := runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"seed"}`); code != 0 {
		t.Fatalf("seed: %s", e)
	}

	// No identity key configured
	t.Setenv("OFFGRID_IDENTITY_KEY", filepath.Join(t.TempDir(), "nonexistent.key"))
	code, _, errOut := runCLI(t, "", "sync", "once", "-db", db, "-peer", "https://127.0.0.1:7878", "-timeout", "1s")
	if code == 0 || !strings.Contains(errOut, "identity init") {
		t.Errorf("expected failure requiring device key, got code %d, err:\n%s", code, errOut)
	}
}

func TestSecureCLIFirstSyncWithoutPeerDeviceFlagFails(t *testing.T) {
	useKey(t, "worker")
	freshDB := filepath.Join(t.TempDir(), "worker.db")

	// Attempting sync against https:// peer without -peer-device on a blank database
	code, _, errOut := runCLI(t, "", "sync", "once", "-db", freshDB, "-create", "-peer", "https://127.0.0.1:7878", "-timeout", "1s")
	if code == 0 || !strings.Contains(errOut, "-peer-device") {
		t.Errorf("expected -peer-device requirement error, got code %d, err:\n%s", code, errOut)
	}
}

func TestSecureCLIRefuseHMACWhenMembershipExists(t *testing.T) {
	useKey(t, "admin")
	db := dbPath(t)
	if code, _, e := runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"seed"}`); code != 0 {
		t.Fatalf("seed: %s", e)
	}
	if code, _, e := runCLI(t, "", "store", "-db", db, "found", "-label", "laptop"); code != 0 {
		t.Fatalf("found: %s", e)
	}

	// Group has membership, http:// peer must be rejected
	t.Setenv("OFFGRID_SYNC_KEY", strings.Repeat("k", 32))
	code, _, errOut := runCLI(t, "", "sync", "once", "-db", db, "-peer", "http://127.0.0.1:7878", "-timeout", "1s")
	if code == 0 || !strings.Contains(errOut, "shared-key (HMAC) path is off") {
		t.Errorf("expected refusal of plain http with membership, got code %d, err:\n%s", code, errOut)
	}
}

func TestSecureCLIEnrollAddrWithoutMembershipRefused(t *testing.T) {
	useKey(t, "unfounded")
	db := dbPath(t)
	if code, _, e := runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"seed"}`); code != 0 {
		t.Fatalf("seed: %s", e)
	}

	// No membership founded: -enroll-addr must be rejected
	t.Setenv("OFFGRID_SYNC_KEY", strings.Repeat("k", 32))
	code, _, errOut := runCLI(t, "", "sync", "serve", "-db", db, "-addr", "127.0.0.1:0", "-enroll-addr", "127.0.0.1:0")
	if code != 2 || !strings.Contains(errOut, "-enroll-addr needs a membership") {
		t.Errorf("expected -enroll-addr needs a membership error, got code %d, err:\n%s", code, errOut)
	}
}

func TestSecureCLIEnrollRequiresHTTPSPeer(t *testing.T) {
	keyAdmin := useKey(t, "admin")
	db := dbPath(t)
	if code, _, e := runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"seed"}`); code != 0 {
		t.Fatalf("seed: %s", e)
	}
	if code, _, e := runCLI(t, "", "store", "-db", db, "found", "-label", "admin-device"); code != 0 {
		t.Fatalf("found: %s", e)
	}

	code, out, e := runCLI(t, "", "store", "-db", db, "invite", "-role", "member", "-label", "pi")
	if code != 0 {
		t.Fatalf("invite failed: exit %d\n%s%s", code, out, e)
	}
	link := linkRE.FindString(out)
	if link == "" {
		t.Fatalf("no invite link in output:\n%s", out)
	}

	useKey(t, "pi")
	code, _, errOut := runCLI(t, "", "sync", "enroll", "-peer", "http://127.0.0.1:7878", "-invite", link, "-label", "pi")
	if code != 2 || !strings.Contains(errOut, "must go over TLS") {
		t.Errorf("expected TLS requirement error for enroll, got code %d, err:\n%s", code, errOut)
	}
	_ = keyAdmin
}

func TestSecureCLISyncRunValidationEdges(t *testing.T) {
	useKey(t, "runner")
	db := dbPath(t)
	if code, _, e := runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"seed"}`); code != 0 {
		t.Fatalf("seed: %s", e)
	}

	// 1. Negative -every must fail with code 2
	code, _, errOut := runCLI(t, "", "sync", "run", "-db", db, "-peer", "https://127.0.0.1:7878", "-every", "-1s")
	if code != 2 || !strings.Contains(errOut, "-every must be positive") {
		t.Errorf("expected negative every error, got code %d, err:\n%s", code, errOut)
	}

	// 2. Zero -round-timeout must fail with code 2
	code, _, errOut = runCLI(t, "", "sync", "run", "-db", db, "-peer", "https://127.0.0.1:7878", "-round-timeout", "0s")
	if code != 2 || !strings.Contains(errOut, "-round-timeout must be positive") {
		t.Errorf("expected zero round-timeout error, got code %d, err:\n%s", code, errOut)
	}
}
