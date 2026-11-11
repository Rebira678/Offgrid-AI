package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"offgrid/internal/store/sqlstore"
)

// loggedDB makes a database with a few ops from one replica.
func loggedDB(t *testing.T) string {
	t.Helper()
	db := dbPath(t)
	for _, js := range []string{
		`{"intent":"create_task","title":"Leaking pump","site":"4","priority":"high"}`,
		`{"intent":"create_task","title":"Replace filter","priority":"low"}`,
		`{"intent":"update_task","task_ref":"pump","status":"in_progress"}`,
	} {
		if code, out, errOut := runCLI(t, "", "exec", "-db", db, js); code != 0 {
			t.Fatalf("%s: exit %d\n%s%s", js, code, out, errOut)
		}
	}
	return db
}

// edit does what someone with the file would do: no append-only triggers, then direct SQL.
func edit(t *testing.T, db string, queries ...string) {
	t.Helper()
	raw, err := sqlstore.OpenDB(db)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for _, q := range []string{
		`DROP TRIGGER IF EXISTS ops_append_only_update`,
		`DROP TRIGGER IF EXISTS ops_append_only_delete`,
		`DROP TRIGGER IF EXISTS op_chain_append_only_update`,
		`DROP TRIGGER IF EXISTS op_chain_append_only_delete`,
		`DROP TRIGGER IF EXISTS op_sigs_append_only_update`,
		`DROP TRIGGER IF EXISTS op_sigs_append_only_delete`,
		`DROP TRIGGER IF EXISTS genesis_attestations_append_only_update`,
		`DROP TRIGGER IF EXISTS genesis_attestations_append_only_delete`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range queries {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func TestVerifyChecksTheChainAndSaysWhatItCannotSee(t *testing.T) {
	db := loggedDB(t)
	code, out, errOut := runCLI(t, "", "store", "-db", db, "verify")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	for _, want := range []string{"ok: the hash chain of all 3 ops matches (1 replicas)", "ok: replayed 3 ops", "state hash", "cannot see a log rewritten from start to end"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "upgraded") {
		t.Errorf("a database born with the chain has nothing retroactive:\n%s", out)
	}
}

func TestAnEditedOpMakesVerifyFailAndNamesIt(t *testing.T) {
	db := loggedDB(t)
	edit(t, db, `UPDATE ops SET payload = replace(payload, 'Leaking pump', 'Leaking pumpkin') WHERE seq = 1`)
	code, out, errOut := runCLI(t, "", "store", "-db", db, "verify")
	if code != 1 || out != "" {
		t.Fatalf("exit %d stdout %q", code, out)
	}
	for _, want := range []string{"TAMPERED: op ", "/1: was changed after it was recorded", "Nothing has been changed or repaired", "offgrid sync summary"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut)
		}
	}
	// It does not repair the log, and it will not hand out heads of a chain that does not check out.
	if code, _, errOut := runCLI(t, "", "store", "-db", db, "head"); code != 1 || !strings.Contains(errOut, "TAMPERED") {
		t.Errorf("head on a tampered log: exit %d, %q", code, errOut)
	}
	if code, _, _ := runCLI(t, "", "store", "-db", db, "verify"); code != 1 {
		t.Error("the second verify must say the same thing: it repaired nothing")
	}
}

func TestHeadsAreSavedAndCheckedLater(t *testing.T) {
	db := loggedDB(t)
	code, out, errOut := runCLI(t, "", "store", "-db", db, "head")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "# ") || !regexp.MustCompile(`^\S+ 3 [0-9a-f]{64}$`).MatchString(lines[1]) {
		t.Fatalf("head output:\n%s", out)
	}
	anchor := filepath.Join(t.TempDir(), "heads.txt")
	os.WriteFile(anchor, []byte(out), 0o644)

	// Both spellings: flag before the command, and after it.
	for _, args := range [][]string{{"store", "-db", db, "-anchor", anchor, "verify"}, {"store", "-db", db, "verify", "-anchor", anchor}} {
		code, out, errOut := runCLI(t, "", args...)
		if code != 0 || !strings.Contains(out, "ok: all 1 saved heads in "+anchor+" are still in the log, unchanged") || strings.Contains(out, "cannot see a log rewritten") {
			t.Errorf("%v: exit %d\n%s%s", args, code, out, errOut)
		}
	}
	// The log grows: the old head is still in it.
	runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"later","priority":"low"}`)
	if code, out, errOut := runCLI(t, "", "store", "-db", db, "verify", "-anchor", anchor); code != 0 {
		t.Errorf("an old head must survive new work: exit %d\n%s%s", code, out, errOut)
	}
}

// Someone who rewrites an op and recomputes the chain from there fails plain
// verify under Day 36 because the attacker cannot forge the replica's ECDSA
// signature. If an attacker also has the key and re-signs, a saved anchor head
// catches them.
func TestARewrittenChainIsCaughtOnlyByASavedHead(t *testing.T) {
	db := loggedDB(t)
	_, saved, _ := runCLI(t, "", "store", "-db", db, "head")
	anchor := filepath.Join(t.TempDir(), "heads.txt")
	os.WriteFile(anchor, []byte(saved), 0o644)

	edit(t, db, `UPDATE ops SET payload = replace(payload, 'Replace filter', 'Replace nothing') WHERE seq = 2`)
	st, err := sqlstore.Open(context.Background(), db, sqlstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ident := st.Identity()
	ops, _ := st.Ops(context.Background(), 0, 100)
	st.Close()
	raw, _ := sqlstore.OpenDB(db)
	prev := sqlstore.GenesisHash
	newHashes := make(map[int64]string)
	for _, op := range ops {
		h := sqlstore.OpHash(op, prev)
		if _, err := raw.Exec(`UPDATE op_chain SET prev = ?, hash = ? WHERE node_id = ? AND seq = ?`, prev, h, op.NodeID, op.Seq); err != nil {
			t.Fatal(err)
		}
		newHashes[op.Seq] = h
		prev = h
	}
	raw.Close()
	// The careful editor also rebuilds the tasks view, or the old check (view against log) would notice.
	if code, _, errOut := runCLI(t, "", "store", "-db", db, "rebuild"); code != 0 {
		t.Fatal(errOut)
	}

	// 1. Without the replica's private key, the rewritten chain fails plain verify
	// because the stored signature does not match the recomputed chain hash.
	if code, _, errOut := runCLI(t, "", "store", "-db", db, "verify"); code != 1 || !strings.Contains(errOut, "TAMPERED") || !strings.Contains(errOut, "forged or edited signature") {
		t.Fatalf("an unauthenticated rewrite must fail plain verify on bad signature: exit %d\n%s", code, errOut)
	}

	// 2. If the attacker also possesses the device key and re-signs the new hash,
	// plain verify passes (it cannot see a log rewritten from scratch), but
	// the saved head from before the rewrite catches the tampering.
	if ident != nil {
		edit(t, db)
		raw, _ := sqlstore.OpenDB(db)
		for _, op := range ops {
			sig, err := ident.Sign([]byte(newHashes[op.Seq]))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec(`UPDATE op_sigs SET sig = ? WHERE node_id = ? AND seq = ?`, sig, op.NodeID, op.Seq); err != nil {
				t.Fatal(err)
			}
		}
		raw.Close()

		if code, out, errOut := runCLI(t, "", "store", "-db", db, "verify"); code != 0 || !strings.Contains(out, "cannot see a log rewritten") {
			t.Fatalf("a self-consistent re-signed rewrite passes plain verify: exit %d\n%s%s", code, out, errOut)
		}
		code, _, errOut := runCLI(t, "", "store", "-db", db, "verify", "-anchor", anchor)
		if code != 1 || !strings.Contains(errOut, "TAMPERED") || !strings.Contains(errOut, "history before it was rewritten") {
			t.Errorf("with the saved head: exit %d\n%s", code, errOut)
		}
	}
}

func TestBadAnchorFilesAreRefusedClearly(t *testing.T) {
	db := loggedDB(t)
	dir := t.TempDir()
	hash := strings.Repeat("a", 64)
	for name, c := range map[string]struct{ content, want string }{
		"empty":       {"# nothing\n\n", "has no saved heads"},
		"two fields":  {"laptop 3\n", `line 1: want "node seq hash"`},
		"bad number":  {"laptop three " + hash + "\n", `line 1: want "node seq hash"`},
		"zero seq":    {"laptop 0 " + hash + "\n", `line 1: want "node seq hash"`},
		"short hash":  {"laptop 3 abc\n", `line 1: want "node seq hash"`},
		"second line": {"laptop 3 " + hash + "\nnonsense\n", "line 2"},
		"four fields": {"laptop 3 " + hash + " extra\n", `line 1: want "node seq hash"`},
	} {
		f := filepath.Join(dir, name)
		os.WriteFile(f, []byte(c.content), 0o644)
		code, _, errOut := runCLI(t, "", "store", "-db", db, "verify", "-anchor", f)
		if code != 2 || !strings.Contains(errOut, c.want) {
			t.Errorf("%s: exit %d, stderr %q, want exit 2 containing %q", name, code, errOut, c.want)
		}
	}
	if code, _, errOut := runCLI(t, "", "store", "-db", db, "verify", "-anchor", filepath.Join(dir, "missing")); code != 2 || !strings.Contains(errOut, "cannot read the anchor file") {
		t.Errorf("missing file: exit %d %q", code, errOut)
	}
	if code, _, _ := runCLI(t, "", "store", "-db", db, "verify", "extra"); code != 2 {
		t.Errorf("a stray argument: exit %d, want 2", code)
	}
}

func TestADatabaseFromBeforeTheChainIsUpgradedAndVerifyAdmitsWhatThatMeans(t *testing.T) {
	db := loggedDB(t)
	raw, _ := sqlstore.OpenDB(db)
	for _, q := range []string{
		`DROP TABLE IF EXISTS op_chain`,
		`DROP TABLE IF EXISTS op_sigs`,
		`DROP TABLE IF EXISTS genesis_attestations`,
		`DELETE FROM schema_migrations WHERE version >= 4`,
		`DELETE FROM meta WHERE key IN ('chain_retro_ops', 'genesis_attested_ops')`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()
	code, out, errOut := runCLI(t, "", "store", "-db", db, "verify")
	if code != 0 || !strings.Contains(out, "3 of them were chained when this database was upgraded") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if _, out, _ := runCLI(t, "", "store", "-db", db, "info"); !strings.Contains(out, "schema version     5") {
		t.Errorf("info:\n%s", out)
	}
}

func TestStoreUsageMentionsHeadAndAnchor(t *testing.T) {
	_, _, errOut := runCLI(t, "", "store")
	if !strings.Contains(errOut, "head") || !strings.Contains(errOut, "-anchor FILE") {
		t.Errorf("usage: %s", errOut)
	}
}
