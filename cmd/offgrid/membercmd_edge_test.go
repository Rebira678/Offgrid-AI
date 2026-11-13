package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreFoundEdgeCases(t *testing.T) {
	db := dbPath(t)
	// Seed a task
	if code, _, e := runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"seed"}`); code != 0 {
		t.Fatalf("seed: %s", e)
	}

	// 1. Founding without device key
	t.Setenv("OFFGRID_IDENTITY_KEY", filepath.Join(t.TempDir(), "nonexistent.key"))
	code, out, errOut := runCLI(t, "", "store", "-db", db, "found", "-label", "test-laptop")
	if code == 0 || !strings.Contains(errOut, "identity init") {
		t.Fatalf("founding without key: code = %d\nout:\n%s\nerr:\n%s", code, out, errOut)
	}

	// 2. Founding without label defaults cleanly to "founder"
	useKey(t, "founder")
	code, out, errOut = runCLI(t, "", "store", "-db", db, "found")
	if code != 0 || !strings.Contains(out, "founded the membership as admin \"founder\"") {
		t.Fatalf("founding without label default: code = %d\nout:\n%s\nerr:\n%s", code, out, errOut)
	}

	// 3. Second founding must fail
	code, out, errOut = runCLI(t, "", "store", "-db", db, "found", "-label", "secondary-laptop")
	if code == 0 || !strings.Contains(errOut, "founded") {
		t.Fatalf("second founding unexpectedly succeeded: code = %d\nout:\n%s\nerr:\n%s", code, out, errOut)
	}
}

func TestStoreInviteFlagValidationEdgeCases(t *testing.T) {
	db := dbPath(t)
	useKey(t, "admin")
	if code, _, e := runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"seed"}`); code != 0 {
		t.Fatalf("seed: %s", e)
	}
	if code, _, e := runCLI(t, "", "store", "-db", db, "found", "-label", "admin-device"); code != 0 {
		t.Fatalf("found: %s", e)
	}

	// 1. Invalid role
	code, _, errOut := runCLI(t, "", "store", "-db", db, "invite", "-role", "superuser", "-label", "pi")
	if code == 0 || !strings.Contains(errOut, "admin or member") {
		t.Errorf("invalid role: code = %d, err = %s", code, errOut)
	}

	// 2. Missing label (empty label rejected)
	code, _, errOut = runCLI(t, "", "store", "-db", db, "invite", "-role", "member")
	if code == 0 || !strings.Contains(errOut, "label") {
		t.Errorf("missing label: code = %d, err = %s", code, errOut)
	}

	// 3. TTL exceeding 24 hours
	code, _, errOut = runCLI(t, "", "store", "-db", db, "invite", "-role", "member", "-label", "pi", "-ttl", "25h")
	if code == 0 || !strings.Contains(errOut, "between a moment and 24h") {
		t.Errorf("25h TTL: code = %d, err = %s", code, errOut)
	}

	// 4. Zero / negative TTL
	for _, ttl := range []string{"0s", "-10m"} {
		code, _, errOut = runCLI(t, "", "store", "-db", db, "invite", "-role", "member", "-label", "pi", "-ttl", ttl)
		if code == 0 || !strings.Contains(errOut, "between a moment and 24h") {
			t.Errorf("TTL %s: code = %d, err = %s", ttl, code, errOut)
		}
	}
}

func TestSyncEnrollCLIValidationEdges(t *testing.T) {
	useKey(t, "pi")

	// 1. Missing peer
	code, _, errOut := runCLI(t, "", "sync", "enroll", "-invite", "og-invite/1/x/y", "-label", "pi")
	if code != 2 || !strings.Contains(errOut, "-peer") {
		t.Errorf("missing -peer: code = %d, err = %s", code, errOut)
	}

	// 2. Missing invite
	code, _, errOut = runCLI(t, "", "sync", "enroll", "-peer", "http://127.0.0.1:7878", "-label", "pi")
	if code != 2 || !strings.Contains(errOut, "-invite") {
		t.Errorf("missing -invite: code = %d, err = %s", code, errOut)
	}

	// 3. Missing label
	code, _, errOut = runCLI(t, "", "sync", "enroll", "-peer", "http://127.0.0.1:7878", "-invite", "og-invite/1/x/y")
	if code != 2 || !strings.Contains(errOut, "-label") {
		t.Errorf("missing -label: code = %d, err = %s", code, errOut)
	}

	// 4. Malformed invite link
	code, _, errOut = runCLI(t, "", "sync", "enroll", "-peer", "http://127.0.0.1:7878", "-invite", "https://example.com/bogus", "-label", "pi")
	if code != 2 || !strings.Contains(errOut, "og-invite/1") {
		t.Errorf("malformed invite: code = %d, err = %s", code, errOut)
	}
}

func TestStoreMembersTwoAdminsClearsWarning(t *testing.T) {
	t.Setenv("OFFGRID_SYNC_KEY", strings.Repeat("k", 32))
	t.Setenv("OFFGRID_NODE", "laptop")
	keyAdmin1 := useKey(t, "admin1")
	db := dbPath(t)
	if code, _, e := runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"seed"}`); code != 0 {
		t.Fatalf("seed: %s", e)
	}
	if code, _, e := runCLI(t, "", "store", "-db", db, "found", "-label", "admin-laptop"); code != 0 {
		t.Fatalf("found: %s", e)
	}

	// Initially 1 admin: must display warning
	code, out, _ := runCLI(t, "", "store", "-db", db, "members")
	if code != 0 || !strings.Contains(out, "WARNING: only 1 admin") {
		t.Fatalf("expected 1-admin warning: %s", out)
	}

	// Invite a second admin
	code, out, e := runCLI(t, "", "store", "-db", db, "invite", "-role", "admin", "-label", "backup-admin")
	link := linkRE.FindString(out)
	if code != 0 || link == "" {
		t.Fatalf("invite admin: exit %d\n%s%s", code, out, e)
	}

	url := serveAdmin(t, db)

	// Second admin device enrolls
	useKey(t, "admin2")
	code, out, e = runCLI(t, "", "sync", "enroll", "-peer", url, "-invite", link, "-label", "backup-admin")
	if code != 0 {
		t.Fatalf("enroll second admin: exit %d\n%s%s", code, out, e)
	}

	// Re-check members on the admin node: now 2 admins, warning MUST BE GONE!
	t.Setenv("OFFGRID_IDENTITY_KEY", keyAdmin1)
	code, out, e = runCLI(t, "", "store", "-db", db, "members")
	if code != 0 {
		t.Fatalf("members query failed: exit %d\nout: %s\nerr: %s", code, out, e)
	}
	if strings.Contains(out, "WARNING") || strings.Contains(out, "only 1 admin") {
		t.Errorf("warning did not disappear after adding second admin:\n%s", out)
	}
	if !strings.Contains(out, "2 admins") {
		t.Errorf("output does not report 2 admins:\n%s", out)
	}
}
