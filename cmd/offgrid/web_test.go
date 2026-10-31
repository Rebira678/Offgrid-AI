package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestWebRefusesBadArgumentsBeforeDoingAnything(t *testing.T) {
	db := dbPath(t)
	if code, _, errOut := runCLI(t, "", "exec", "-db", db, `{"intent":"list_tasks"}`); code != 0 {
		t.Fatal(errOut)
	}
	missing := filepath.Join(t.TempDir(), "nope.db")
	for name, c := range map[string]struct {
		args []string
		code int
		msg  string
	}{
		"no db":          {[]string{"web"}, 2, "use -db FILE"},
		"extra argument": {[]string{"web", "-db", db, "nope"}, 2, `unexpected argument "nope"`},
		"all interfaces": {[]string{"web", "-db", db, "-listen", "0.0.0.0:8787"}, 2, "no login and no encryption"},
		"lan address":    {[]string{"web", "-db", db, "-listen", "192.168.1.5:8787"}, 2, "only listens on this machine"},
		"no port":        {[]string{"web", "-db", db, "-listen", "127.0.0.1"}, 2, "host:port"},
		"unknown flag":   {[]string{"web", "-db", db, "-public"}, 2, "flag provided but not defined"},
		"missing db":     {[]string{"web", "-db", missing}, 1, "no database at"},
	} {
		code, out, errOut := runCLI(t, "", c.args...)
		if code != c.code || !strings.Contains(errOut, c.msg) || out != "" {
			t.Errorf("%s: exit %d, want %d; stderr %q; stdout %q", name, code, c.code, errOut, out)
		}
	}
	// A refused start must not create the database it was pointed at.
	if _, err := os.Stat(missing); err == nil {
		t.Error("created a database")
	}
}

func TestWebIsListedInTheUsage(t *testing.T) {
	_, out, _ := runCLI(t, "", "help")
	if !strings.Contains(out, "offgrid web") || !strings.Contains(out, "this machine only") {
		t.Errorf("usage does not mention the dashboard:\n%s", out)
	}
}

// The dashboard as a user meets it: a separate process, a real socket, a real
// browser-style request, and the same database changed from the command line.
func TestTheDashboardRunsFromTheBinaryAndAcceptsASuggestion(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	bin := buildBinary(t)
	dir := t.TempDir()
	db, queue := filepath.Join(dir, "tasks.db"), filepath.Join(dir, "queue.db")
	env := []string{"OFFGRID_NODE=laptop"}
	must := func(args ...string) string {
		t.Helper()
		out, errOut, err := runBin(t, bin, env, args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s%s", args, err, out, errOut)
		}
		return out
	}
	must("exec", "-seed", "-db", db, `{"intent":"list_tasks"}`)
	must("exec", "-as-ai", "-by", "qwen2.5:7b", "-db", db, `{"intent":"update_task","task_ref":"filter","status":"in_progress"}`)
	must("queue", "-q", queue, "enqueue", "-key", "k1", "-kind", "interpret", `{"text":"hello"}`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := exec.CommandContext(ctx, bin, "web", "-db", db, "-queue", queue, "-listen", "127.0.0.1:0")
	srv.Env = append(os.Environ(), env...)
	stdout, _ := srv.StdoutPipe()
	var srvErr lockedBuf
	srv.Stderr = &srvErr
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); srv.Wait() }()
	lines := make(chan string, 2)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	var base string
	select {
	case l := <-lines:
		m := regexp.MustCompile(`node laptop, dashboard on (http://127\.0\.0\.1:\d+)`).FindStringSubmatch(l)
		if m == nil {
			t.Fatalf("unexpected first line %q (stderr: %s)", l, srvErr.String())
		}
		base = m[1]
	case <-time.After(20 * time.Second):
		t.Fatalf("the dashboard did not start; stderr: %s", srvErr.String())
	}

	get := func(path string) (int, string) {
		t.Helper()
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var b strings.Builder
		sc := bufio.NewReader(resp.Body)
		for {
			s, err := sc.ReadString('\n')
			b.WriteString(s)
			if err != nil {
				break
			}
		}
		return resp.StatusCode, b.String()
	}
	if code, body := get("/"); code != 200 || !strings.Contains(body, "<title>Offgrid</title>") {
		t.Fatalf("GET / = %d", code)
	}
	var st struct {
		Node  string `json:"node"`
		Tasks []struct {
			ShortID string `json:"short_id"`
			Status  string `json:"status"`
		} `json:"tasks"`
		Suggestions []struct {
			ID string `json:"id"`
		} `json:"suggestions"`
		Queue *struct {
			Queued int `json:"queued"`
		} `json:"queue"`
	}
	_, body := get("/api/state")
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err, body)
	}
	if st.Node != "laptop" || len(st.Tasks) != 3 || len(st.Suggestions) != 1 || st.Queue == nil || st.Queue.Queued != 1 {
		t.Fatalf("state = %s", body)
	}

	// Accept it the way the page does.
	req, _ := http.NewRequest("POST", base+"/api/suggestions/"+st.Suggestions[0].ID+"/accept", nil)
	req.Header.Set("X-Offgrid-Action", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("accept = %d", resp.StatusCode)
	}
	// The command line, a different process, sees the change: one database, two doors.
	out := must("exec", "-db", db, `{"intent":"list_tasks","status":"in_progress"}`)
	if !strings.Contains(out, "Replace filter") {
		t.Errorf("the accepted change is not in the database:\n%s", out)
	}
	if out := must("suggestions", "-db", db); strings.Contains(out, "laptop.") {
		t.Errorf("the suggestion is still pending:\n%s", out)
	}
}
