package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func runQ(t *testing.T, q string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	full := append([]string{"queue"}, args...)
	if q != "" {
		full = []string{"queue", "-q", q}
		full = append(full, args...)
	}
	code := run(full, &out, &errb)
	return code, out.String(), errb.String()
}

func qfile(t *testing.T) string {
	t.Setenv("OFFGRID_QUEUE", "")
	t.Setenv("OFFGRID_DB", "")
	return filepath.Join(t.TempDir(), "sub", "queue.db") // the folder does not exist yet
}

func TestQueueEnqueueIsIdempotent(t *testing.T) {
	q := qfile(t)
	code, out, _ := runQ(t, q, "enqueue", "-key", "k1", "-kind", "summarize", `{"week":41}`)
	if code != 0 || !strings.Contains(out, "queued job 1") {
		t.Fatalf("first: %d %q", code, out)
	}
	code, out, _ = runQ(t, q, "enqueue", "-key", "k1", "-kind", "summarize", `{"week": 41}`)
	if code != 0 || !strings.Contains(out, "already queued") || !strings.Contains(out, "Nothing added") {
		t.Fatalf("repeat: %d %q", code, out)
	}
	code, _, errOut := runQ(t, q, "enqueue", "-key", "k1", "-kind", "summarize", `{"week":42}`)
	if code != 1 || !strings.Contains(errOut, "different job") {
		t.Fatalf("conflict: %d %q", code, errOut)
	}
	_, out, _ = runQ(t, q, "stats")
	if !strings.Contains(out, "total    1") {
		t.Errorf("stats:\n%s", out)
	}
}

// The Day 9 "done when", through the real command line: every enqueue is a
// separate process-like run that opens and closes the file, so this also
// proves each job was on disk before its command returned.
func TestQueue100JobsEnqueuedOfflinePersist(t *testing.T) {
	q := qfile(t)
	for i := 1; i <= 100; i++ {
		code, _, errOut := runQ(t, q, "enqueue", "-key", fmt.Sprintf("offline-%d", i), "-kind", "summarize", fmt.Sprintf(`{"n":%d}`, i))
		if code != 0 {
			t.Fatalf("enqueue %d: %d %s", i, code, errOut)
		}
	}
	_, out, _ := runQ(t, q, "stats")
	if !strings.Contains(out, "queued   100") || !strings.Contains(out, "total    100") {
		t.Fatalf("stats:\n%s", out)
	}
	_, out, _ = runQ(t, q, "list", "-status", "queued")
	if n := strings.Count(out, "summarize"); n != 100 {
		t.Errorf("list shows %d jobs, want 100", n)
	}
	_, out, _ = runQ(t, q, "list", "-n", "5")
	if n := strings.Count(out, "summarize"); n != 5 {
		t.Errorf("list -n 5 shows %d", n)
	}
}

func TestQueueLifecycleThroughTheCLI(t *testing.T) {
	q := qfile(t)
	runQ(t, q, "enqueue", "-key", "k", "-kind", "summarize", "-max", "2")

	_, out, _ := runQ(t, q, "claim")
	var j struct {
		ID, Attempt int
		Key         string
		Payload     json.RawMessage
	}
	if err := json.Unmarshal([]byte(out), &j); err != nil || j.ID != 1 || j.Attempt != 1 || j.Key != "k" || string(j.Payload) != "{}" {
		t.Fatalf("claim output %q (%v)", out, err)
	}
	if _, out, _ = runQ(t, q, "claim"); !strings.Contains(out, "no job ready") {
		t.Errorf("a running job must not be claimed twice: %q", out)
	}
	code, out, _ := runQ(t, q, "fail", "-error", "model timed out", "1", "1")
	if code != 0 || !strings.Contains(out, "will retry in") {
		t.Fatalf("fail: %d %q", code, out)
	}
	if _, out, _ = runQ(t, q, "list"); !strings.Contains(out, "model timed out") || !strings.Contains(out, "queued") {
		t.Errorf("list:\n%s", out)
	}
	if _, out, _ = runQ(t, q, "claim"); !strings.Contains(out, "no job ready") {
		t.Errorf("claimed before the backoff passed: %q", out)
	}

	// a stale or wrong claim token is refused
	if code, _, errOut := runQ(t, q, "done", "1", "1"); code != 1 || !strings.Contains(errOut, "no longer valid") {
		t.Errorf("done on a job that is not running: %d %q", code, errOut)
	}
	// only a dead job can be requeued
	code, _, errOut := runQ(t, q, "retry", "1")
	if code != 1 || !strings.Contains(errOut, "only a dead job") {
		t.Errorf("retry of a queued job: %d %q", code, errOut)
	}
}

func TestQueueDeadLetterAndRetry(t *testing.T) {
	q := qfile(t)
	runQ(t, q, "enqueue", "-key", "k", "-kind", "summarize")
	runQ(t, q, "claim")
	code, out, _ := runQ(t, q, "fail", "-permanent", "-error", "payload invalid", "1", "1")
	if code != 0 || !strings.Contains(out, "dead") {
		t.Fatalf("%d %q", code, out)
	}
	if _, out, _ = runQ(t, q, "list", "-status", "dead"); !strings.Contains(out, "payload invalid") {
		t.Errorf("dead list must show why:\n%s", out)
	}
	if code, out, _ = runQ(t, q, "retry", "1"); code != 0 || !strings.Contains(out, "queued again") {
		t.Errorf("retry: %d %q", code, out)
	}
	_, out, _ = runQ(t, q, "claim")
	if !strings.Contains(out, `"attempt":1`) {
		t.Errorf("a retried job starts its attempts over: %q", out)
	}
	if code, out, _ = runQ(t, q, "done", "1", "1"); code != 0 || !strings.Contains(out, "done") {
		t.Errorf("done: %d %q", code, out)
	}
	if code, _, errOut := runQ(t, q, "retry", "99"); code != 1 || !strings.Contains(errOut, "no such job") {
		t.Errorf("retry unknown: %d %q", code, errOut)
	}
}

func TestQueuePathFallbacks(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OFFGRID_QUEUE", "")
	t.Setenv("OFFGRID_DB", filepath.Join(dir, "tasks.db"))
	if code, _, _ := runQ(t, "", "enqueue", "-key", "k", "-kind", "x"); code != 0 {
		t.Fatal("enqueue with only OFFGRID_DB set")
	}
	if code, out, _ := runQ(t, filepath.Join(dir, "queue.db"), "stats"); code != 0 || !strings.Contains(out, "total    1") {
		t.Errorf("the queue should live next to the tasks database: %d %q", code, out)
	}
	t.Setenv("OFFGRID_QUEUE", filepath.Join(dir, "other.db"))
	runQ(t, "", "enqueue", "-key", "k2", "-kind", "x")
	if code, out, _ := runQ(t, filepath.Join(dir, "other.db"), "stats"); code != 0 || !strings.Contains(out, "total    1") {
		t.Errorf("OFFGRID_QUEUE should win over OFFGRID_DB: %d %q", code, out)
	}
}

func TestQueueUsageErrors(t *testing.T) {
	q := qfile(t)
	cases := [][]string{
		{},                        // no subcommand
		{"bogus"},                 // unknown
		{"enqueue", "-kind", "x"}, // no key
		{"enqueue", "-key", "k"},  // no kind
		{"done", "1"},             // missing attempt
		{"done", "x", "y"},        // not numbers
		{"fail", "1"},             // missing attempt
		{"retry"},                 // missing id
		{"retry", "1", "2"},       // too many
		{"enqueue", "-key", "k", "-kind", "x", "{}", "{}"},
	}
	for _, args := range cases {
		code, _, _ := runQ(t, q, args...)
		if code != 2 {
			t.Errorf("queue %v: exit %d, want 2", args, code)
		}
	}
	if code, _, errOut := runQ(t, filepath.Join(t.TempDir(), "nope.db"), "list"); code != 1 || !strings.Contains(errOut, "no queue at") {
		t.Errorf("listing a queue that does not exist: %d %q", code, errOut)
	}
	if code, _, errOut := runQ(t, "", "stats"); code != 2 || !strings.Contains(errOut, "no queue file") {
		t.Errorf("no path anywhere: %d %q", code, errOut)
	}
	if code, _, errOut := runQ(t, q, "enqueue", "-key", "k", "-kind", "Bad Kind"); code != 1 || !strings.Contains(errOut, "kind") {
		t.Errorf("invalid kind: %d %q", code, errOut)
	}
}

func TestQueueJitterCSV(t *testing.T) {
	code, out, _ := runQ(t, "", "jitter", "-clients", "50", "-attempt", "3", "-base", "2s", "-seed", "7")
	if code != 0 {
		t.Fatal(code)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if lines[0] != "mode,client,delay_ms" || len(lines) != 1+3*50 {
		t.Fatalf("csv has %d lines, header %q", len(lines), lines[0])
	}
	for _, l := range lines[1:51] {
		if !strings.HasPrefix(l, "none,") || !strings.HasSuffix(l, ",8000") {
			t.Fatalf("no jitter must be exactly 8000 ms: %q", l)
		}
	}
	_, out2, _ := runQ(t, "", "jitter", "-clients", "50", "-attempt", "3", "-base", "2s", "-seed", "7")
	if out != out2 {
		t.Error("the same seed must give the same output")
	}
	if code, _, _ := runQ(t, "", "jitter", "-clients", "0"); code != 2 {
		t.Error("-clients 0 should be refused")
	}
	if code, _, _ := runQ(t, "", "jitter", "-base", "10m", "-max", "1s"); code != 2 {
		t.Error("base above max should be refused")
	}
}

func TestUsageMentionsQueue(t *testing.T) {
	var out bytes.Buffer
	run([]string{"help"}, &out, &out)
	if !strings.Contains(out.String(), "offgrid queue") {
		t.Error("usage does not list queue")
	}
}
