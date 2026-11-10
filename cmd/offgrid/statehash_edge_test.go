package main

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"offgrid/internal/store/sqlstore"
	"offgrid/internal/syncer"
)

func setupBrokenViewDB(t *testing.T) string {
	t.Helper()
	db := filepath.Join(t.TempDir(), "broken.db")
	if code, _, e := runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"a"}`); code != 0 {
		t.Fatalf("exec: %d %s", code, e)
	}
	raw, err := sql.Open("sqlite3", db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`ALTER TABLE tasks RENAME COLUMN title TO title_x`); err != nil {
		raw.Close()
		t.Fatalf("break view: %v", err)
	}
	raw.Close()
	return db
}

func setupSyncKeyFile(t *testing.T) ([]byte, string) {
	t.Helper()
	key := []byte("0123456789abcdef-edge-key")
	keyFile := filepath.Join(t.TempDir(), "sync.key")
	if err := os.WriteFile(keyFile, key, 0o600); err != nil {
		t.Fatal(err)
	}
	return key, keyFile
}

func TestSyncRunReportsUnavailableWhenStateHashFails(t *testing.T) {
	t.Setenv("OFFGRID_DB", "")
	t.Setenv("OFFGRID_NODE", "")

	key, keyFile := setupSyncKeyFile(t)
	ctx := context.Background()
	peer, err := sqlstore.Open(ctx, filepath.Join(t.TempDir(), "peer.db"), sqlstore.Options{NodeID: "peer"})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	srv := httptest.NewServer(syncer.Handler(peer, syncer.ServerOptions{Key: key}))
	defer srv.Close()

	db := setupBrokenViewDB(t)

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr lockedBuf
	done := make(chan int, 1)
	go func() {
		done <- cmdSyncRunContext(runCtx, []string{
			"-db", db,
			"-peer", srv.URL,
			"-key-file", keyFile,
			"-every", "20ms",
			"-round-timeout", "2s",
		}, &stdout, &stderr)
	}()

	waitFor(t, &stdout, "UNAVAILABLE (", 3*time.Second)
	cancel()

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("sync run exited %d; stderr:\n%s", code, stderr.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for sync run shutdown")
	}

	out := stdout.String()
	if !strings.Contains(out, "UNAVAILABLE (") {
		t.Errorf("expected UNAVAILABLE status when StateHash fails:\n%s", out)
	}
	if strings.Contains(out, "state \n") || strings.HasSuffix(strings.TrimSpace(out), "state") {
		t.Errorf("printed blank state hash:\n%s", out)
	}
	if !strings.Contains(out, "stopped") {
		t.Errorf("expected clean stopped line:\n%s", out)
	}
}

func TestSyncRunPartitionedReportsUnavailableWhenStateHashFails(t *testing.T) {
	t.Setenv("OFFGRID_DB", "")
	t.Setenv("OFFGRID_NODE", "")

	_, keyFile := setupSyncKeyFile(t)
	db := setupBrokenViewDB(t)

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr lockedBuf
	done := make(chan int, 1)
	go func() {
		done <- cmdSyncRunContext(runCtx, []string{
			"-db", db,
			"-peer", "http://127.0.0.1:1",
			"-key-file", keyFile,
			"-every", "20ms",
			"-round-timeout", "500ms",
			"-fault-partition",
		}, &stdout, &stderr)
	}()

	waitFor(t, &stdout, "UNAVAILABLE (", 3*time.Second)
	cancel()

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("sync run exited %d; stderr:\n%s", code, stderr.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for sync run shutdown")
	}

	out := stdout.String()
	if !strings.Contains(out, "OFFLINE (network off)") || !strings.Contains(out, "state UNAVAILABLE (") {
		t.Errorf("expected offline + UNAVAILABLE state hash message:\n%s", out)
	}
}

func TestSyncRunAndOnceRejectInvalidDurations(t *testing.T) {
	t.Setenv("OFFGRID_DB", "")
	t.Setenv("OFFGRID_SYNC_KEY", "0123456789abcdef0123")
	db := filepath.Join(t.TempDir(), "x.db")

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"run every 0", []string{"sync", "run", "-db", db, "-peer", "http://127.0.0.1:1", "-every", "0"}, "-every"},
		{"run every negative", []string{"sync", "run", "-db", db, "-peer", "http://127.0.0.1:1", "-every", "-1s"}, "-every"},
		{"run round-timeout 0", []string{"sync", "run", "-db", db, "-peer", "http://127.0.0.1:1", "-round-timeout", "0"}, "-round-timeout"},
		{"run round-timeout negative", []string{"sync", "run", "-db", db, "-peer", "http://127.0.0.1:1", "-round-timeout", "-5s"}, "-round-timeout"},
		{"once timeout 0", []string{"sync", "once", "-db", db, "-peer", "http://127.0.0.1:1", "-timeout", "0"}, "-timeout"},
		{"once timeout negative", []string{"sync", "once", "-db", db, "-peer", "http://127.0.0.1:1", "-timeout", "-2s"}, "-timeout"},
		{"serve window 0", []string{"sync", "serve", "-db", db, "-window", "0"}, "-window"},
		{"serve window negative", []string{"sync", "serve", "-db", db, "-window", "-5s"}, "-window"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, errOut := runCLI(t, "", tc.args...)
			if code != 2 {
				t.Errorf("got exit %d, want 2 (stderr: %q)", code, errOut)
			}
			if !strings.Contains(errOut, tc.want) {
				t.Errorf("stderr %q does not mention %q", errOut, tc.want)
			}
		})
	}
}

func TestSyncSummaryEmptyVsMissingAndHashFormat(t *testing.T) {
	t.Setenv("OFFGRID_DB", "")

	// Missing db must exit 3 (reads never create)
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.db")
	code, _, errOut := runCLI(t, "", "sync", "summary", "-db", missing)
	if code != exitNoDatabase {
		t.Fatalf("missing db summary exited %d, want %d (stderr: %q)", code, exitNoDatabase, errOut)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("sync summary created files on missing db: %v", ents)
	}

	// Empty store prints valid 64-hex hash
	emptyPath := filepath.Join(dir, "empty.db")
	st, err := sqlstore.Open(context.Background(), emptyPath, sqlstore.Options{NodeID: "empty-node"})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	code, out, errOut := runCLI(t, "", "sync", "summary", "-db", emptyPath)
	if code != 0 {
		t.Fatalf("empty db summary failed (%d): %s", code, errOut)
	}
	if !strings.Contains(out, "holds:       nothing yet") {
		t.Errorf("expected 'nothing yet' in summary:\n%s", out)
	}
	if !regexp.MustCompile(`state hash:\s+[0-9a-f]{64}`).MatchString(out) {
		t.Errorf("expected 64-char hex state hash in summary:\n%s", out)
	}
}

func TestSyncOnceAdversarialFailuresLeaveStoreClean(t *testing.T) {
	t.Setenv("OFFGRID_DB", "")
	_, keyFile := setupSyncKeyFile(t)

	db := filepath.Join(t.TempDir(), "local.db")
	if code, _, e := runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"local task"}`); code != 0 {
		t.Fatalf("exec: %d %s", code, e)
	}
	_, sumBefore, errOut := runCLI(t, "", "sync", "summary", "-db", db)
	if errOut != "" {
		t.Fatalf("summary failed: %s", errOut)
	}

	// 1. Rogue peer returns unsigned 200 OK JSON: CLI must exit 1 and not mutate store.
	badSigSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"node":"rogue","protocol":1,"summary":{}}`))
	}))
	defer badSigSrv.Close()

	code, out, errOut := runCLI(t, "", "sync", "once", "-db", db, "-peer", badSigSrv.URL, "-key-file", keyFile, "-timeout", "3s")
	if code != 1 {
		t.Fatalf("expected exit 1 on unsigned peer reply, got %d (out=%q err=%q)", code, out, errOut)
	}
	if !strings.Contains(errOut, "not signed") {
		t.Errorf("expected signature failure on stderr, got %q", errOut)
	}

	// 2. Malformed URL fails cleanly with exit 1.
	code, _, errOut = runCLI(t, "", "sync", "once", "-db", db, "-peer", "://bad-url", "-key-file", keyFile, "-timeout", "3s")
	if code != 1 || errOut == "" {
		t.Fatalf("expected exit 1 on bad peer URL, got %d (%q)", code, errOut)
	}

	_, sumAfter, errOut := runCLI(t, "", "sync", "summary", "-db", db)
	if errOut != "" || sumAfter != sumBefore {
		t.Fatalf("store changed after failed syncs:\nbefore:\n%s\nafter:\n%s", sumBefore, sumAfter)
	}
}
