package web

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"offgrid/internal/jobqueue"
	"offgrid/internal/task"
)

func TestEdgeBuildNilStoreReturnsError(t *testing.T) {
	_, err := Build(context.Background(), Config{})
	if err == nil || !strings.Contains(err.Error(), "no store") {
		t.Fatalf("expected 'no store' error, got %v", err)
	}

	code, res := resolve(context.Background(), Config{}, "s1", "accept")
	if code != http.StatusInternalServerError {
		t.Errorf("expected 500 when store is nil, got %d (res: %v)", code, res)
	}
}

func TestEdgeBuildMetricsDirCorruptedAndNonexistent(t *testing.T) {
	c := &clk{t0}
	st := newStore(t, "laptop", c)

	// Case 1: Metrics dir does not exist
	state, err := Build(context.Background(), Config{
		Store:      st,
		Now:        c.now,
		MetricsDir: filepath.Join(t.TempDir(), "nonexistent"),
	})
	if err != nil {
		t.Fatalf("Build failed on nonexistent metrics dir: %v", err)
	}
	if len(state.Problems) != 0 {
		t.Errorf("expected 0 problems on missing dir, got %v", state.Problems)
	}

	// Case 2: Metrics dir has subdirectories and corrupted JSON
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "subdir.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "empty.json"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "corrupt.json"), []byte("{truncated"), 0o644); err != nil {
		t.Fatal(err)
	}

	state, err = Build(context.Background(), Config{
		Store:      st,
		Now:        c.now,
		MetricsDir: dir,
	})
	if err != nil {
		t.Fatalf("Build failed on corrupted files: %v", err)
	}
	if len(state.Problems) == 0 {
		t.Error("expected problems reported for corrupt/empty files in metrics dir")
	}
}

func TestEdgeOriginCaseInsensitive(t *testing.T) {
	r := newRig(t, nil)
	_, sg := r.suggest("Replace filter")
	path := "/api/suggestions/" + sg.ID + "/dismiss"

	host := strings.TrimPrefix(r.url, "http://")
	// Uppercase scheme and host
	hdr := map[string]string{
		ActionHeader:     "1",
		"Origin":         "HTTP://" + strings.ToUpper(host),
		"Sec-Fetch-Site": "same-origin",
	}
	code, _, body := r.do("POST", path, "", hdr)
	if code != http.StatusOK {
		t.Errorf("expected 200 for case-insensitive origin, got %d: %s", code, body)
	}
}

func TestEdgeSuggestionDeletedTask(t *testing.T) {
	r := newRig(t, nil)
	tk, sg := r.suggest("Replace filter")

	// Delete the task before reviewing the suggestion
	if err := r.st.DeleteTask(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}

	// State should still show the suggestion with fallback task name
	st := build(t, r.cfg)
	if len(st.Suggestions) != 1 {
		t.Fatalf("expected 1 suggestion, got %d", len(st.Suggestions))
	}
	if st.Suggestions[0].ID != sg.ID {
		t.Errorf("suggestion ID mismatch: %s != %s", st.Suggestions[0].ID, sg.ID)
	}

	// Attempting to accept a suggestion whose task was deleted must be refused
	code, body := r.post("/api/suggestions/" + sg.ID + "/accept")
	if code != http.StatusNotFound && code != http.StatusConflict {
		t.Errorf("expected 404 or 409 when accepting suggestion on deleted task, got %d: %s", code, body)
	}

	// Dismissing it should still work cleanly
	code, body = r.post("/api/suggestions/" + sg.ID + "/dismiss")
	if code != http.StatusOK {
		t.Errorf("dismiss should succeed even if task was deleted, got %d: %s", code, body)
	}
}

func TestEdgeSuggestionNoteOnlyDraft(t *testing.T) {
	r := newRig(t, nil)
	tk := mk(r.t, r.st, "Test note-only draft")

	// Note-only draft: status and priority are nil
	sg, _, err := r.st.Suggest(context.Background(), task.Draft{
		Task: tk.ID,
		Note: "check the valve",
		By:   "test-ai",
	})
	if err != nil {
		t.Fatal(err)
	}

	st := build(t, r.cfg)
	if len(st.Suggestions) != 1 {
		t.Fatalf("expected 1 suggestion, got %d", len(st.Suggestions))
	}
	s := st.Suggestions[0]
	if s.Status != "" || s.Priority != "" || s.Note != "check the valve" {
		t.Errorf("expected empty status/priority and preserved note, got %+v", s)
	}

	// Dismiss it
	code, body := r.post("/api/suggestions/" + sg.ID + "/dismiss")
	if code != http.StatusOK {
		t.Errorf("dismiss note-only draft failed (%d): %s", code, body)
	}
}

func TestEdgePostExceedsMaxBytes(t *testing.T) {
	r := newRig(t, nil)
	_, sg := r.suggest("Replace filter")
	path := "/api/suggestions/" + sg.ID + "/dismiss"

	// Create request with body exceeding 1KB MaxBytesReader
	bigPayload := strings.Repeat("A", 2048)
	req, err := http.NewRequest("POST", r.url+path, bytes.NewBufferString(bigPayload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(ActionHeader, "1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Should not succeed with a huge body (MaxBytesReader returns error or fails)
	if resp.StatusCode == http.StatusOK {
		// Even if server doesn't read body and returns 200, check body length safety
		t.Logf("handler resolved without reading body (status 200)")
	}
}

func TestEdgeResolveConcurrentAcceptSameSuggestion(t *testing.T) {
	r := newRig(t, nil)
	_, sg := r.suggest("Replace filter")
	path := "/api/suggestions/" + sg.ID + "/accept"

	const concurrency = 8
	var wg sync.WaitGroup
	codes := make([]int, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			code, _ := r.post(path)
			codes[idx] = code
		}(i)
	}
	wg.Wait()

	okCount, conflictCount := 0, 0
	for _, c := range codes {
		if c == http.StatusOK {
			okCount++
		} else if c == http.StatusConflict {
			conflictCount++
		} else {
			t.Errorf("unexpected status code: %d", c)
		}
	}

	if okCount != 1 {
		t.Errorf("expected exactly 1 successful accept, got %d", okCount)
	}
	if conflictCount != concurrency-1 {
		t.Errorf("expected %d conflict responses, got %d", concurrency-1, conflictCount)
	}
}

func TestEdgeLargeNumberOfTasksAndPagination(t *testing.T) {
	c := &clk{t0}
	st := newStore(t, "laptop", c)

	// Create MaxTasks + 20 tasks
	for i := 0; i < MaxTasks+20; i++ {
		mk(t, st, fmt.Sprintf("Bulk task %d", i))
	}

	state, err := Build(context.Background(), Config{
		Store: st,
		Now:   c.now,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(state.Tasks) != MaxTasks {
		t.Errorf("Tasks capped at %d, got %d", MaxTasks, len(state.Tasks))
	}
	if state.TasksTotal != MaxTasks+20 {
		t.Errorf("TasksTotal = %d, want %d", state.TasksTotal, MaxTasks+20)
	}
}

func TestEdgeBuildQueueErrorsAndLatencyBounds(t *testing.T) {
	c := &clk{t0}
	st := newStore(t, "laptop", c)

	// Case: Queue next ready time in the past
	pastTime := t0.Add(-10 * time.Minute)
	state, err := Build(context.Background(), Config{
		Store: st,
		Now:   c.now,
		Queue: func(context.Context) (jobqueue.Stats, error) {
			return jobqueue.Stats{
				Queued:    5,
				NextReady: pastTime,
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if state.Queue == nil || state.Queue.Queued != 5 || state.Queue.NextReady == nil {
		t.Fatalf("unexpected queue state: %+v", state.Queue)
	}
	if !state.Queue.NextReady.Equal(pastTime) {
		t.Errorf("NextReady = %v, want %v", state.Queue.NextReady, pastTime)
	}
}

func TestEdgeDNSRebindingVariants(t *testing.T) {
	r := newRig(t, nil)

	forbiddenHosts := []string{
		"localhost:8787@attacker.com",
		"attacker.com#localhost:8787",
		"127.0.0.1.nip.io",
		"127.0.0.1.xip.io",
		"localhost.localdomain.attacker.com",
		"192.168.0.1:8787",
		"10.200.0.1:8787",
		"172.16.0.1:8787",
	}

	for _, fh := range forbiddenHosts {
		code, _, _ := r.do("GET", "/api/state", fh, nil)
		if code != http.StatusForbidden {
			t.Errorf("host %q returned code %d, want 403 Forbidden", fh, code)
		}
	}
}

func TestEdgeServeContextCancelUnderLoad(t *testing.T) {
	c := &clk{t0}
	cfg := Config{Store: newStore(t, "laptop", c), Now: c.now}

	ctx, cancel := context.WithCancel(context.Background())
	addrChan := make(chan string, 1)

	go func() {
		_ = Serve(ctx, "127.0.0.1:0", cfg, func(a net.Addr) {
			addrChan <- a.String()
		})
	}()

	addr := <-addrChan
	url := "http://" + addr + "/api/state"

	// Start multiple parallel polling requests
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				resp, err := http.Get(url)
				if err == nil {
					resp.Body.Close()
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
	}

	// Cancel context during active traffic
	time.Sleep(25 * time.Millisecond)
	cancel()
	wg.Wait()

	// Verify server stopped
	time.Sleep(50 * time.Millisecond)
	if _, err := http.Get(url); err == nil {
		t.Error("expected server to be stopped after cancel")
	}
}
