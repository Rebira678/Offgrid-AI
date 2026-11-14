package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"offgrid/internal/store/sqlstore"
	"offgrid/internal/syncer"
)

// Day 38: enrollment commands.
//
//	offgrid store -db F found [-label L]           the first device makes itself the first admin
//	offgrid store -db F invite [-role member|admin] [-label L] [-ttl 15m]
//	offgrid store -db F members                    who is enrolled
//	offgrid sync enroll -peer URL -invite LINK -label L     (on the NEW device)

// groupToken writes a token in groups of four so a person can read it out.
func groupToken(t string) string {
	var parts []string
	for len(t) > 4 {
		parts = append(parts, t[:4])
		t = t[4:]
	}
	return strings.Join(append(parts, t), "-")
}

func needDeviceKey(w io.Writer) int {
	fmt.Fprintln(w, "error: this command signs with this device's key, and there is none. Make one with: offgrid identity init")
	return 1
}

// storeMemberCmd runs found, invite and members. st was opened with the device key.
func storeMemberCmd(ctx context.Context, sub string, st *sqlstore.Store, role, label string, ttl time.Duration, device string, stdout, stderr io.Writer) int {
	switch sub {
	case "found":
		if label == "" {
			label = "founder"
		}
		if err := st.Found(ctx, label); err != nil {
			return memberError(stderr, err)
		}
		rep, _ := st.Members(ctx)
		fmt.Fprintf(stdout, "ok: %s founded the membership as admin %q.\n", rep.Founder(), label)
		fmt.Fprintln(stdout, "Every other device must be added by an admin. Check this device ID is the one you expect: it is the root of trust, and devices that enroll pin it.")
		fmt.Fprintln(stdout, "Before a pilot, add a second admin:  offgrid store -db FILE invite -role admin -label NAME")
	case "invite":
		r := sqlstore.Role(role)
		inv, err := st.CreateInvite(ctx, r, label, ttl)
		if err != nil {
			return memberError(stderr, err)
		}
		fmt.Fprintf(stdout, "invite for a %s %q, valid until %s, single use\n", inv.Role, inv.Label, inv.Expires.Local().Format("15:04:05 Mon 2 Jan"))
		fmt.Fprintf(stdout, "code   %s\n", groupToken(inv.Token))
		fmt.Fprintf(stdout, "admin  %s\n", inv.Admin)
		fmt.Fprintf(stdout, "link   %s\n", inv.Link(""))
		fmt.Fprintln(stdout, "Give it to the new device by a channel only you can see (in person, a QR code: `qrencode -t ANSI '<link>'`).")
		fmt.Fprintln(stdout, "Whoever holds it before it expires can enroll as that device. The new device must redeem it at THIS node (it is not synced).")
		fmt.Fprintln(stdout, "The link has no certificate fingerprint yet; that arrives with mTLS on Day 40.")
	case "revoke":
		rev, err := st.Revoke(ctx, device)
		if err != nil {
			return memberError(stderr, err)
		}
		fmt.Fprintf(stdout, "ok: %s revoked.\n", rev.Device)
		if len(rev.Cutoffs) == 0 {
			fmt.Fprintln(stdout, "This node held none of its work, so none of it counts.")
		}
		for _, c := range rev.Cutoffs {
			fmt.Fprintf(stdout, "  %s: work up to op %d counts; later work is void\n", c.Node, c.Seq)
		}
		fmt.Fprintln(stdout, "Work the device did after that and has not synced here is lost to the group; re-enter it by hand if it matters.")
		fmt.Fprintln(stdout, "Other nodes learn of it when they next sync. Its key never comes back; enroll the device again with a NEW key (offgrid identity init).")
	case "members":
		rep, err := st.Members(ctx)
		if err != nil {
			return memberError(stderr, err)
		}
		printMembership(stdout, rep)
	}
	return 0
}

func memberError(w io.Writer, err error) int {
	switch {
	case errors.Is(err, sqlstore.ErrNotAuthorized):
		fmt.Fprintf(w, "error: %v\nOnly an admin can do this. If nothing is founded yet, run: offgrid store -db FILE found\n", err)
	case errors.Is(err, sqlstore.ErrAlreadyMember):
		fmt.Fprintf(w, "error: %v\n", err)
	default:
		fmt.Fprintf(w, "error: %v\n", err)
	}
	return 1
}

// printMembership lists members and warns about a single admin.
func printMembership(w io.Writer, rep sqlstore.MembershipReport) {
	if len(rep.Members) == 0 {
		fmt.Fprintln(w, "no membership yet. The first device runs: offgrid store -db FILE found")
		return
	}
	n := rep.Admins()
	fmt.Fprintf(w, "%d members, %d admin", len(rep.Members), n)
	if n != 1 {
		fmt.Fprint(w, "s")
	}
	fmt.Fprintln(w)
	for i, m := range rep.Members {
		tag := ""
		if i == 0 {
			tag = " (founder)"
		}
		if m.Revoked {
			tag += " REVOKED"
		}
		fmt.Fprintf(w, "  %-6s %s  %q%s\n", m.Role, m.Device, m.Label, tag)
	}
	for _, p := range rep.Problems {
		fmt.Fprintf(w, "  PROBLEM: op %s/%d: %s\n", p.Node, p.Seq, p.Reason)
	}
	if n < 2 {
		fmt.Fprintln(w, "WARNING: only 1 admin. If that device is lost nobody can enroll or remove anyone. Add a second admin before the pilot:\n         offgrid store -db FILE invite -role admin -label NAME")
	}
}

func cmdSyncEnroll(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sync enroll", flag.ContinueOnError)
	fs.SetOutput(stderr)
	peer := fs.String("peer", "", "base URL of the admin's node, e.g. http://192.168.1.5:7878")
	invite := fs.String("invite", "", "the invite link (og-invite/1/...)")
	code := fs.String("code", "", "instead of -invite: the typed code, with -admin")
	admin := fs.String("admin", "", "with -code: the admin's device ID (dev_...)")
	label := fs.String("label", "", "a name for this device, recorded in the membership (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *peer == "" || *label == "" {
		fmt.Fprintln(stderr, "usage: offgrid sync enroll -peer URL -invite LINK -label NAME")
		fmt.Fprintln(stderr, "error: -peer and -label are required")
		return 2
	}
	if *invite == "" && (*admin == "" || *code == "") {
		fmt.Fprintln(stderr, "error: -invite LINK (or -code CODE and -admin DEV_ID) is required")
		return 2
	}
	var link sqlstore.InviteLink
	var err error
	if *invite != "" {
		link, err = sqlstore.ParseInviteLink(*invite)
	} else {
		link, err = sqlstore.ParseInviteLink("og-invite/1/" + *admin + "/" + sqlstore.NormalizeToken(*code) + "/")
	}
	if err != nil {
		fmt.Fprintf(stderr, "error: the invite is not usable: %v\n", err)
		return 2
	}
	sg, err := deviceSigner()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if sg == nil {
		fmt.Fprintln(stderr, "error: this device has no key to enroll. Make one with: offgrid identity init")
		return 1
	}
	proof, err := sqlstore.EnrollProof(link.Token, sg, *label)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := syncer.Enroll(ctx, &http.Client{Timeout: 30 * time.Second}, *peer, syncer.EnrollRequest{
		Token: link.Token, PublicKey: sg.PublicKey(), Label: *label, Proof: proof,
	})
	if err != nil {
		var re *syncer.RemoteError
		switch {
		case errors.As(err, &re) && re.Code == syncer.CodeInviteInvalid:
			fmt.Fprintln(stderr, "error: the invite is not valid (wrong, already used, or expired). Ask the admin for a new one.")
		case errors.As(err, &re) && re.Code == syncer.CodeTooManyAttempts:
			fmt.Fprintln(stderr, "error: that node has had too many failed attempts. Wait a minute and try again.")
		case errors.As(err, &re) && re.Code == syncer.CodeAlreadyMember:
			fmt.Fprintln(stderr, "error: this device is already a member. Run sync once to receive the membership.")
		default:
			fmt.Fprintln(stderr, "error:", err)
		}
		return 1
	}
	if err := syncer.CheckEnrollment(resp, link, sg, *label); err != nil {
		fmt.Fprintf(stderr, "error: %v\nThe node answered, but not as the admin the invite named. Do not use it.\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "enrolled: %s is now %s as %q\n", resp.Device, article(resp.Role), resp.Label)
	fmt.Fprintf(stdout, "admin:    %s (the device the invite named; its signed receipt checks out)\n", resp.AdminDevice)
	fmt.Fprintf(stdout, "Now receive the membership:  offgrid sync once -db FILE -create -peer %s\n", *peer)
	return 0
}

func article(role string) string {
	if role == string(sqlstore.RoleAdmin) {
		return "an admin"
	}
	return "a member"
}

// printMembershipSummary is the short form `store verify` prints.
func printMembershipSummary(w io.Writer, rep sqlstore.MembershipReport) {
	if len(rep.Members) == 0 {
		return
	}
	fmt.Fprintf(w, "ok: %d members, %d admin(s); founded by %s\n", len(rep.Members), rep.Admins(), rep.Founder())
	if rep.Admins() < 2 {
		fmt.Fprintln(w, "    WARNING: only 1 admin. Add a second before the pilot (offgrid store invite -role admin).")
	}
}

// printVoid lists ops that are in the log but do not count.
func printVoid(w io.Writer, rep sqlstore.VerifyReport) {
	if len(rep.Void) == 0 {
		return
	}
	fmt.Fprintf(w, "void: %d op(s) are in the log as evidence but do not count\n", len(rep.Void))
	for i, v := range rep.Void {
		if i == 20 {
			fmt.Fprintf(w, "  ... and %d more\n", len(rep.Void)-20)
			break
		}
		fmt.Fprintf(w, "  %s/%d  %s\n", v.Node, v.Seq, v.Reason)
	}
	if rep.VoidLost > 0 {
		fmt.Fprintf(w, "note: %d op(s) once recorded void are not void now (the membership changed under them)\n", rep.VoidLost)
	}
}
