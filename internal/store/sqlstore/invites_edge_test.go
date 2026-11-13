package sqlstore

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"offgrid/internal/identity"
)

func TestInviteEdgeTokensAndNormalization(t *testing.T) {
	ctx := context.Background()
	a, _, _ := admin(t)
	inv, err := a.CreateInvite(ctx, RoleMember, "edge-test", 15*time.Minute)
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}

	// 1. Valid token variations (spaces, lowercase, dashes)
	lower := strings.ToLower(inv.Token)
	withSpaces := strings.ReplaceAll(inv.Token, "-", "   ")
	withDashes := strings.ReplaceAll(inv.Token, "-", "----")

	for name, tok := range map[string]string{
		"lowercase":   lower,
		"with_spaces": withSpaces,
		"with_dashes": withDashes,
	} {
		norm := NormalizeToken(tok)
		if len(norm) != 24 {
			t.Errorf("%s normalized length = %d, want 24", name, len(norm))
		}
		if norm != strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(inv.Token)) {
			t.Errorf("%s did not normalize to clean raw token", name)
		}
	}

	// 2. Malformed token strings (non-base32 chars, truncated, huge)
	idb := newKey(t)
	malformed := []string{
		"",
		"SHORT",
		"123456789012345678901234", // '1', '8', '9', '0' are not RFC 4648 standard base32
		"ABCD-EFGH-IJKL-MNOP-QRST-UVW@",
		strings.Repeat("A", 10000),
	}

	for _, badTok := range malformed {
		p, _ := EnrollProof(badTok, idb, "pi")
		_, err := a.RedeemInvite(ctx, badTok, idb.PublicKey(), "pi", p)
		if !errors.Is(err, ErrInviteInvalid) {
			t.Errorf("token %q (len %d): err = %v, want ErrInviteInvalid", badTok, len(badTok), err)
		}
	}
}

func TestInviteDatabaseTriggersEnforceIntegrityAndImmutability(t *testing.T) {
	ctx := context.Background()
	a, _, _ := admin(t)
	inv, err := a.CreateInvite(ctx, RoleMember, "tamper-target", 15*time.Minute)
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}

	rawTok := NormalizeToken(inv.Token)
	invID := rawTok[:8]

	// 1. Trigger: cannot DELETE an unredeemed invite
	_, err = a.db.ExecContext(ctx, "DELETE FROM invites WHERE id = ?", invID)
	if err == nil || !strings.Contains(err.Error(), "invites are kept: deletes are not allowed") {
		t.Fatalf("delete unredeemed invite: err = %v, want trigger abort", err)
	}

	// 2. Trigger: cannot change role from member to admin
	_, err = a.db.ExecContext(ctx, "UPDATE invites SET role = 'admin' WHERE id = ?", invID)
	if err == nil || !strings.Contains(err.Error(), "only a first redemption may update a row") {
		t.Fatalf("elevating invite role: err = %v, want trigger abort", err)
	}

	// 3. Trigger: cannot extend expiry directly in DB
	_, err = a.db.ExecContext(ctx, "UPDATE invites SET expires = expires + 3600 WHERE id = ?", invID)
	if err == nil || !strings.Contains(err.Error(), "only a first redemption may update a row") {
		t.Fatalf("extending invite expires: err = %v, want trigger abort", err)
	}

	// 4. Trigger: cannot tamper with salt or hash
	_, err = a.db.ExecContext(ctx, "UPDATE invites SET salt = zeroblob(16) WHERE id = ?", invID)
	if err == nil || !strings.Contains(err.Error(), "only a first redemption may update a row") {
		t.Fatalf("tampering with salt: err = %v, want trigger abort", err)
	}

	// Redeem cleanly once
	idb := newKey(t)
	proof := proofFor(t, inv, idb, "clinic-pi")
	if _, err := a.RedeemInvite(ctx, inv.Token, idb.PublicKey(), "clinic-pi", proof); err != nil {
		t.Fatalf("RedeemInvite failed: %v", err)
	}

	// 5. Trigger: cannot un-redeem (set redeemed_at = NULL)
	_, err = a.db.ExecContext(ctx, "UPDATE invites SET redeemed_at = NULL WHERE id = ?", invID)
	if err == nil || !strings.Contains(err.Error(), "only a first redemption may update a row") {
		t.Fatalf("un-redeeming invite: err = %v, want trigger abort", err)
	}

	// 6. Trigger: cannot re-assign redeemed_by
	_, err = a.db.ExecContext(ctx, "UPDATE invites SET redeemed_by = 'hacker' WHERE id = ?", invID)
	if err == nil || !strings.Contains(err.Error(), "only a first redemption may update a row") {
		t.Fatalf("tampering redeemed_by: err = %v, want trigger abort", err)
	}

	// 7. Trigger: cannot delete a redeemed invite
	_, err = a.db.ExecContext(ctx, "DELETE FROM invites WHERE id = ?", invID)
	if err == nil || !strings.Contains(err.Error(), "invites are kept: deletes are not allowed") {
		t.Fatalf("deleting redeemed invite: err = %v, want trigger abort", err)
	}
}

func TestInviteProofAndReceiptEdgeTampering(t *testing.T) {
	ctx := context.Background()
	a, _, _ := admin(t)
	inv, err := a.CreateInvite(ctx, RoleMember, "proof-test", 15*time.Minute)
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}

	idb := newKey(t)
	idc := newKey(t)

	// 1. Proof signed for device B presented with device C's public key
	proofB := proofFor(t, inv, idb, "pi")
	_, err = a.RedeemInvite(ctx, inv.Token, idc.PublicKey(), "pi", proofB)
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("proof key mismatch: err = %v, want ErrBadSignature", err)
	}

	// 2. Proof signed for label "alpha" presented with label "beta"
	proofAlpha, _ := EnrollProof(inv.Token, idb, "alpha")
	_, err = a.RedeemInvite(ctx, inv.Token, idb.PublicKey(), "beta", proofAlpha)
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("proof label mismatch: err = %v, want ErrBadSignature", err)
	}

	// 3. Successful redemption gives valid receipt
	proofValid := proofFor(t, inv, idb, "alpha")
	en, err := a.RedeemInvite(ctx, inv.Token, idb.PublicKey(), "alpha", proofValid)
	if err != nil {
		t.Fatalf("RedeemInvite: %v", err)
	}

	// 4. Receipt verification against admin's public key
	msg := receiptMessage(inv.ID, idb.ID(), en.Role, "alpha")
	if err := identity.Verify(en.AdminKey, msg, en.Receipt); err != nil {
		t.Fatalf("receipt verification failed: %v", err)
	}

	// Tampered receipt signature
	tampered := make([]byte, len(en.Receipt))
	copy(tampered, en.Receipt)
	tampered[0] ^= 0xff
	if err := identity.Verify(en.AdminKey, msg, tampered); err == nil {
		t.Fatal("accepted bit-flipped receipt signature")
	}

	// Tampered receipt message (wrong label)
	wrongMsg := receiptMessage(inv.ID, idb.ID(), en.Role, "beta")
	if err := identity.Verify(en.AdminKey, wrongMsg, en.Receipt); err == nil {
		t.Fatal("accepted receipt with wrong message/label")
	}
}

func TestInviteConcurrentRedemptionRaceUnderHeavyContention(t *testing.T) {
	ctx := context.Background()
	a, _, _ := admin(t)
	inv, err := a.CreateInvite(ctx, RoleMember, "race-test", 15*time.Minute)
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}

	const goroutines = 24
	var successes int64
	var failures int64

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			dev := newKey(t)
			label := "contestant"
			proof := proofFor(t, inv, dev, label)

			<-start
			_, err := a.RedeemInvite(ctx, inv.Token, dev.PublicKey(), label, proof)
			if err == nil {
				atomic.AddInt64(&successes, 1)
			} else {
				if errors.Is(err, ErrInviteUsed) || errors.Is(err, ErrInviteInvalid) {
					atomic.AddInt64(&failures, 1)
				} else {
					t.Errorf("unexpected error on concurrent race: %v", err)
				}
			}
		}(i)
	}

	close(start)
	wg.Wait()

	if successes != 1 {
		t.Fatalf("expected exactly 1 successful redemption, got %d (failures: %d)", successes, failures)
	}
	if failures != goroutines-1 {
		t.Fatalf("expected %d failures, got %d", goroutines-1, failures)
	}

	// Database members must contain founder + 1 enrolled device
	rep := mustMembers(t, a)
	if len(rep.Members) != 2 {
		t.Fatalf("expected exactly 2 members in store, got %d", len(rep.Members))
	}
}
