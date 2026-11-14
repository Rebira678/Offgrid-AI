package syncer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"offgrid/internal/identity"
	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
)

// TestSyncWireStrangerPushForbidden verifies that an un-enrolled stranger's
// ops are refused with 403 Forbidden when pushed over the sync wire, even if
// the stranger holds the valid HMAC shared sync key.
func TestSyncWireStrangerPushForbidden(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{t0}

	admin, _ := newSignedStore(t, "laptop", clk)
	if err := admin.Found(ctx, "founder"); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(Handler(admin, ServerOptions{Key: key, Now: clk.now}))
	t.Cleanup(srv.Close)

	// Stranger has a valid HMAC sync key, but its device identity was never enrolled.
	stranger, strangerID := newSignedStore(t, "stranger-node", clk)
	add(t, stranger, "stranger task")

	cl := &Client{
		Peer:     srv.URL,
		Key:      key,
		Node:     "stranger-node",
		DeviceID: strangerID.ID(),
		Now:      clk.now,
	}

	_, err := cl.Once(ctx, stranger)
	if err == nil {
		t.Fatal("expected push from un-enrolled stranger to fail, got nil error")
	}

	var rerr *RemoteError
	if !errors.As(err, &rerr) {
		t.Fatalf("expected *RemoteError, got %T: %v", err, err)
	}
	if rerr.Status != http.StatusForbidden || rerr.Code != CodeNotAuthorized {
		t.Fatalf("expected 403 %s, got %d %s: %s", CodeNotAuthorized, rerr.Status, rerr.Code, rerr.Msg)
	}

	// Admin must not have the stranger's task in its materialized view.
	tasks, err := admin.List(ctx, task.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Fatalf("admin view contains %d tasks; want 0", len(tasks))
	}
}

// TestSyncWireRevokedDeviceOpStoredAndVoided verifies that ops signed by a revoked
// device after its cutoff are accepted over the wire (200 OK) to preserve hash-chain
// continuity, but are quarantined into op_void and omitted from the materialized view.
func TestSyncWireRevokedDeviceOpStoredAndVoided(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{t0}

	admin, ida := newSignedStore(t, "admin-laptop", clk)
	if err := admin.Found(ctx, "founder"); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(Handler(admin, ServerOptions{Key: key, Enroller: admin, Now: clk.now}))
	t.Cleanup(srv.Close)

	// Worker enrolls cleanly
	workerID, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	inv, err := admin.CreateInvite(ctx, sqlstore.RoleMember, "worker-pi", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := sqlstore.EnrollProof(inv.Token, workerID, "worker-pi")
	if err != nil {
		t.Fatal(err)
	}

	req := EnrollRequest{
		Token:     inv.Token,
		PublicKey: workerID.PublicKey(),
		Label:     "worker-pi",
		Proof:     proof,
	}
	if _, err := Enroll(ctx, nil, srv.URL, req); err != nil {
		t.Fatalf("enroll failed: %v", err)
	}

	worker, err := sqlstore.Open(ctx, filepath.Join(t.TempDir(), "worker.db"), sqlstore.Options{
		NodeID: "worker-node",
		Now:    clk.now,
		Signer: workerID,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { worker.Close() })

	clWorker := &Client{
		Peer:     srv.URL,
		Key:      key,
		Node:     "worker-node",
		DeviceID: workerID.ID(),
		Now:      clk.now,
	}

	// 1. Worker creates legitimate pre-revocation task
	tPre := add(t, worker, "worker pre-revoke task")

	// Sync pre-revoke task to admin
	if _, err := clWorker.Once(ctx, worker); err != nil {
		t.Fatalf("initial worker sync failed: %v", err)
	}

	// Admin must show the pre-revoke task in its view
	tasks, err := admin.List(ctx, task.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].ID != tPre.ID {
		t.Fatalf("admin tasks want 1 (id=%s), got %d", tPre.ID, len(tasks))
	}

	// 2. Admin revokes worker device
	rev, err := admin.Revoke(ctx, workerID.ID())
	if err != nil {
		t.Fatalf("revoke failed: %v", err)
	}
	if rev.Device != workerID.ID() {
		t.Fatalf("unexpected revoked device: %s", rev.Device)
	}

	// 3. Worker creates a post-revocation task and notes
	tPost := add(t, worker, "worker post-revoke rogue task")
	if _, err := worker.AddNote(ctx, tPre.ID, "rogue note from revoked device"); err != nil {
		t.Fatal(err)
	}

	// Worker syncs to admin: push must SUCCEED (200 OK) so the log chain isn't broken
	rep, err := clWorker.Once(ctx, worker)
	if err != nil {
		t.Fatalf("sync after revoke should succeed with 200 OK, got: %v", err)
	}
	if rep.Pushed < 2 {
		t.Fatalf("expected at least 2 ops pushed, got %d", rep.Pushed)
	}

	// 4. Verify admin's view: post-revoke task and note must NOT exist!
	adminTasks, err := admin.List(ctx, task.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(adminTasks) != 1 {
		t.Fatalf("admin view must only have 1 active task, got %d", len(adminTasks))
	}
	if adminTasks[0].ID != tPre.ID {
		t.Fatalf("expected pre-revoke task %s, got %s", tPre.ID, adminTasks[0].ID)
	}

	// Verify pre-revoke task does NOT have the rogue note
	retrievedPre, err := admin.Get(ctx, tPre.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(retrievedPre.Notes) != 0 {
		t.Fatalf("expected 0 notes on pre-revoke task, got %d: %+v", len(retrievedPre.Notes), retrievedPre.Notes)
	}

	// Verify post-revoke task is completely missing from Get
	if _, err := admin.Get(ctx, tPost.ID); !errors.Is(err, task.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for post-revoke task, got: %v", err)
	}

	// Rebuild the admin view from raw log: post-revoke op must still be excluded
	if _, err := admin.Rebuild(ctx); err != nil {
		t.Fatalf("rebuild view failed: %v", err)
	}
	adminTasksAfterRebuild, err := admin.List(ctx, task.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(adminTasksAfterRebuild) != 1 {
		t.Fatalf("post-rebuild admin view must have 1 task, got %d", len(adminTasksAfterRebuild))
	}

	// Founder remains founder
	members, err := admin.Members(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if members.Founder() != ida.ID() {
		t.Fatalf("expected founder %s, got %s", ida.ID(), members.Founder())
	}
}

// TestSyncWireUnauthorizedRevokeAttemptForbidden verifies that a non-admin
// attempting to issue a member.revoke op is refused by the store and cannot push.
func TestSyncWireUnauthorizedRevokeAttemptForbidden(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{t0}

	admin, ida := newSignedStore(t, "admin-laptop", clk)
	if err := admin.Found(ctx, "founder"); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(Handler(admin, ServerOptions{Key: key, Enroller: admin, Now: clk.now}))
	t.Cleanup(srv.Close)

	// Non-admin worker enrolls
	workerID, _ := identity.Generate()
	inv, _ := admin.CreateInvite(ctx, sqlstore.RoleMember, "worker-pi", time.Hour)
	proof, _ := sqlstore.EnrollProof(inv.Token, workerID, "worker-pi")
	req := EnrollRequest{Token: inv.Token, PublicKey: workerID.PublicKey(), Label: "worker-pi", Proof: proof}
	if _, err := Enroll(ctx, nil, srv.URL, req); err != nil {
		t.Fatalf("enroll failed: %v", err)
	}

	worker, err := sqlstore.Open(ctx, filepath.Join(t.TempDir(), "worker.db"), sqlstore.Options{
		NodeID: "worker-node",
		Now:    clk.now,
		Signer: workerID,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { worker.Close() })

	clWorker := &Client{
		Peer:     srv.URL,
		Key:      key,
		Node:     "worker-node",
		DeviceID: workerID.ID(),
		Now:      clk.now,
	}

	// Worker syncs initially so it has the membership state
	if _, err := clWorker.Once(ctx, worker); err != nil {
		t.Fatalf("worker initial sync failed: %v", err)
	}

	// Worker attempts to revoke the admin directly on worker's local store
	_, err = worker.Revoke(ctx, ida.ID())
	if err == nil {
		t.Fatal("worker local revoke of admin should fail with not an admin")
	}
	if !strings.Contains(err.Error(), "not an admin") {
		t.Fatalf("expected 'not an admin' error, got: %v", err)
	}
}

// TestSyncWireBatchWithInterleavedRevocationAndConvergence verifies that when
// multiple replicas exchange ops involving member adds, task updates, and revocations,
// a third un-involved peer pulling from the server arrives at the identical state hash,
// identical active tasks, and identical void ops.
func TestSyncWireBatchWithInterleavedRevocationAndConvergence(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{t0}

	admin, ida := newSignedStore(t, "admin-laptop", clk)
	if err := admin.Found(ctx, "founder"); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(Handler(admin, ServerOptions{Key: key, Enroller: admin, Now: clk.now}))
	t.Cleanup(srv.Close)

	// Worker enrolls
	workerID, _ := identity.Generate()
	inv, _ := admin.CreateInvite(ctx, sqlstore.RoleMember, "worker-pi", time.Hour)
	proof, _ := sqlstore.EnrollProof(inv.Token, workerID, "worker-pi")
	req := EnrollRequest{Token: inv.Token, PublicKey: workerID.PublicKey(), Label: "worker-pi", Proof: proof}
	if _, err := Enroll(ctx, nil, srv.URL, req); err != nil {
		t.Fatalf("enroll failed: %v", err)
	}

	worker, err := sqlstore.Open(ctx, filepath.Join(t.TempDir(), "worker.db"), sqlstore.Options{
		NodeID: "worker-node",
		Now:    clk.now,
		Signer: workerID,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { worker.Close() })

	clWorker := &Client{
		Peer:     srv.URL,
		Key:      key,
		Node:     "worker-node",
		DeviceID: workerID.ID(),
		Now:      clk.now,
	}

	// Worker creates pre-revocation task
	t1 := add(t, worker, "legitimate-task-1")
	if _, err := clWorker.Once(ctx, worker); err != nil {
		t.Fatal(err)
	}

	// Admin revokes worker
	if _, err := admin.Revoke(ctx, workerID.ID()); err != nil {
		t.Fatal(err)
	}

	// Worker creates post-revocation rogue task
	add(t, worker, "rogue-task-2")
	if _, err := clWorker.Once(ctx, worker); err != nil {
		t.Fatal(err)
	}

	// Third peer (tab) syncs directly from admin server
	tabID, _ := identity.Generate()
	tab, err := sqlstore.Open(ctx, filepath.Join(t.TempDir(), "tab.db"), sqlstore.Options{
		NodeID: "tab-node",
		Now:    clk.now,
		Signer: tabID,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tab.Close() })

	clTab := &Client{
		Peer:     srv.URL,
		Key:      key,
		Node:     "tab-node",
		DeviceID: tabID.ID(),
		Now:      clk.now,
	}

	if _, err := clTab.Once(ctx, tab); err != nil {
		t.Fatalf("tab sync failed: %v", err)
	}

	// Verification: tab must hold identical state hash to admin
	hAdmin := hash(t, admin)
	hTab := hash(t, tab)
	if hAdmin != hTab {
		t.Fatalf("state hash mismatch:\nadmin: %s\ntab:   %s", hAdmin, hTab)
	}

	// Tab tasks view must contain ONLY t1, never rogue-task-2
	tabTasks, err := tab.List(ctx, task.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tabTasks) != 1 || tabTasks[0].ID != t1.ID {
		t.Fatalf("tab tasks want 1 (id=%s), got %d (%+v)", t1.ID, len(tabTasks), tabTasks)
	}

	// Tab must record the void op in its VoidOps list
	tabVoid, err := tab.VoidOps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tabVoid) != 1 {
		t.Fatalf("tab VoidOps want 1, got %d: %+v", len(tabVoid), tabVoid)
	}
	if tabVoid[0].Device != workerID.ID() {
		t.Errorf("tab void op device = %s, want %s", tabVoid[0].Device, workerID.ID())
	}
	_ = ida
}
