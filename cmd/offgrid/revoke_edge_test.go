package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreRevokeCLIFlagAndPermissionEdges(t *testing.T) {
	t.Setenv("OFFGRID_SYNC_KEY", strings.Repeat("k", 32))
	t.Setenv("OFFGRID_NODE", "admin-laptop")
	db := dbPath(t)
	adminKey := useKey(t, "admin")

	// Seed task and found membership
	if code, _, e := runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"seed"}`); code != 0 {
		t.Fatalf("seed: %s", e)
	}
	if code, _, e := runCLI(t, "", "store", "-db", db, "found", "-label", "admin-laptop"); code != 0 {
		t.Fatalf("found: %s", e)
	}

	// Invite a member (worker-pi)
	code, out, e := runCLI(t, "", "store", "-db", db, "invite", "-role", "member", "-label", "worker-pi")
	link := linkRE.FindString(out)
	if code != 0 || link == "" {
		t.Fatalf("invite failed: exit %d\n%s%s", code, out, e)
	}

	url := serveAdmin(t, db)

	// Enroll worker-pi
	workerKey := useKey(t, "worker")
	if code, _, e := runCLI(t, "", "sync", "enroll", "-peer", url, "-invite", link, "-label", "worker-pi"); code != 0 {
		t.Fatalf("enroll worker: exit %d\n%s", code, e)
	}

	// Fetch worker device ID
	workerDev := strings.Fields(func() string { _, o, _ := runCLI(t, "", "identity", "show"); return o }())
	var workerID string
	for _, f := range workerDev {
		if strings.HasPrefix(f, "dev_") {
			workerID = f
		}
	}
	if workerID == "" {
		t.Fatalf("unable to find worker device ID in %q", workerDev)
	}

	// 1. Missing -device flag on revoke command (must return exit 2)
	t.Setenv("OFFGRID_IDENTITY_KEY", adminKey)
	t.Setenv("OFFGRID_NODE", "admin-laptop")
	code, _, errOut := runCLI(t, "", "store", "-db", db, "revoke")
	if code != 2 || !strings.Contains(errOut, "-device is required") {
		t.Errorf("missing -device flag: expected exit 2, got %d, err: %s", code, errOut)
	}

	// 2. Non-existent device key when revoking
	t.Setenv("OFFGRID_IDENTITY_KEY", filepath.Join(t.TempDir(), "nonexistent.key"))
	code, _, errOut = runCLI(t, "", "store", "-db", db, "revoke", "-device", workerID)
	if code == 0 || !strings.Contains(errOut, "identity init") {
		t.Errorf("missing key on revoke: expected error with identity init, got code %d, err: %s", code, errOut)
	}

	// 3. Non-admin attempting to revoke on its own database
	t.Setenv("OFFGRID_IDENTITY_KEY", workerKey)
	t.Setenv("OFFGRID_NODE", "worker-node")
	dbWorker := dbPath(t)
	if code, _, e := runCLI(t, "", "sync", "once", "-db", dbWorker, "-create", "-peer", url); code != 0 {
		t.Fatalf("worker initial sync: exit %d\n%s", code, e)
	}
	code, _, errOut = runCLI(t, "", "store", "-db", dbWorker, "revoke", "-device", workerID)
	if code == 0 || !strings.Contains(errOut, "not an admin") {
		t.Errorf("non-admin revoking: expected error naming not an admin, got code %d, err: %s", code, errOut)
	}

	// 4. Admin attempting to revoke the founder
	t.Setenv("OFFGRID_IDENTITY_KEY", adminKey)
	t.Setenv("OFFGRID_NODE", "admin-laptop")
	founderDev := strings.Fields(func() string { _, o, _ := runCLI(t, "", "identity", "show"); return o }())
	var founderID string
	for _, f := range founderDev {
		if strings.HasPrefix(f, "dev_") {
			founderID = f
		}
	}
	code, _, errOut = runCLI(t, "", "store", "-db", db, "revoke", "-device", founderID)
	if code == 0 || !strings.Contains(errOut, "founder and cannot be revoked") {
		t.Errorf("revoking founder: expected error, got code %d, err: %s", code, errOut)
	}

	// 5. Admin revoking an unknown / unenrolled device
	code, _, errOut = runCLI(t, "", "store", "-db", db, "revoke", "-device", "dev_0000000000000000000000000000000000000000000000000000000000000000")
	if code == 0 || !strings.Contains(errOut, "not a member") {
		t.Errorf("revoking non-member: expected error, got code %d, err: %s", code, errOut)
	}

	// 6. Valid revocation succeeds
	code, out, errOut = runCLI(t, "", "store", "-db", db, "revoke", "-device", workerID)
	if code != 0 || !strings.Contains(out, "revoked") {
		t.Fatalf("valid revocation failed: code %d\nout: %s\nerr: %s", code, out, errOut)
	}

	// 7. Revoking the same device a second time fails with already revoked
	code, _, errOut = runCLI(t, "", "store", "-db", db, "revoke", "-device", workerID)
	if code == 0 || !strings.Contains(errOut, "already revoked") {
		t.Errorf("revoking twice: expected already revoked, got code %d, err: %s", code, errOut)
	}

	// 8. store members must display REVOKED tag
	code, out, _ = runCLI(t, "", "store", "-db", db, "members")
	if code != 0 || !strings.Contains(out, "REVOKED") || !strings.Contains(out, "worker-pi") {
		t.Errorf("store members missing REVOKED tag: %s", out)
	}
}
