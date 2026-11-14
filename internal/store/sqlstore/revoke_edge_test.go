package sqlstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"offgrid/internal/identity"
	"offgrid/internal/task"
)

func TestOpVoidTriggerImmutability(t *testing.T) {
	ctx := context.Background()
	e := trio(t)
	create(t, e.b, "task-before")
	pull(t, e.a, e.b, 50)

	if _, err := e.a.Revoke(ctx, e.idb.ID()); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	create(t, e.b, "task-after")
	pull(t, e.a, e.b, 50)

	// Verify that an op is recorded in op_void
	var count int
	if err := e.a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM op_void`).Scan(&count); err != nil || count == 0 {
		t.Fatalf("expected op_void rows, got count=%d, err=%v", count, err)
	}

	// 1. Direct raw SQL DELETE must be aborted by trigger
	_, err := e.a.db.ExecContext(ctx, `DELETE FROM op_void`)
	if err == nil || !strings.Contains(err.Error(), "op_void is append-only: deletes are not allowed") {
		t.Fatalf("expected delete to abort via trigger, got: %v", err)
	}

	// 2. Direct raw SQL UPDATE must be aborted by trigger
	_, err = e.a.db.ExecContext(ctx, `UPDATE op_void SET reason = 'tampered'`)
	if err == nil || !strings.Contains(err.Error(), "op_void is append-only: updates are not allowed") {
		t.Fatalf("expected update to abort via trigger, got: %v", err)
	}
}

func TestCutoffHashCaseInsensitivityAndValidation(t *testing.T) {
	// 1. Lowercase vs uppercase hex SHA-256 hash
	lowerHash := "a1b2c3d4e5f67890a1b2c3d4e5f67890a1b2c3d4e5f67890a1b2c3d4e5f67890"
	upperHash := strings.ToUpper(lowerHash)

	if !isHex64(lowerHash) {
		t.Errorf("expected isHex64 to accept lowercase hash")
	}
	if !isHex64(upperHash) {
		t.Errorf("expected isHex64 to accept uppercase hash")
	}

	// 2. Validation error cases in checkMemberRevoke
	cases := []struct {
		name    string
		payload memberRevokePayload
		wantErr string
	}{
		{
			name:    "invalid device prefix",
			payload: memberRevokePayload{Device: "invalid_id", Cutoffs: []Cutoff{}},
			wantErr: "not a device ID",
		},
		{
			name: "cutoff without replica",
			payload: memberRevokePayload{
				Device:  "dev_0123456789abcdef",
				Cutoffs: []Cutoff{{Node: "", Seq: 1, Hash: lowerHash}},
			},
			wantErr: "names no replica",
		},
		{
			name: "duplicate cutoffs for same replica",
			payload: memberRevokePayload{
				Device: "dev_0123456789abcdef",
				Cutoffs: []Cutoff{
					{Node: "node-a", Seq: 1, Hash: lowerHash},
					{Node: "node-a", Seq: 2, Hash: lowerHash},
				},
			},
			wantErr: "two cutoffs for replica",
		},
		{
			name: "negative sequence cutoff",
			payload: memberRevokePayload{
				Device:  "dev_0123456789abcdef",
				Cutoffs: []Cutoff{{Node: "node-a", Seq: -1, Hash: lowerHash}},
			},
			wantErr: "negative sequence",
		},
		{
			name: "seq 0 with non-empty hash",
			payload: memberRevokePayload{
				Device:  "dev_0123456789abcdef",
				Cutoffs: []Cutoff{{Node: "node-a", Seq: 0, Hash: lowerHash}},
			},
			wantErr: "sequence 0 has no hash",
		},
		{
			name: "invalid non-hex hash length",
			payload: memberRevokePayload{
				Device:  "dev_0123456789abcdef",
				Cutoffs: []Cutoff{{Node: "node-a", Seq: 1, Hash: "tooshort"}},
			},
			wantErr: "not a SHA-256 in hex",
		},
		{
			name: "invalid non-hex characters in hash",
			payload: memberRevokePayload{
				Device:  "dev_0123456789abcdef",
				Cutoffs: []Cutoff{{Node: "node-a", Seq: 1, Hash: strings.Repeat("z", 64)}},
			},
			wantErr: "not a SHA-256 in hex",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkMemberRevoke(tc.payload)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("got %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestRevokeDeviceMultipleReplicasCutoff(t *testing.T) {
	ctx := context.Background()
	adminID := newKey(t)
	workerID := newKey(t)

	adminStore, clkAdmin := member(t, "admin-node", adminID)
	if err := adminStore.Found(ctx, "founder"); err != nil {
		t.Fatal(err)
	}
	clkAdmin.tick(time.Second)
	if err := adminStore.AddMember(ctx, workerID.PublicKey(), RoleMember, "multi-node-worker"); err != nil {
		t.Fatal(err)
	}

	// Worker signs operations on 3 separate nodes: replica-1, replica-2, replica-3
	w1, clkW1 := member(t, "replica-1", workerID)
	w2, clkW2 := member(t, "replica-2", workerID)
	w3, clkW3 := member(t, "replica-3", workerID)

	pull(t, w1, adminStore, 50)
	pull(t, w2, adminStore, 50)
	pull(t, w3, adminStore, 50)

	clkW1.tick(time.Second)
	create(t, w1, "task-r1-1")
	create(t, w1, "task-r1-2")

	clkW2.tick(time.Second)
	create(t, w2, "task-r2-1")

	clkW3.tick(time.Second)
	create(t, w3, "task-r3-1")
	create(t, w3, "task-r3-2")
	create(t, w3, "task-r3-3")

	// Sync all replicas into admin node
	pull(t, adminStore, w1, 50)
	pull(t, adminStore, w2, 50)
	pull(t, adminStore, w3, 50)

	// Admin revokes worker
	clkAdmin.tick(time.Minute)
	rev, err := adminStore.Revoke(ctx, workerID.ID())
	if err != nil {
		t.Fatalf("Revoke failed: %v", err)
	}

	// Must contain cutoffs for all 3 replicas
	if len(rev.Cutoffs) != 3 {
		t.Fatalf("expected cutoffs for 3 replicas, got %d: %+v", len(rev.Cutoffs), rev.Cutoffs)
	}

	cutoffMap := map[string]Cutoff{}
	for _, c := range rev.Cutoffs {
		cutoffMap[c.Node] = c
	}

	if c, ok := cutoffMap["replica-1"]; !ok || c.Seq < 2 || len(c.Hash) != 64 {
		t.Errorf("unexpected cutoff for replica-1: %+v", c)
	}
	if c, ok := cutoffMap["replica-2"]; !ok || c.Seq < 1 || len(c.Hash) != 64 {
		t.Errorf("unexpected cutoff for replica-2: %+v", c)
	}
	if c, ok := cutoffMap["replica-3"]; !ok || c.Seq < 3 || len(c.Hash) != 64 {
		t.Errorf("unexpected cutoff for replica-3: %+v", c)
	}
}

func TestConcurrentRevocationsAndMembershipRace(t *testing.T) {
	ctx := context.Background()
	adminID := newKey(t)
	adminStore, clk := member(t, "admin", adminID)
	if err := adminStore.Found(ctx, "founder"); err != nil {
		t.Fatal(err)
	}

	// Enroll 8 members
	var members []*identity.Identity
	for i := 0; i < 8; i++ {
		clk.tick(time.Second)
		mID := newKey(t)
		members = append(members, mID)
		if err := adminStore.AddMember(ctx, mID.PublicKey(), RoleMember, fmt.Sprintf("worker-%d", i)); err != nil {
			t.Fatal(err)
		}
	}

	// Concurrently attempt to revoke all 8 members from multiple goroutines
	var wg sync.WaitGroup
	errs := make([]error, len(members))
	for i, m := range members {
		wg.Add(1)
		go func(idx int, id *identity.Identity) {
			defer wg.Done()
			_, errs[idx] = adminStore.Revoke(ctx, id.ID())
		}(i, m)
	}
	wg.Wait()

	// Every single revocation must have succeeded
	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d revocation failed: %v", i, err)
		}
	}

	rep, err := adminStore.Members(ctx)
	if err != nil {
		t.Fatal(err)
	}

	for _, m := range rep.Members {
		if m.Device != adminID.ID() && !m.Revoked {
			t.Errorf("device %s should be revoked", m.Device)
		}
	}
}

func TestAuthorizeEdgeMatrix(t *testing.T) {
	founder := newKey(t)
	admin2 := newKey(t)
	member1 := newKey(t)
	stranger := newKey(t)

	now := time.Now()
	events := []memberEvent{
		{
			Kind:   KindMemberAdd,
			Node:   "node-founder",
			Seq:    1,
			TS:     now,
			Signer: founder.ID(),
			Add: memberAddPayload{
				Device:    founder.ID(),
				PublicKey: founder.PublicKey(),
				Role:      RoleAdmin,
				Label:     "founder",
			},
		},
		{
			Kind:   KindMemberAdd,
			Node:   "node-founder",
			Seq:    2,
			TS:     now.Add(time.Second),
			Signer: founder.ID(),
			Add: memberAddPayload{
				Device:    admin2.ID(),
				PublicKey: admin2.PublicKey(),
				Role:      RoleAdmin,
				Label:     "admin-2",
			},
		},
		{
			Kind:   KindMemberAdd,
			Node:   "node-founder",
			Seq:    3,
			TS:     now.Add(2 * time.Second),
			Signer: founder.ID(),
			Add: memberAddPayload{
				Device:    member1.ID(),
				PublicKey: member1.PublicKey(),
				Role:      RoleMember,
				Label:     "member-1",
			},
		},
		{
			Kind:   KindMemberRevoke,
			Node:   "node-founder",
			Seq:    4,
			TS:     now.Add(3 * time.Second),
			Signer: founder.ID(),
			Revoke: memberRevokePayload{
				Device: member1.ID(),
				Cutoffs: []Cutoff{
					{Node: "node-m1", Seq: 10, Hash: strings.Repeat("a", 64)},
				},
			},
		},
	}

	st := foldState(events, nil)

	// 1. Unsigned op: always allowed (Authorize does not judge attribution)
	if err := Authorize(st, OpRef{Node: "node-m1", Seq: 99, Kind: KindTaskCreated, Signer: ""}); err != nil {
		t.Errorf("unsigned op should be allowed by Authorize, got: %v", err)
	}

	// 2. Stranger: ErrNotAuthorized
	if err := Authorize(st, OpRef{Node: "stranger-node", Seq: 1, Kind: KindTaskCreated, Signer: stranger.ID()}); !errors.Is(err, ErrNotAuthorized) {
		t.Errorf("stranger op should be rejected with ErrNotAuthorized, got: %v", err)
	}

	// 3. Member within cutoff: allowed
	if err := Authorize(st, OpRef{Node: "node-m1", Seq: 10, Kind: KindTaskCreated, Signer: member1.ID()}); err != nil {
		t.Errorf("op within cutoff should be allowed, got: %v", err)
	}

	// 4. Member past cutoff: ErrRevoked
	if err := Authorize(st, OpRef{Node: "node-m1", Seq: 11, Kind: KindTaskCreated, Signer: member1.ID()}); !errors.Is(err, ErrRevoked) {
		t.Errorf("op past cutoff should return ErrRevoked, got: %v", err)
	}

	// 5. Member on unmentioned replica: ErrRevoked
	if err := Authorize(st, OpRef{Node: "other-node", Seq: 1, Kind: KindTaskCreated, Signer: member1.ID()}); !errors.Is(err, ErrRevoked) {
		t.Errorf("op on unmentioned replica should return ErrRevoked, got: %v", err)
	}

	// 6. Regular member attempting admin op (member.add): ErrNotAuthorized
	if err := Authorize(st, OpRef{Node: "node-m1", Seq: 1, Kind: KindMemberAdd, Signer: member1.ID()}); !errors.Is(err, ErrNotAuthorized) {
		t.Errorf("regular member attempting member.add should return ErrNotAuthorized, got: %v", err)
	}

	// 7. Regular member attempting member.revoke: ErrNotAuthorized
	if err := Authorize(st, OpRef{Node: "node-m1", Seq: 1, Kind: KindMemberRevoke, Signer: member1.ID()}); !errors.Is(err, ErrNotAuthorized) {
		t.Errorf("regular member attempting member.revoke should return ErrNotAuthorized, got: %v", err)
	}

	// 8. Admin attempting member.add: allowed
	if err := Authorize(st, OpRef{Node: "node-founder", Seq: 5, Kind: KindMemberAdd, Signer: admin2.ID()}); err != nil {
		t.Errorf("admin attempting member.add should be allowed, got: %v", err)
	}
}

func TestRefoldViewMultiEntityConvergence(t *testing.T) {
	ctx := context.Background()
	e := trio(t)

	// Pi creates task-1, adds a note to it, and creates task-2
	create(t, e.b, "clean-water")
	tasks, err := e.b.List(ctx, task.Filter{})
	if err != nil || len(tasks) == 0 {
		t.Fatalf("list tasks: %v", err)
	}
	taskID := tasks[0].ID

	if _, err := e.b.AddNote(ctx, taskID, "water pump inspected"); err != nil {
		t.Fatalf("add note: %v", err)
	}
	create(t, e.b, "solar-inverter")

	// Sync into admin
	pull(t, e.a, e.b, 50)

	// Admin revokes Pi: cutoffs freeze Pi's work up to this point
	e.clkA.tick(time.Minute)
	if _, err := e.a.Revoke(ctx, e.idb.ID()); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// Pi writes additional ops after being revoked
	e.clkB.tick(time.Minute)
	create(t, e.b, "rogue-task-after-cutoff")
	if _, err := e.b.AddNote(ctx, taskID, "rogue-note"); err != nil {
		t.Fatalf("add rogue note: %v", err)
	}

	// Sync again into admin
	pull(t, e.a, e.b, 50)

	// Verify view: "rogue-task-after-cutoff" MUST NOT exist in e.a view
	titles := viewTitles(t, e.a)
	if strings.Contains(titles, "rogue-task-after-cutoff") {
		t.Fatalf("view contains voided task: %s", titles)
	}
	if !strings.Contains(titles, "clean-water") || !strings.Contains(titles, "solar-inverter") {
		t.Fatalf("view dropped valid pre-cutoff tasks: %s", titles)
	}

	// Verify notes: "rogue-note" must not exist in clean-water task notes
	freshTasks, err := e.a.List(ctx, task.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range freshTasks {
		if tk.ID == taskID {
			for _, n := range tk.Notes {
				if strings.Contains(n.Text, "rogue-note") {
					t.Fatalf("view contains voided note: %s", n.Text)
				}
			}
		}
	}
}

// TestAuthorizeZeroAllocationsOnHotPath verifies that Authorize does not
// allocate on the heap for authorized ops or static ErrRevoked verdicts.
func TestAuthorizeZeroAllocationsOnHotPath(t *testing.T) {
	founder, _ := identity.Generate()
	worker, _ := identity.Generate()

	events := []memberEvent{
		{
			Kind:    KindMemberAdd,
			Node:    "admin",
			Seq:     1,
			TS:      time.Unix(1700000000, 0).UTC(),
			Counter: 1,
			Signer:  founder.ID(),
			Add: memberAddPayload{
				Device:    founder.ID(),
				PublicKey: founder.PublicKey(),
				Role:      RoleAdmin,
				Label:     "founder",
			},
		},
		{
			Kind:    KindMemberAdd,
			Node:    "admin",
			Seq:     2,
			TS:      time.Unix(1700000010, 0).UTC(),
			Counter: 2,
			Signer:  founder.ID(),
			Add: memberAddPayload{
				Device:    worker.ID(),
				PublicKey: worker.PublicKey(),
				Role:      RoleMember,
				Label:     "worker",
			},
		},
		{
			Kind:    KindMemberRevoke,
			Node:    "admin",
			Seq:     3,
			TS:      time.Unix(1700000100, 0).UTC(),
			Counter: 3,
			Signer:  founder.ID(),
			Revoke: memberRevokePayload{
				Device: worker.ID(),
				Cutoffs: []Cutoff{
					{Node: "worker-node", Seq: 10, Hash: strings.Repeat("a", 64)},
				},
			},
		},
	}
	st := foldState(events, nil)
	opAllowed := OpRef{Node: "worker-node", Seq: 5, Kind: "task.create", Signer: worker.ID()}
	opRevoked := OpRef{Node: "worker-node", Seq: 15, Kind: "task.create", Signer: worker.ID()}

	allocsAllowed := testing.AllocsPerRun(1000, func() {
		if err := Authorize(st, opAllowed); err != nil {
			t.Fatal(err)
		}
	})
	if allocsAllowed > 0 {
		t.Errorf("Authorize allowed: got %f allocs, want 0", allocsAllowed)
	}

	allocsRevoked := testing.AllocsPerRun(1000, func() {
		if err := Authorize(st, opRevoked); !errors.Is(err, ErrRevoked) {
			t.Fatal(err)
		}
	})
	if allocsRevoked > 0 {
		t.Errorf("Authorize revoked: got %f allocs, want 0", allocsRevoked)
	}
}

// TestEquivocationIsolationAcrossReplicas proves that when a revoked device
// writes an equivocated chain on replica-1, only replica-1 has its cutoffs
// dropped to seq 0; replica-2 remains valid up to its legitimate cutoff.
func TestEquivocationIsolationAcrossReplicas(t *testing.T) {
	ctx := context.Background()
	e := trio(t)

	// Pi writes 2 tasks on replica-1 and 2 tasks on replica-2
	// e.b is replica "pi". We'll also open a second store for pi on "pi-backup"
	piBackupID := e.idb
	piBackup, clkPiBackup := member(t, "pi-backup", piBackupID)

	create(t, e.b, "pi-task-node-pi")
	create(t, piBackup, "pi-backup-task-1")
	create(t, piBackup, "pi-backup-task-2")

	// Sync pi and piBackup into laptop (e.a)
	pull(t, e.a, e.b, 50)
	pull(t, e.a, piBackup, 50)

	// Get legitimate chain hashes
	held, err := loadHeld(ctx, e.a.db, []memberEvent{
		{
			Kind: KindMemberRevoke,
			Revoke: memberRevokePayload{
				Device: e.idb.ID(),
				Cutoffs: []Cutoff{
					{Node: "pi", Seq: 1, Hash: strings.Repeat("0", 64)},
					{Node: "pi-backup", Seq: 2, Hash: strings.Repeat("0", 64)},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	realHashPi, _ := held("pi", 1)
	realHashPiBackup, _ := held("pi-backup", 2)

	// Admin revokes Pi:
	// For "pi": provide a bogus/equivocated hash!
	// For "pi-backup": provide the real honest hash!
	bogusHash := strings.Repeat("f", 64)
	cutoffs := []Cutoff{
		{Node: "pi", Seq: 1, Hash: bogusHash},
		{Node: "pi-backup", Seq: 2, Hash: realHashPiBackup},
	}

	if err := e.a.RevokeAt(ctx, e.idb.ID(), cutoffs); err != nil {
		t.Fatalf("RevokeAt: %v", err)
	}

	// Effective cutoffs: "pi" must be reduced to seq 0 (all voided),
	// but "pi-backup" must preserve seq 2!
	mRep, err := e.a.Members(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var piMember *Member
	for i := range mRep.Members {
		if mRep.Members[i].Device == e.idb.ID() {
			piMember = &mRep.Members[i]
		}
	}
	if piMember == nil || !piMember.Revoked {
		t.Fatalf("expected pi to be revoked")
	}

	for _, c := range piMember.Cutoffs {
		if c.Node == "pi" && c.Seq != 0 {
			t.Errorf("expected equivocation on 'pi' to set seq=0, got %d", c.Seq)
		}
		if c.Node == "pi-backup" && c.Seq != 2 {
			t.Errorf("expected honest 'pi-backup' to maintain seq=2, got %d", c.Seq)
		}
	}

	// Verify view: "pi-task-node-pi" is voided and absent,
	// while "pi-backup-task-1" and "pi-backup-task-2" remain in view!
	titles := viewTitles(t, e.a)
	if strings.Contains(titles, "pi-task-node-pi") {
		t.Fatalf("equivocated task on 'pi' was not voided: %s", titles)
	}
	if !strings.Contains(titles, "pi-backup-task-1") || !strings.Contains(titles, "pi-backup-task-2") {
		t.Fatalf("honest tasks on 'pi-backup' were incorrectly voided: %s", titles)
	}
	_ = realHashPi
	_ = clkPiBackup
}

// TestRevokedKeyNeverReEnrolls proves that once a public key / device ID
// has been revoked, attempting to re-enroll it with a new invite fails deterministically,
// both locally (ErrAlreadyMember) and across the sync wire (ErrNotAuthorized).
func TestRevokedKeyNeverReEnrolls(t *testing.T) {
	ctx := context.Background()
	e := trio(t)

	// Admin revokes Pi
	if _, err := e.a.Revoke(ctx, e.idb.ID()); err != nil {
		t.Fatalf("revoke pi: %v", err)
	}

	// 1. Founder attempts to locally re-add Pi's public key with a new member.add op:
	// Store refuses immediately with ErrAlreadyMember.
	err := e.a.AddMember(ctx, e.idb.PublicKey(), RoleMember, "pi-reborn")
	if !errors.Is(err, ErrAlreadyMember) {
		t.Fatalf("expected ErrAlreadyMember on re-adding revoked device, got: %v", err)
	}

	// 2. Over the sync wire / replay: another admin replica that only saw the
	// founding op attempts to add Pi's key. When that op arrives at e.a
	// (which already has Pi enrolled and revoked), checkMembershipFresh rejects it.
	remoteAdmin, clkRemote := member(t, "remote-admin", e.ida)
	// Pull only the founding op (seq 1) from e.a
	foundOps, _, err := e.a.OpsSince(ctx, Summary{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remoteAdmin.ApplyRemote(ctx, foundOps); err != nil {
		t.Fatalf("remoteAdmin sync founding: %v", err)
	}

	// remoteAdmin independently adds Pi
	clkRemote.tick(time.Second)
	if err := remoteAdmin.AddMember(ctx, e.idb.PublicKey(), RoleMember, "pi-reborn"); err != nil {
		t.Fatalf("remoteAdmin AddMember: %v", err)
	}

	// remoteAdmin attempts to push its add op to e.a: must be rejected!
	ops, _, err := remoteAdmin.OpsSince(ctx, Summary{"remote-admin": 0, "laptop": 1}, 100)
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.a.ApplyRemote(ctx, ops)
	if err == nil {
		t.Fatalf("expected ApplyRemote of re-enrolled revoked key to fail, got res: %+v", res)
	}
	if !errors.Is(err, ErrNotAuthorized) || !strings.Contains(err.Error(), "revoked key never comes back") {
		t.Fatalf("expected ErrNotAuthorized with 'revoked key never comes back', got: %v", err)
	}
}
