package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

func TestMemberAddPayloadValidationEdgeCases(t *testing.T) {
	ida := newKey(t)
	devID := ida.ID()
	pub := ida.PublicKey()

	// 1. Valid payload passes
	valid := memberAddPayload{
		Device:    devID,
		PublicKey: pub,
		Role:      RoleMember,
		Label:     "valid-device",
	}
	if err := checkMemberAdd(valid); err != nil {
		t.Fatalf("valid payload failed: %v", err)
	}

	// 2. Device ID does not match public key
	wrongDev := valid
	wrongDev.Device = "dev_00000000000000000000000000000000"
	if err := checkMemberAdd(wrongDev); err == nil || !strings.Contains(err.Error(), "the device ID is not the hash") {
		t.Errorf("mismatched device ID: err = %v", err)
	}

	// 3. Malformed / corrupted public key hex
	badKeys := []string{
		"",
		"not-hex",
		"04deadbeef", // too short
		pub[:len(pub)-2],
		pub + "ff",
	}
	for _, bk := range badKeys {
		bad := valid
		bad.PublicKey = bk
		if err := checkMemberAdd(bad); err == nil {
			t.Errorf("accepted bad public key %q", bk)
		}
	}

	// 4. Invalid roles
	for _, r := range []Role{"", "root", "guest", "owner", "superadmin"} {
		bad := valid
		bad.Role = r
		if err := checkMemberAdd(bad); err == nil {
			t.Errorf("accepted invalid role %q", r)
		}
	}

	// 5. Label edges
	emptyLabel := valid
	emptyLabel.Label = ""
	if err := checkMemberAdd(emptyLabel); err == nil {
		t.Error("accepted empty label")
	}

	hugeLabel := valid
	hugeLabel.Label = strings.Repeat("A", 1000)
	if err := checkMemberAdd(hugeLabel); err == nil {
		t.Error("accepted 1000-char label exceeding bounds")
	}
}

func TestMemberDuplicateAndReAddRefusals(t *testing.T) {
	ctx := context.Background()
	a, ida, _ := admin(t)
	idb := newKey(t)

	// Add member B cleanly
	if err := a.AddMember(ctx, idb.PublicKey(), RoleMember, "nurse-pi"); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	// 1. Re-adding device B with same key and role
	err := a.AddMember(ctx, idb.PublicKey(), RoleMember, "nurse-pi")
	if !errors.Is(err, ErrAlreadyMember) {
		t.Fatalf("re-add same device: err = %v, want ErrAlreadyMember", err)
	}

	// 2. Re-adding device B with upgraded role (admin) - roles are fixed on Day 38
	err = a.AddMember(ctx, idb.PublicKey(), RoleAdmin, "nurse-pi-promoted")
	if !errors.Is(err, ErrAlreadyMember) {
		t.Fatalf("promoting existing device: err = %v, want ErrAlreadyMember", err)
	}

	// 3. Founder cannot be re-added as member
	err = a.AddMember(ctx, ida.PublicKey(), RoleMember, "founder-as-member")
	if !errors.Is(err, ErrAlreadyMember) {
		t.Fatalf("re-adding founder: err = %v, want ErrAlreadyMember", err)
	}
}

func TestMemberRemoteArrivalRejectsUnauthorizedAdds(t *testing.T) {
	ctx := context.Background()
	a, _, _ := admin(t)
	idb := newKey(t)

	// Admin A adds member B
	if err := a.AddMember(ctx, idb.PublicKey(), RoleMember, "member-device"); err != nil {
		t.Fatal(err)
	}

	// Member B creates its store
	b, _ := member(t, "member-node", idb)
	pull(t, b, a, 10)

	// Verify B knows it is a member
	repB := mustMembers(t, b)
	if roleOf(repB, idb) != RoleMember {
		t.Fatalf("node B has role %q, want member", roleOf(repB, idb))
	}

	// Member B tries to add device C directly (not authorized)
	idc := newKey(t)
	err := b.AddMember(ctx, idc.PublicKey(), RoleMember, "intruder")
	if !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("non-admin adding member locally: err = %v, want ErrNotAuthorized", err)
	}

	// Member B directly records a member.add into its own log using recordAdd
	err = b.write(ctx, func(tx *sql.Tx) error {
		return b.recordAdd(ctx, tx, idc.PublicKey(), RoleMember, "forged-add")
	})
	if err != nil {
		t.Fatalf("b.write recordAdd: %v", err)
	}

	// Pull from Member B to Admin A: A must refuse with ErrNotAuthorized!
	opsB := opsOf(t, b)
	_, err = a.ApplyRemote(ctx, opsB)
	if !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("ApplyRemote of non-admin member.add: err = %v, want ErrNotAuthorized", err)
	}

	// Verify store state remained clean
	repA := mustMembers(t, a)
	if len(repA.Members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(repA.Members))
	}
	if len(repA.Problems) != 0 {
		t.Fatalf("expected 0 problems, got %+v", repA.Problems)
	}
}

func TestMembershipReportAdminCountAndFounder(t *testing.T) {
	ctx := context.Background()
	a, ida, _ := admin(t)

	rep1 := mustMembers(t, a)
	if rep1.Admins() != 1 {
		t.Errorf("Admins() = %d, want 1", rep1.Admins())
	}
	if rep1.Founder() != ida.ID() {
		t.Errorf("Founder() = %q, want %q", rep1.Founder(), ida.ID())
	}

	// Add second admin
	idAdmin2 := newKey(t)
	if err := a.AddMember(ctx, idAdmin2.PublicKey(), RoleAdmin, "backup-admin"); err != nil {
		t.Fatal(err)
	}

	// Add 3 regular members
	for i := 0; i < 3; i++ {
		m := newKey(t)
		if err := a.AddMember(ctx, m.PublicKey(), RoleMember, "worker"); err != nil {
			t.Fatal(err)
		}
	}

	rep2 := mustMembers(t, a)
	if rep2.Admins() != 2 {
		t.Errorf("Admins() = %d, want 2", rep2.Admins())
	}
	if len(rep2.Members) != 5 {
		t.Errorf("len(Members) = %d, want 5", len(rep2.Members))
	}
	if rep2.Founder() != ida.ID() {
		t.Errorf("Founder() = %q, want %q", rep2.Founder(), ida.ID())
	}
}
