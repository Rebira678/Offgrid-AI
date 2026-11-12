package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// signingKey makes a device key and points the CLI at it for this test.
func signingKey(t *testing.T) string {
	t.Helper()
	key := filepath.Join(t.TempDir(), "device.key")
	if code, out, errOut := runCLI(t, "", "identity", "init", "-key", key); code != 0 {
		t.Fatalf("identity init: exit %d\n%s%s", code, out, errOut)
	}
	t.Setenv("OFFGRID_IDENTITY_KEY", key)
	return key
}

func TestCommandsSignWhenThereIsAKeyAndVerifyReportsIt(t *testing.T) {
	signingKey(t)
	db := loggedDB(t) // three exec runs, each writes under the key
	code, out, errOut := runCLI(t, "", "store", "-db", db, "verify")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if !strings.Contains(out, "ok: 3 of 3 ops are signed, by 1 device(s)") || strings.Contains(out, "WARNING") {
		t.Errorf("verify output:\n%s", out)
	}
}

func TestOldUnsignedHistoryIsReportedThenAttested(t *testing.T) {
	db := loggedDB(t) // no key: unsigned, like a Day 30 database
	_, out, _ := runCLI(t, "", "store", "-db", db, "verify")
	if !strings.Contains(out, "ok: 0 of 3 ops are signed") || !strings.Contains(out, "WARNING: 3 ops are unsigned and nothing vouches for them") {
		t.Fatalf("before attesting:\n%s", out)
	}
	if code, _, errOut := runCLI(t, "", "store", "-db", db, "attest"); code == 0 || !strings.Contains(errOut, "device key") {
		t.Fatalf("attest without a key: exit %d, %q", code, errOut)
	}
	signingKey(t)
	if code, out, errOut := runCLI(t, "", "store", "-db", db, "attest"); code != 0 || !strings.Contains(out, "signed the chain head of 1 replicas") {
		t.Fatalf("attest: exit %d\n%s%s", code, out, errOut)
	}
	_, out, _ = runCLI(t, "", "store", "-db", db, "verify")
	for _, want := range []string{"ok: 1 of 4 ops are signed", "3 older ops are unsigned and trusted as of the migration, not proven", "attested by dev_"} {
		if !strings.Contains(out, want) {
			t.Errorf("after attesting, output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "WARNING") {
		t.Errorf("nothing should be left unvouched:\n%s", out)
	}
}

func TestLosingTheKeyDoesNotQuietlyTurnSigningOff(t *testing.T) {
	key := signingKey(t)
	db := loggedDB(t)
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"after the key was lost"}`)
	// exec reports a refused intent in its output, as it always has.
	if !strings.Contains(out+errOut, "no device key is loaded") || !strings.Contains(out, "0 accepted, 1 refused") {
		t.Fatalf("exit %d\n%s%s\na replica that signed must not write unsigned ops", code, out, errOut)
	}
}

func TestADamagedKeyStopsTheCommand(t *testing.T) {
	key := signingKey(t)
	if err := os.Chmod(key, 0o644); err != nil {
		t.Fatal(err)
	}
	db := dbPath(t)
	code, _, errOut := runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"x"}`)
	if code == 0 || !strings.Contains(errOut, "Refusing to run") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
}

func TestSyncWarnsWithoutAKeyAndRequireSignedNeedsOne(t *testing.T) {
	t.Setenv("OFFGRID_SYNC_KEY", strings.Repeat("k", 32))
	db := loggedDB(t)
	code, _, errOut := runCLI(t, "", "sync", "once", "-db", db, "-peer", "http://127.0.0.1:1", "-require-signed")
	if code == 0 || !strings.Contains(errOut, "-require-signed needs a device key") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	_, _, errOut = runCLI(t, "", "sync", "once", "-db", db, "-peer", "http://127.0.0.1:1")
	if !strings.Contains(errOut, "shared sync key is deprecated") {
		t.Fatalf("sync once must warn that the shared key is deprecated: %q", errOut)
	}
	if !strings.Contains(errOut, "NOT signed") {
		t.Fatalf("a sync without a key must say its ops are unsigned: %q", errOut)
	}
}
