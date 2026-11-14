package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"offgrid/internal/identity"
	"offgrid/internal/store/sqlstore"
	"offgrid/internal/syncer"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// useKey points the CLI at a fresh device key for the commands that follow.
func useKey(t *testing.T, name string) string {
	t.Helper()
	key := filepath.Join(t.TempDir(), name+".key")
	if code, out, errOut := runCLI(t, "", "identity", "init", "-key", key); code != 0 {
		t.Fatalf("identity init: exit %d\n%s%s", code, out, errOut)
	}
	t.Setenv("OFFGRID_IDENTITY_KEY", key)
	return key
}

type safeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *safeBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// serveAdmin starts `sync serve` for db under the CURRENT device key and returns its URL.
func serveAdmin(t *testing.T, db string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	out, errb := &safeBuf{}, &safeBuf{}
	done := make(chan int, 1)
	go func() {
		done <- cmdSyncServeContext(ctx, []string{"-db", db, "-addr", "127.0.0.1:0"}, out, errb)
	}()
	t.Cleanup(func() { cancel(); <-done })
	re := regexp.MustCompile(`serving on (http://\S+)`)
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if m := re.FindStringSubmatch(out.String()); m != nil {
			return m[1]
		}
	}
	t.Fatalf("serve did not start: %s", errb.String())
	return ""
}

var linkRE = regexp.MustCompile(`og-invite/1/\S+`)

func TestEnrollmentEndToEnd(t *testing.T) {
	t.Setenv("OFFGRID_SYNC_KEY", strings.Repeat("k", 32))
	t.Setenv("OFFGRID_NODE", "laptop")
	adminKey := useKey(t, "admin")
	db := dbPath(t)
	if code, _, e := runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"seed"}`); code != 0 {
		t.Fatalf("seed: %s", e)
	}

	// Before founding, nothing can be invited.
	if code, _, e := runCLI(t, "", "store", "-db", db, "invite", "-label", "Pi"); code == 0 || !strings.Contains(e, "found") {
		t.Fatalf("invite before founding: exit %d %q", code, e)
	}
	if code, out, e := runCLI(t, "", "store", "-db", db, "found", "-label", "laptop"); code != 0 || !strings.Contains(out, "founded") {
		t.Fatalf("found: exit %d\n%s%s", code, out, e)
	}
	if code, _, e := runCLI(t, "", "store", "-db", db, "found", "-label", "again"); code == 0 {
		t.Fatalf("a second founding must fail: %s", e)
	}

	code, out, e := runCLI(t, "", "store", "-db", db, "invite", "-label", "Pi", "-ttl", "15m")
	link := linkRE.FindString(out)
	if code != 0 || link == "" || !strings.Contains(out, "single use") {
		t.Fatalf("invite: exit %d\n%s%s", code, out, e)
	}

	url := serveAdmin(t, db)

	// The new device, under its own key, enrolls.
	piKey := useKey(t, "pi")
	code, out, e = runCLI(t, "", "sync", "enroll", "-peer", url, "-invite", link, "-label", "Pi")
	if code != 0 || !strings.Contains(out, "enrolled") || !strings.Contains(out, "sync once") {
		t.Fatalf("enroll: exit %d\n%s%s", code, out, e)
	}
	// Once only.
	code, _, e = runCLI(t, "", "sync", "enroll", "-peer", url, "-invite", link, "-label", "Pi")
	if code == 0 || !strings.Contains(e, "not valid") {
		t.Fatalf("reusing the invite: exit %d %q", code, e)
	}

	// It then syncs and sees the membership; the admin lists two members, one admin.
	t.Setenv("OFFGRID_NODE", "pi")
	dbB := dbPath(t)
	if code, _, e := runCLI(t, "", "sync", "once", "-db", dbB, "-create", "-peer", url); code != 0 {
		t.Fatalf("sync once: %s", e)
	}
	for _, c := range []struct{ d, node, key string }{{db, "laptop", adminKey}, {dbB, "pi", piKey}} {
		d := c.d
		t.Setenv("OFFGRID_NODE", c.node)
		t.Setenv("OFFGRID_IDENTITY_KEY", c.key)
		code, out, e := runCLI(t, "", "store", "-db", d, "members")
		if code != 0 || !strings.Contains(out, "laptop") || !strings.Contains(out, "Pi") ||
			!strings.Contains(out, "1 admin") || !strings.Contains(out, "WARNING") || !strings.Contains(out, "second admin") {
			t.Fatalf("members of %s: exit %d\n%s%s", d, code, out, e)
		}
	}
	code, out, e = runCLI(t, "", "store", "-db", dbB, "verify")
	if code != 0 || !strings.Contains(out, "2 members, 1 admin(s)") || !strings.Contains(out, "WARNING: only 1 admin") {
		t.Fatalf("verify: exit %d\n%s%s", code, out, e)
	}

	// A second admin: invited as admin, enrolled from the same node, and the warning goes away.
	t.Setenv("OFFGRID_IDENTITY_KEY", adminKey)
	t.Setenv("OFFGRID_NODE", "laptop")
	code, out, e = runCLI(t, "", "store", "-db", db, "invite", "-role", "admin", "-label", "Dell")
	link2 := linkRE.FindString(out)
	if code != 0 || link2 == "" {
		t.Fatalf("admin invite: %d %s%s", code, out, e)
	}
	useKey(t, "dell")
	if code, out, e := runCLI(t, "", "sync", "enroll", "-peer", url, "-invite", link2, "-label", "Dell"); code != 0 || !strings.Contains(out, "an admin") {
		t.Fatalf("second admin: exit %d\n%s%s", code, out, e)
	}
	t.Setenv("OFFGRID_IDENTITY_KEY", adminKey)
	_, out, _ = runCLI(t, "", "store", "-db", db, "members")
	if !strings.Contains(out, "2 admins") || strings.Contains(out, "WARNING") {
		t.Fatalf("with two admins:\n%s", out)
	}
}

func TestEnrollNeedsALabelAnOwnKeyAndAWellFormedInvite(t *testing.T) {
	t.Setenv("OFFGRID_IDENTITY_KEY", filepath.Join(t.TempDir(), "nokey"))
	link := "og-invite/1/dev_00000000000000000000000000000000/AAAAAAAAAAAAAAAAAAAAAAAA/"
	if code, _, e := runCLI(t, "", "sync", "enroll", "-peer", "http://127.0.0.1:1", "-invite", link, "-label", "x"); code == 0 || !strings.Contains(e, "identity init") {
		t.Fatalf("no key: exit %d %q", code, e)
	}
	useKey(t, "me")
	if code, _, e := runCLI(t, "", "sync", "enroll", "-peer", "http://127.0.0.1:1", "-invite", link); code != 2 || !strings.Contains(e, "-label") {
		t.Fatalf("no label: exit %d %q", code, e)
	}
	if code, _, e := runCLI(t, "", "sync", "enroll", "-peer", "http://127.0.0.1:1", "-invite", "garbage", "-label", "x"); code != 2 || !strings.Contains(e, "invite") {
		t.Fatalf("garbage invite: exit %d %q", code, e)
	}
}

func TestVerifyFailsWhenAStoredMembershipDoesNotHold(t *testing.T) {
	// An unsigned database has no membership: verify says nothing about it, and still passes.
	db := loggedDB(t)
	code, out, _ := runCLI(t, "", "store", "-db", db, "verify")
	if code != 0 || strings.Contains(out, "member") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if code, out, _ := runCLI(t, "", "store", "-db", db, "members"); code != 0 || !strings.Contains(out, "no membership") {
		t.Fatalf("members of a database with none: exit %d\n%s", code, out)
	}
}

// A node that is not the admin the invite named answers with a perfectly signed
// receipt of its own. The device must refuse it and say so.
func TestEnrollRefusesAnAnswerFromAnotherAdmin(t *testing.T) {
	useKey(t, "me")
	adminID, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	mallory, _ := identity.Generate()
	sg, _ := deviceSigner()
	tok := "AAAAAAAAAAAAAAAAAAAAAAAA"
	invID, _ := sqlstore.InviteID(tok)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc, _ := mallory.Sign(sqlstore.ReceiptMessage(invID, sg.ID(), sqlstore.RoleAdmin, "me"))
		json.NewEncoder(w).Encode(syncer.EnrollResponse{
			InviteID: invID, Device: sg.ID(), Role: "admin", Label: "me",
			AdminDevice: mallory.ID(), AdminKey: mallory.PublicKey(), Receipt: rc,
		})
	}))
	defer srv.Close()
	link := "og-invite/1/" + adminID.ID() + "/" + tok + "/"
	code, out, e := runCLI(t, "", "sync", "enroll", "-peer", srv.URL, "-invite", link, "-label", "me")
	if code == 0 || strings.Contains(out, "enrolled") || !strings.Contains(e, "not as the admin the invite named") {
		t.Fatalf("exit %d\n%s%s", code, out, e)
	}
}

func TestRevokingADeviceVoidsItsLaterWorkAndVerifyListsIt(t *testing.T) {
	t.Setenv("OFFGRID_SYNC_KEY", strings.Repeat("k", 32))
	t.Setenv("OFFGRID_NODE", "laptop")
	adminKey := useKey(t, "admin")
	db := dbPath(t)
	runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"seed"}`)
	runCLI(t, "", "store", "-db", db, "found", "-label", "laptop")
	_, out, _ := runCLI(t, "", "store", "-db", db, "invite", "-label", "Pi")
	link := linkRE.FindString(out)
	url := serveAdmin(t, db)
	piKey := useKey(t, "pi")
	if code, _, e := runCLI(t, "", "sync", "enroll", "-peer", url, "-invite", link, "-label", "Pi"); code != 0 {
		t.Fatal(e)
	}
	t.Setenv("OFFGRID_NODE", "pi")
	dbB := dbPath(t)
	if code, _, e := runCLI(t, "", "sync", "once", "-db", dbB, "-create", "-peer", url); code != 0 {
		t.Fatal(e)
	}
	runCLI(t, "", "exec", "-db", dbB, `{"intent":"create_task","title":"pi-before"}`)
	if code, _, e := runCLI(t, "", "sync", "once", "-db", dbB, "-peer", url); code != 0 {
		t.Fatal(e)
	}
	piID := strings.Fields(func() string { _, o, _ := runCLI(t, "", "identity", "show"); return o }())
	var dev string
	for _, f := range piID {
		if strings.HasPrefix(f, "dev_") {
			dev = f
		}
	}
	if dev == "" {
		t.Fatalf("no device id in %q", piID)
	}

	// The admin revokes the Pi.
	t.Setenv("OFFGRID_IDENTITY_KEY", adminKey)
	t.Setenv("OFFGRID_NODE", "laptop")
	code, out, e := runCLI(t, "", "store", "-db", db, "revoke", "-device", dev)
	if code != 0 || !strings.Contains(out, "revoked") || !strings.Contains(out, "pi:") {
		t.Fatalf("revoke: exit %d\n%s%s", code, out, e)
	}
	if code, _, e := runCLI(t, "", "store", "-db", db, "revoke", "-device", dev); code == 0 || !strings.Contains(e, "already revoked") {
		t.Fatalf("twice: %d %q", code, e)
	}
	if code, _, e := runCLI(t, "", "store", "-db", db, "revoke"); code != 2 || !strings.Contains(e, "-device") {
		t.Fatalf("no device: %d %q", code, e)
	}

	// The Pi, not having heard, keeps writing and syncs.
	t.Setenv("OFFGRID_IDENTITY_KEY", piKey)
	t.Setenv("OFFGRID_NODE", "pi")
	runCLI(t, "", "exec", "-db", dbB, `{"intent":"create_task","title":"pi-after"}`)
	runCLI(t, "", "sync", "once", "-db", dbB, "-peer", url)

	t.Setenv("OFFGRID_IDENTITY_KEY", adminKey)
	t.Setenv("OFFGRID_NODE", "laptop")
	_, out, _ = runCLI(t, "", "store", "-db", db, "members")
	if !strings.Contains(out, "REVOKED") {
		t.Fatalf("members must show the revoked device:\n%s", out)
	}
	code, out, e = runCLI(t, "", "store", "-db", db, "verify")
	if code != 0 || !strings.Contains(out, "void") || !strings.Contains(out, "pi/") {
		t.Fatalf("verify must list the void ops: exit %d\n%s%s", code, out, e)
	}
	_, out, _ = runCLI(t, "", "exec", "-db", db, `{"intent":"list_tasks"}`)
	if strings.Contains(out, "pi-after") || !strings.Contains(out, "pi-before") {
		t.Fatalf("the view must keep the work before the cutoff and drop the work after:\n%s", out)
	}
}
