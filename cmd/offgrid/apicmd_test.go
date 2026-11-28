package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"offgrid/internal/webhook"
)

func apiDB(t *testing.T) string {
	t.Helper()
	db := dbPath(t)
	if code, out, e := runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"Leaking pump","site":"4","priority":"high"}`, `{"intent":"create_task","title":"=HYPERLINK(\"http://evil\")"}`); code != 0 {
		t.Fatalf("%d %s %s", code, out, e)
	}
	return db
}

func TestAPIHelpSpecAndUnknown(t *testing.T) {
	if code, out, _ := runCLI(t, "", "api"); code != 0 || !strings.Contains(out, "offgrid api serve") {
		t.Fatal(out)
	}
	code, out, _ := runCLI(t, "", "api", "spec")
	var spec struct {
		OpenAPI string `json:"openapi"`
		Paths   map[string]any
	}
	if code != 0 || json.Unmarshal([]byte(out), &spec) != nil || spec.OpenAPI == "" || spec.Paths["/v1/tasks"] == nil {
		t.Fatalf("%d %.200s", code, out)
	}
	if code, _, e := runCLI(t, "", "api", "nope"); code != 2 || !strings.Contains(e, "unknown api command") {
		t.Fatal(code, e)
	}
	if code, _, e := runCLI(t, "", "api", "webhook", "nope"); code != 2 || !strings.Contains(e, "unknown webhook command") {
		t.Fatal(code, e)
	}
	if code, out, _ := runCLI(t, "", "api", "webhook"); code != 0 || !strings.Contains(out, "task.created") {
		t.Fatal(out)
	}
}

func TestAPIExportImportRoundTripAndDryRun(t *testing.T) {
	src := apiDB(t)
	code, csv, e := runCLI(t, "", "api", "export", "-db", src)
	if code != 0 || !strings.HasPrefix(csv, "id,title,site,") || !strings.Contains(csv, "Leaking pump") || strings.Contains(csv, "\n=HYPER") || !strings.Contains(csv, "'=HYPERLINK") {
		t.Fatalf("%d %s %s", code, csv, e)
	}
	file := filepath.Join(t.TempDir(), "in.csv")
	os.WriteFile(file, []byte(csv+"\n,,\n,,,bogus,,\nno-title-here\n"), 0o600)
	dst := dbPath(t)
	runCLI(t, "", "exec", "-db", dst, `{"intent":"list_tasks"}`)
	code, out, _ := runCLI(t, "", "api", "import", "-db", dst, "-dry-run", file)
	if !strings.Contains(out, "would create 2 of") {
		t.Fatalf("%d %s", code, out)
	}
	_, list, _ := runCLI(t, "", "exec", "-db", dst, `{"intent":"list_tasks"}`)
	if !strings.Contains(list, "No matching tasks.") {
		t.Fatalf("a dry run changed the database: %s", list)
	}
	code, out, _ = runCLI(t, "", "api", "import", "-db", dst, file)
	if !strings.Contains(out, "created 2 of") {
		t.Fatalf("%d %s", code, out)
	}
	_, list, _ = runCLI(t, "", "exec", "-db", dst, `{"intent":"list_tasks"}`)
	if !strings.Contains(list, "Leaking pump") || !strings.Contains(list, `=HYPERLINK("http://evil")`) || strings.Contains(list, "'=HYPERLINK") {
		t.Fatalf("the round trip must give the same titles back: %s", list)
	}
	// again: nothing new, two skipped
	if _, out, _ = runCLI(t, "", "api", "import", "-db", dst, file); !strings.Contains(out, "created 0 of") || strings.Count(out, "skipped:") != 2 {
		t.Fatal(out)
	}
	// bad rows make the exit code non-zero and are listed
	bad := filepath.Join(t.TempDir(), "bad.csv")
	os.WriteFile(bad, []byte("title,status\nok one,open\nbad one,exploded\n"), 0o600)
	code, out, _ = runCLI(t, "", "api", "import", "-db", dst, bad)
	if code != 1 || !strings.Contains(out, "row 3 failed") || !strings.Contains(out, "created 1 of 2") {
		t.Fatalf("%d %s", code, out)
	}
	// usage and refusals
	if code, _, e := runCLI(t, "", "api", "import", "-db", dst); code != 2 || !strings.Contains(e, "usage") {
		t.Fatal(code, e)
	}
	if code, _, _ := runCLI(t, "", "api", "import", "-db", dst, filepath.Join(t.TempDir(), "missing.csv")); code != 1 {
		t.Fatal(code)
	}
	empty := filepath.Join(t.TempDir(), "e.csv")
	os.WriteFile(empty, nil, 0o600)
	if code, _, e := runCLI(t, "", "api", "import", "-db", dst, empty); code != 1 || !strings.Contains(e, "empty") {
		t.Fatal(code, e)
	}
	big := filepath.Join(t.TempDir(), "big.csv")
	bf, _ := os.Create(big)
	bf.WriteString("title\nx\n")
	bf.Truncate(9 << 20)
	bf.Close()
	if code, _, e := runCLI(t, "", "api", "import", "-db", dst, big); code != 1 || !strings.Contains(e, "nothing was imported") {
		t.Fatal(code, e)
	}
	if code, _, e := runCLI(t, "", "api", "export"); code != 2 || !strings.Contains(e, "-db") {
		t.Fatal(code, e)
	}
	if code, _, _ := runCLI(t, "", "api", "export", "-db", filepath.Join(t.TempDir(), "none.db")); code == 0 {
		t.Fatal("a missing database must fail, not make an empty one")
	}
}

func TestAPIServeIsLoopbackOnlyAndWorks(t *testing.T) {
	db := apiDB(t)
	var o, e syncBuf
	for _, addr := range []string{"0.0.0.0:0", ":0", "192.168.1.5:8090"} {
		if code := apiServe(context.Background(), []string{"-db", db, "-addr", addr}, &o, &e); code != 2 {
			t.Fatalf("%s: %d", addr, code)
		}
	}
	if !strings.Contains(e.String(), "API has no login") {
		t.Fatal(e.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	var out, errOut syncBuf
	done := make(chan int, 1)
	go func() { done <- apiServe(ctx, []string{"-db", db, "-addr", "127.0.0.1:0"}, &out, &errOut) }()
	m := waitMatch(t, &out, regexp.MustCompile(`http://(127\.0\.0\.1:\d+)/v1/`))
	base := "http://" + m[1]
	cl := &http.Client{Timeout: 10 * time.Second}
	res, err := cl.Get(base + "/v1/tasks")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(b), "Leaking pump") {
		t.Fatalf("%d %s", res.StatusCode, b)
	}
	post := func(h map[string]string, host string) int {
		req, _ := http.NewRequest("POST", base+"/v1/tasks", strings.NewReader(`{"title":"via api"}`))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range h {
			req.Header.Set(k, v)
		}
		if host != "" {
			req.Host = host
		}
		r, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	if c := post(nil, ""); c != 403 {
		t.Fatalf("a write without the header: %d", c)
	}
	if c := post(map[string]string{"X-Offgrid-Action": "1"}, "evil.example"); c != 403 {
		t.Fatalf("a rebound host: %d", c)
	}
	if c := post(map[string]string{"X-Offgrid-Action": "1"}, ""); c != 201 {
		t.Fatalf("a proper write: %d", c)
	}
	imp, _ := http.NewRequest("POST", base+"/v0/import/tasks.csv?dry_run=true", strings.NewReader("title\nx\n"))
	imp.Header.Set("X-Offgrid-Action", "1")
	if r, err := cl.Do(imp); err != nil || r.StatusCode != 200 {
		t.Fatalf("import is mounted on the local API: %v", err)
	}
	cancel()
	select {
	case c := <-done:
		if c != 0 {
			t.Fatalf("exit %d: %s", c, errOut.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("did not stop")
	}
	if code := apiServe(context.Background(), []string{"-db", filepath.Join(t.TempDir(), "none.db")}, &o, &e); code == 0 {
		t.Fatal("no database")
	}
	if code := apiServe(context.Background(), nil, &o, &e); code != 2 && os.Getenv("OFFGRID_DB") == "" {
		t.Fatal(code)
	}
}

func TestWebhookCommands(t *testing.T) {
	db := apiDB(t)
	var hits atomic.Int64
	var lastSig, lastBody atomic.Value
	rc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		hits.Add(1)
		lastSig.Store(r.Header.Get(webhook.HeaderSignature) + "|" + r.Header.Get(webhook.HeaderTimestamp))
		lastBody.Store(string(b))
	}))
	defer rc.Close()

	if code, out, _ := runCLI(t, "", "api", "webhook", "list", "-db", db); code != 0 || !strings.Contains(out, "no webhooks") {
		t.Fatal(out)
	}
	if code, _, e := runCLI(t, "", "api", "webhook", "add", "-db", db, "-url", "ftp://x"); code != 1 || e == "" {
		t.Fatal(code, e)
	}
	if code, _, e := runCLI(t, "", "api", "webhook", "add", "-db", db, "-url", rc.URL, "-event", "task.deleted"); code != 1 || !strings.Contains(e, "task.created") {
		t.Fatal(code, e)
	}
	code, out, e := runCLI(t, "", "api", "webhook", "add", "-db", db, "-url", rc.URL, "-event", "task.created", "-include-text")
	if code != 0 {
		t.Fatal(code, e)
	}
	m := regexp.MustCompile(`added (wh_\w+) -> \S+\n\s+secret \(shown once, keep it\): ([0-9a-f]{64})`).FindStringSubmatch(out)
	if m == nil {
		t.Fatal(out)
	}
	id, secret := m[1], m[2]
	path := filepath.Join(filepath.Dir(db), "webhooks.json")
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%v %v", fi, err)
		}
	}
	_, list, _ := runCLI(t, "", "api", "webhook", "list", "-db", db)
	if !strings.Contains(list, id) || strings.Contains(list, secret) || !strings.Contains(list, secret[:4]+"…") || !strings.Contains(list, "task.created") {
		t.Fatalf("list must show the hook but not the secret:\n%s", list)
	}

	// test sends a ping, signed with the secret
	if code, out, e := runCLI(t, "", "api", "webhook", "test", "-db", db, "-id", id); code != 0 || !strings.Contains(out, "delivered") {
		t.Fatal(code, out, e)
	}
	sig := strings.SplitN(lastSig.Load().(string), "|", 2)
	if err := webhook.Verify(secret, sig[0], sig[1], []byte(lastBody.Load().(string)), time.Now()); err != nil {
		t.Fatal(err)
	}
	if code, _, e := runCLI(t, "", "api", "webhook", "test", "-db", db, "-id", "wh_nope"); code != 1 || !strings.Contains(e, "no hook") {
		t.Fatal(code, e)
	}

	// run: only what happens after add is delivered, and only the chosen type
	ctx, cancel := context.WithCancel(context.Background())
	var out2, err2 syncBuf
	done := make(chan int, 1)
	go func() { done <- webhookCmd(ctx, []string{"run", "-db", db, "-poll", "20ms"}, &out2, &err2) }()
	waitMatch(t, &out2, regexp.MustCompile(`1 hook\(s\)`))
	before := hits.Load()
	runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"after the hook"}`)
	for end := time.Now().Add(10 * time.Second); hits.Load() == before && time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
	}
	if hits.Load() != before+1 || !strings.Contains(lastBody.Load().(string), "after the hook") || strings.Contains(lastBody.Load().(string), "Leaking pump") {
		t.Fatalf("hits %d (before %d) body %s", hits.Load(), before, lastBody.Load())
	}
	cancel()
	if c := <-done; c != 0 || !strings.Contains(out2.String(), "stopped: 1 delivered, 0 given up") {
		t.Fatalf("%d %s %s", c, out2.String(), err2.String())
	}

	if code, out, _ := runCLI(t, "", "api", "webhook", "remove", "-db", db, "-id", id); code != 0 || !strings.Contains(out, "removed") {
		t.Fatal(out)
	}
	if code, _, e := runCLI(t, "", "api", "webhook", "remove", "-db", db, "-id", id); code != 1 || !strings.Contains(e, "no hook") {
		t.Fatal(code, e)
	}
	if _, list, _ := runCLI(t, "", "api", "webhook", "list", "-db", db); !strings.Contains(list, "no webhooks") {
		t.Fatal(list)
	}
	// a ping to a dead receiver fails with a non-zero exit
	runCLI(t, "", "api", "webhook", "add", "-db", db, "-url", "http://127.0.0.1:1/x")
	_, l, _ := runCLI(t, "", "api", "webhook", "list", "-db", db)
	id2 := regexp.MustCompile(`wh_\w+`).FindString(l)
	if code, _, e := runCLI(t, "", "api", "webhook", "test", "-db", db, "-id", id2); code != 1 || !strings.Contains(e, "ping failed") {
		t.Fatal(code, e)
	}
}
