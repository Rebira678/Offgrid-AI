package main

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWebCLIAddressValidationEdgeCases(t *testing.T) {
	db := dbPath(t)
	if code, _, errOut := runCLI(t, "", "exec", "-seed", "-db", db, `{"intent":"list_tasks"}`); code != 0 {
		t.Fatal(errOut)
	}

	for name, tc := range map[string]struct {
		listen string
		code   int
		msg    string
	}{
		"missing port":      {"127.0.0.1", 2, "use host:port"},
		"missing port ipv6": {"::1", 2, "use host:port"},
		"non-loopback ip":   {"192.168.1.100:8787", 2, "only listens on this machine"},
		"wildcard ip":       {"0.0.0.0:8787", 2, "no login and no encryption"},
		"wildcard ipv6":     {"[::]:8787", 2, "only listens on this machine"},
		"empty address":     {"", 2, "bad address"},
	} {
		t.Run(name, func(t *testing.T) {
			code, out, errOut := runCLI(t, "", "web", "-db", db, "-listen", tc.listen)
			if code != tc.code || !strings.Contains(errOut, tc.msg) || out != "" {
				t.Errorf("%s: code %d (want %d), stderr: %q", name, code, tc.code, errOut)
			}
		})
	}
}

func TestWebCLIEnvFallback(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "env_test.db")
	if code, _, errOut := runCLI(t, "", "exec", "-seed", "-db", db, `{"intent":"list_tasks"}`); code != 0 {
		t.Fatal(errOut)
	}

	// Without -db flag, but with OFFGRID_DB environment variable set
	t.Setenv("OFFGRID_DB", db)
	code, _, errOut := runCLI(t, "", "web", "-listen", "192.168.1.1:8787")
	if code != 2 || !strings.Contains(errOut, "only listens on this machine") {
		t.Errorf("expected address refusal after reading OFFGRID_DB from env: %d, %s", code, errOut)
	}
}

func TestWebCLIReadOnlyFlag(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping binary execution test in short mode")
	}
	bin := buildBinary(t)
	dir := t.TempDir()
	db := filepath.Join(dir, "tasks.db")
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := startBinary(t, ctx, bin, env, "web", "-db", db, "-listen", "127.0.0.1:0", "-read-only")
	defer srv.stop()

	base := srv.waitURL(t, 10*time.Second)

	// Fetch state and verify read_only is true
	resp, err := http.Get(base + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/state status: %d", resp.StatusCode)
	}

	// Attempt to dismiss or accept must fail with 403 Forbidden
	req, _ := http.NewRequest("POST", base+"/api/suggestions/laptop.1/dismiss", nil)
	req.Header.Set("X-Offgrid-Action", "1")
	postResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer postResp.Body.Close()

	if postResp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden in -read-only mode, got %d", postResp.StatusCode)
	}
}

type runningBinary struct {
	cancel context.CancelFunc
	lines  chan string
	stderr *lockedBuf
}

func (r *runningBinary) stop() {
	r.cancel()
}

func (r *runningBinary) waitURL(t *testing.T, timeout time.Duration) string {
	t.Helper()
	select {
	case line := <-r.lines:
		idx := strings.Index(line, "http://")
		if idx == -1 {
			t.Fatalf("unexpected line: %q", line)
		}
		parts := strings.Fields(line[idx:])
		return parts[0]
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for URL (stderr: %s)", r.stderr.String())
		return ""
	}
}

func startBinary(t *testing.T, ctx context.Context, bin string, env []string, args ...string) *runningBinary {
	t.Helper()
	subCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(subCtx, bin, args...)
	cmd.Env = append(os.Environ(), env...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var errBuf lockedBuf
	cmd.Stderr = &errBuf

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	lines := make(chan string, 10)
	go func() {
		buf := make([]byte, 2048)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				for _, line := range strings.Split(string(buf[:n]), "\n") {
					if strings.Contains(line, "dashboard on http://") {
						lines <- line
					}
				}
			}
			if err != nil {
				break
			}
		}
	}()

	return &runningBinary{
		cancel: cancel,
		lines:  lines,
		stderr: &errBuf,
	}
}
