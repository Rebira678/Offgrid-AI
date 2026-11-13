package sqlstore

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"offgrid/internal/identity"
)

// admin is a founded replica with its clock.
func admin(t *testing.T) (*Store, *identity.Identity, *clock) {
	t.Helper()
	id := newKey(t)
	s, clk := member(t, "laptop", id)
	if err := s.Found(context.Background(), "founder"); err != nil {
		t.Fatal(err)
	}
	return s, id, clk
}

func proofFor(t *testing.T, inv Invite, id *identity.Identity, label string) []byte {
	t.Helper()
	p, err := EnrollProof(inv.Token, id, label)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestInviteIsOneTimeShortLivedAndStoredHashed(t *testing.T) {
	ctx := context.Background()
	a, ida, _ := admin(t)
	inv, err := a.CreateInvite(ctx, RoleMember, "for the Pi", 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Admin != ida.ID() || inv.Role != RoleMember || !inv.Expires.After(epoch) || len(inv.Token) < 24 {
		t.Fatalf("%+v", inv)
	}
	// The token is shown once and never stored: not in the table, not in any text column.
	raw := strings.NewReplacer("-", "", " ", "").Replace(inv.Token)
	var dump string
	rows, err := a.db.QueryContext(ctx, `SELECT id || '|' || hex(salt) || '|' || hex(hash) || '|' || role || '|' || label FROM invites`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var l string
		rows.Scan(&l)
		dump += l + "\n"
	}
	rows.Close()
	if strings.Contains(strings.ToUpper(dump), raw[8:]) || strings.Contains(dump, inv.Token) {
		t.Fatalf("the secret part of the token is stored in the clear:\n%s", dump)
	}
	var saltLen, hashLen int
	a.db.QueryRowContext(ctx, `SELECT length(salt), length(hash) FROM invites`).Scan(&saltLen, &hashLen)
	if saltLen != 16 || hashLen != 32 {
		t.Fatalf("salt %d bytes, hash %d bytes; want a 16-byte random salt and a SHA-256 hash", saltLen, hashLen)
	}
	// Two invites never share a salt or an ID.
	inv2, _ := a.CreateInvite(ctx, RoleMember, "another", time.Minute)
	if inv2.Token == inv.Token {
		t.Fatal("two invites with one token")
	}
	var distinct int
	a.db.QueryRowContext(ctx, `SELECT count(DISTINCT salt) FROM invites`).Scan(&distinct)
	if distinct != 2 {
		t.Fatal("salts repeat")
	}
}

func TestInviteLifetimeIsBounded(t *testing.T) {
	a, _, _ := admin(t)
	for _, ttl := range []time.Duration{0, -time.Minute, 25 * time.Hour} {
		if _, err := a.CreateInvite(context.Background(), RoleMember, "x", ttl); err == nil {
			t.Errorf("ttl %s accepted: a stolen invite is a stolen enrollment until it expires, so the limit is not optional", ttl)
		}
	}
}

func TestOnlyAnAdminMakesInvites(t *testing.T) {
	ctx := context.Background()
	a, _, _ := admin(t)
	idb := newKey(t)
	a.AddMember(ctx, idb.PublicKey(), RoleMember, "pi")
	b, _ := member(t, "pi", idb)
	pull(t, b, a, 10)
	if _, err := b.CreateInvite(ctx, RoleMember, "x", time.Minute); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("a member making an invite: err = %v", err)
	}
	s, _ := freshStore(t)
	if _, err := s.CreateInvite(ctx, RoleMember, "x", time.Minute); err == nil {
		t.Fatal("a replica with no key cannot make invites")
	}
}

func TestRedeemEnrollsTheDeviceWithASignedMemberAdd(t *testing.T) {
	ctx := context.Background()
	a, ida, _ := admin(t)
	inv, _ := a.CreateInvite(ctx, RoleMember, "for the Pi", time.Hour)
	idb := newKey(t)
	en, err := a.RedeemInvite(ctx, inv.Token, idb.PublicKey(), "Pi at the clinic", proofFor(t, inv, idb, "Pi at the clinic"))
	if err != nil {
		t.Fatal(err)
	}
	if en.Device != idb.ID() || en.Role != RoleMember || en.AdminDevice != ida.ID() || en.AdminKey != ida.PublicKey() {
		t.Fatalf("%+v", en)
	}
	// The admin signed a receipt the NEW device can check against the admin it pinned from the invite.
	if err := identity.Verify(en.AdminKey, receiptMessage(inv.ID, idb.ID(), en.Role, "Pi at the clinic"), en.Receipt); err != nil {
		t.Fatalf("receipt: %v", err)
	}
	rep := mustMembers(t, a)
	if roleOf(rep, idb) != RoleMember || len(rep.Members) != 2 {
		t.Fatalf("%+v", rep)
	}
	// The member.add op is in the log, signed by the admin, and syncs like any op.
	b, _ := member(t, "pi", idb)
	pull(t, b, a, 10)
	if rb := mustMembers(t, b); roleOf(rb, idb) != RoleMember || len(rb.Problems) != 0 {
		t.Fatalf("%+v", rb)
	}
	for _, m := range rep.Members {
		if m.Device == idb.ID() && (m.AddedBy != ida.ID() || m.Label != "Pi at the clinic") {
			t.Fatalf("%+v", m)
		}
	}
}

func TestAnInviteWorksOnlyOnce(t *testing.T) {
	ctx := context.Background()
	a, _, _ := admin(t)
	inv, _ := a.CreateInvite(ctx, RoleMember, "x", time.Hour)
	idb, idc := newKey(t), newKey(t)
	if _, err := a.RedeemInvite(ctx, inv.Token, idb.PublicKey(), "first", proofFor(t, inv, idb, "first")); err != nil {
		t.Fatal(err)
	}
	_, err := a.RedeemInvite(ctx, inv.Token, idc.PublicKey(), "second", proofFor(t, inv, idc, "second"))
	if !errors.Is(err, ErrInviteUsed) {
		t.Fatalf("err = %v, want ErrInviteUsed", err)
	}
	if rep := mustMembers(t, a); len(rep.Members) != 2 || roleOf(rep, idc) != "" {
		t.Fatalf("a reused invite enrolled a device: %+v", rep)
	}
	// The database itself refuses to un-redeem or delete an invite.
	for _, q := range []string{`UPDATE invites SET redeemed_at = NULL`, `UPDATE invites SET expires = expires + 1000`, `DELETE FROM invites`} {
		if _, err := a.db.ExecContext(ctx, q); err == nil || !strings.Contains(err.Error(), "invites") {
			t.Errorf("%s: err = %v, want the trigger to refuse", q, err)
		}
	}
}

func TestAnExpiredInviteIsRefusedAndNotBurned(t *testing.T) {
	ctx := context.Background()
	a, _, clk := admin(t)
	inv, _ := a.CreateInvite(ctx, RoleMember, "x", 10*time.Minute)
	idb := newKey(t)
	clk.tick(10*time.Minute + time.Second)
	if _, err := a.RedeemInvite(ctx, inv.Token, idb.PublicKey(), "late", proofFor(t, inv, idb, "late")); !errors.Is(err, ErrInviteExpired) {
		t.Fatalf("err = %v, want ErrInviteExpired", err)
	}
	if rep := mustMembers(t, a); len(rep.Members) != 1 {
		t.Fatalf("%+v", rep)
	}
	// Exactly at the deadline is still too late: an invite lives for its TTL, not a moment more.
	inv2, _ := a.CreateInvite(ctx, RoleMember, "y", 10*time.Minute)
	clk.tick(10 * time.Minute)
	if _, err := a.RedeemInvite(ctx, inv2.Token, idb.PublicKey(), "edge", proofFor(t, inv2, idb, "edge")); !errors.Is(err, ErrInviteExpired) {
		t.Fatalf("at the deadline: err = %v, want ErrInviteExpired", err)
	}
	// One second before it, it works.
	inv3, _ := a.CreateInvite(ctx, RoleMember, "z", 10*time.Minute)
	clk.tick(10*time.Minute - time.Second)
	if _, err := a.RedeemInvite(ctx, inv3.Token, idb.PublicKey(), "in time", proofFor(t, inv3, idb, "in time")); err != nil {
		t.Fatalf("one second before the deadline: %v", err)
	}
}

func TestWrongTokensAreInvalidWhateverTheInvitesState(t *testing.T) {
	ctx := context.Background()
	a, _, clk := admin(t)
	inv, _ := a.CreateInvite(ctx, RoleMember, "x", time.Minute)
	idb := newKey(t)
	// Right ID, wrong secret.
	raw := strings.NewReplacer("-", "").Replace(inv.Token)
	flip := byte('A')
	if raw[len(raw)-1] == 'A' {
		flip = 'B'
	}
	wrong := raw[:len(raw)-1] + string(flip)
	bad := inv
	bad.Token = wrong
	if _, err := a.RedeemInvite(ctx, wrong, idb.PublicKey(), "x", proofFor(t, bad, idb, "x")); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("wrong secret: err = %v, want ErrInviteInvalid", err)
	}
	// Unknown ID, garbage, empty.
	for _, tok := range []string{"AAAAAAAAAAAAAAAAAAAAAAAA", "not a token", ""} {
		if _, err := a.RedeemInvite(ctx, tok, idb.PublicKey(), "x", []byte("p")); !errors.Is(err, ErrInviteInvalid) {
			t.Errorf("%q: err = %v, want ErrInviteInvalid", tok, err)
		}
	}
	// A wrong secret on an EXPIRED invite is still "invalid", not "expired": the answer must not tell a guesser which invites exist.
	clk.tick(time.Hour)
	if _, err := a.RedeemInvite(ctx, wrong, idb.PublicKey(), "x", proofFor(t, bad, idb, "x")); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("wrong secret, expired invite: err = %v", err)
	}
	// Dashes, case and spaces do not matter to the person typing.
	inv2, _ := a.CreateInvite(ctx, RoleMember, "y", time.Hour)
	typed := strings.ToLower(strings.ReplaceAll(inv2.Token, "-", " "))
	if _, err := a.RedeemInvite(ctx, typed, idb.PublicKey(), "y", proofFor(t, inv2, idb, "y")); err != nil {
		t.Fatalf("a token typed in lower case with spaces: %v", err)
	}
}

func TestABadProofDoesNotBurnTheInvite(t *testing.T) {
	ctx := context.Background()
	a, _, _ := admin(t)
	inv, _ := a.CreateInvite(ctx, RoleMember, "x", time.Hour)
	idb, mallory := newKey(t), newKey(t)
	// Mallory saw the token (a shoulder-surfed code) but cannot prove she holds idb's key: she signs for a key that is not hers.
	if _, err := a.RedeemInvite(ctx, inv.Token, idb.PublicKey(), "x", proofFor(t, inv, mallory, "x")); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("err = %v, want ErrBadSignature", err)
	}
	// A proof made for another label (the label is part of what is proved) is refused too.
	if _, err := a.RedeemInvite(ctx, inv.Token, idb.PublicKey(), "Admin Reza", proofFor(t, inv, idb, "x")); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("a proof for another label: err = %v", err)
	}
	// The real device can still use it.
	if _, err := a.RedeemInvite(ctx, inv.Token, idb.PublicKey(), "x", proofFor(t, inv, idb, "x")); err != nil {
		t.Fatalf("the invite was burned by a failed attempt: %v", err)
	}
}

func TestAnAlreadyEnrolledDeviceCannotUseAnotherInvite(t *testing.T) {
	ctx := context.Background()
	a, ida, _ := admin(t)
	inv, _ := a.CreateInvite(ctx, RoleAdmin, "x", time.Hour)
	if _, err := a.RedeemInvite(ctx, inv.Token, ida.PublicKey(), "me again", proofFor(t, inv, ida, "me again")); !errors.Is(err, ErrAlreadyMember) {
		t.Fatalf("err = %v, want ErrAlreadyMember", err)
	}
	// The failed redemption left the invite usable.
	idb := newKey(t)
	if _, err := a.RedeemInvite(ctx, inv.Token, idb.PublicKey(), "b", proofFor(t, inv, idb, "b")); err != nil {
		t.Fatal(err)
	}
}

func TestASecondAdminCanBeEnrolledAndThenEnrollsOthers(t *testing.T) {
	ctx := context.Background()
	a, _, _ := admin(t)
	inv, _ := a.CreateInvite(ctx, RoleAdmin, "second admin", time.Hour)
	idb := newKey(t)
	if _, err := a.RedeemInvite(ctx, inv.Token, idb.PublicKey(), "Dawit's laptop", proofFor(t, inv, idb, "Dawit's laptop")); err != nil {
		t.Fatal(err)
	}
	b, _ := member(t, "dawit", idb)
	pull(t, b, a, 10)
	inv2, err := b.CreateInvite(ctx, RoleMember, "a phone", time.Hour)
	if err != nil {
		t.Fatalf("the second admin must be able to invite: %v", err)
	}
	idc := newKey(t)
	if _, err := b.RedeemInvite(ctx, inv2.Token, idc.PublicKey(), "phone", proofFor(t, inv2, idc, "phone")); err != nil {
		t.Fatal(err)
	}
	pull(t, a, b, 10)
	if ra := mustMembers(t, a); len(ra.Members) != 3 || ra.Admins() != 2 || len(ra.Problems) != 0 {
		t.Fatalf("%+v", ra)
	}
}

func TestTwoDevicesRacingForOneInviteGetExactlyOne(t *testing.T) {
	ctx := context.Background()
	a, _, _ := admin(t)
	inv, _ := a.CreateInvite(ctx, RoleMember, "x", time.Hour)
	const n = 8
	keys := make([]*identity.Identity, n)
	for i := range keys {
		keys[i] = newKey(t)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := a.RedeemInvite(ctx, inv.Token, keys[i].PublicKey(), "d", proofFor(t, inv, keys[i], "d"))
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				won++
			} else if !errors.Is(err, ErrInviteUsed) {
				t.Errorf("a loser got %v, want ErrInviteUsed", err)
			}
		}(i)
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("%d devices redeemed one invite", won)
	}
	if rep := mustMembers(t, a); len(rep.Members) != 2 {
		t.Fatalf("%d members, want the founder and one new device", len(rep.Members))
	}
}

func TestInviteLinkRoundTripsAndIsPinnedToTheAdmin(t *testing.T) {
	a, ida, _ := admin(t)
	inv, _ := a.CreateInvite(context.Background(), RoleMember, "x", time.Hour)
	fp := strings.Repeat("ab", 32)
	link := inv.Link(fp)
	got, err := ParseInviteLink(link)
	if err != nil {
		t.Fatal(err)
	}
	if got.Admin != ida.ID() || got.Fingerprint != fp || NormalizeToken(got.Token) != NormalizeToken(inv.Token) {
		t.Fatalf("%+v", got)
	}
	// The fingerprint is optional until Day 40 gives the node a certificate.
	if g, err := ParseInviteLink(inv.Link("")); err != nil || g.Fingerprint != "" {
		t.Fatalf("%+v %v", g, err)
	}
	for _, bad := range []string{"", "og-invite/2/x/y/z", "og-invite/1/notadevice/ABC/", "og-invite/1/" + ida.ID() + "/!!!/", "og-invite/1/" + ida.ID() + "/" + inv.Token + "/zz", "https://example.com"} {
		if _, err := ParseInviteLink(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
