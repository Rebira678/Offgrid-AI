package main

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"offgrid/internal/store/sqlstore"
	"offgrid/internal/syncer"
)

func runTask(t *testing.T, db string, args ...string) (int, string, string) {
	t.Helper()
	var o, e strings.Builder
	code := run(append([]string{"task", "-db", db}, args...), &o, &e)
	return code, o.String(), e.String()
}

func seededDB(t *testing.T) string {
	t.Helper()
	db := filepath.Join(t.TempDir(), "t.db")
	for _, js := range []string{
		`{"intent":"create_task","title":"Leaking pump","site":"4","priority":"high"}`,
		`{"intent":"create_task","title":"Replace filter","priority":"low"}`,
		`{"intent":"add_note","task_ref":"T1","note":"seal cracked"}`,
		`{"intent":"add_note","task_ref":"T1","note":"parts ordered"}`,
	} {
		if code, out, _ := runExec(t, "", "-db", db, js); code != 0 {
			t.Fatalf("seeding %s: %s", js, out)
		}
	}
	return db
}

func TestTaskListDeleteAndRemoveNote(t *testing.T) {
	db := seededDB(t)
	code, out, _ := runTask(t, db, "list")
	if code != 0 || !strings.Contains(out, "Leaking pump") || !strings.Contains(out, "1. seal cracked") || !strings.Contains(out, "2. parts ordered") || !strings.Contains(out, "state hash: ") {
		t.Fatalf("list:\n%s", out)
	}
	if code, out, e := runTask(t, db, "remove-note", "pump", "1"); code != 0 || !strings.Contains(out, "removed note 1 of T1") {
		t.Fatalf("remove-note: %d %s %s", code, out, e)
	}
	_, out, _ = runTask(t, db, "list")
	if strings.Contains(out, "seal cracked") || !strings.Contains(out, "1. parts ordered") {
		t.Fatalf("after remove-note:\n%s", out)
	}
	if code, out, _ := runTask(t, db, "delete", "T2"); code != 0 || !strings.Contains(out, `deleted T2 "Replace filter"`) {
		t.Fatalf("delete: %d %s", code, out)
	}
	_, out, _ = runTask(t, db, "list")
	if strings.Contains(out, "Replace filter") {
		t.Fatalf("a deleted task is still listed:\n%s", out)
	}
	// problems are said plainly and change nothing
	for name, args := range map[string][]string{
		"unknown task":    {"delete", "T99"},
		"already deleted": {"delete", "T2"},
		"note too high":   {"remove-note", "T1", "5"},
		"note not number": {"remove-note", "T1", "x"},
		"no ref":          {"delete"},
		"unknown command": {"frobnicate"},
	} {
		if code, _, e := runTask(t, db, args...); code == 0 || e == "" {
			t.Errorf("%s: code %d, stderr %q; want a failure with a message", name, code, e)
		}
	}
	_, after, _ := runTask(t, db, "list")
	if after != out {
		t.Errorf("a refused command changed the list:\n%s\nvs\n%s", out, after)
	}
}

func TestTheModelCannotAskForADelete(t *testing.T) {
	// Deleting is a command-line operation only. A message that asks for it
	// must not reach the store through the tools.
	db := seededDB(t)
	for _, js := range []string{
		`{"intent":"delete_task","task_ref":"T1"}`,
		`{"intent":"delete","task_ref":"T1"}`,
		`{"intent":"remove_task","task_ref":"T1"}`,
	} {
		code, out, _ := runExec(t, "", "-db", db, js)
		if code != 0 || !strings.Contains(out, "0 accepted, 1 refused") || !strings.Contains(out, "is not allowed") {
			t.Errorf("%s: code %d, output %q; want it refused as a disallowed intent", js, code, out)
		}
	}
	_, out, _ := runTask(t, db, "list")
	if !strings.Contains(out, "Leaking pump") || !strings.Contains(out, "Replace filter") {
		t.Fatalf("a task vanished:\n%s", out)
	}
}

func TestSyncRunReportsOfflineThenSyncsWhenTheSwitchIsFlipped(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	os.WriteFile(keyFile, []byte("run-test-key-0123456789abcdef\n"), 0o600)
	t.Setenv("OFFGRID_SYNC_KEY", "")

	srvStore, err := sqlstore.Open(ctx, filepath.Join(dir, "a.db"), sqlstore.Options{NodeID: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	defer srvStore.Close()
	key, _ := syncer.LoadKey(keyFile)
	srv := httptest.NewServer(syncer.Handler(srvStore, syncer.ServerOptions{Key: key}))
	defer srv.Close()
	if code, out, _ := runExec(t, "", "-db", filepath.Join(dir, "a.db"), `{"intent":"create_task","title":"Leaking pump","priority":"high"}`); code != 0 {
		t.Fatal(out)
	}

	faultFile := filepath.Join(dir, "fault")
	os.WriteFile(faultFile, []byte("partition\n"), 0o600)
	var out lockedBuf
	done := make(chan int, 1)
	go func() {
		done <- run([]string{"sync", "run", "-db", filepath.Join(dir, "b.db"), "-peer", srv.URL, "-key-file", keyFile,
			"-every", "100ms", "-for", "6s", "-name", "B", "-fault-file", faultFile}, &out, &out)
	}()
	waitFor(t, &out, "OFFLINE", 4*time.Second)
	if strings.Contains(out.String(), "SYNCED") {
		t.Fatal("synced while the network was off")
	}
	os.WriteFile(faultFile, []byte("ok\n"), 0o600)
	waitFor(t, &out, "SYNCED: pulled 1, pushed 0", 4*time.Second)
	waitFor(t, &out, "in step", 4*time.Second)
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, out.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("sync run did not stop at -for")
	}
	if !strings.Contains(out.String(), "FAULT INJECTION ON (simulated") {
		t.Errorf("the output does not say the network fault is simulated:\n%s", out.String())
	}
}
