package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"offgrid/internal/agent"
	"offgrid/internal/intent"
	"offgrid/internal/llm"
	"offgrid/internal/netfault"
	"offgrid/internal/structured"
)

// fakeModels replaces the real Ollama clients for the duration of a test.
type fakeModels struct {
	mu        sync.Mutex
	chatHosts []string
	chatCalls int
}

func useFakeModels(t *testing.T) *fakeModels {
	t.Helper()
	f := &fakeModels{}
	oldChat, oldClass := newChatClient, newClassifier
	newChatClient = func(host string) llm.Client {
		f.mu.Lock()
		f.chatHosts = append(f.chatHosts, host)
		f.mu.Unlock()
		return &llm.Fake{Handler: func(ctx context.Context, req llm.Request) (string, error) {
			f.mu.Lock()
			f.chatCalls++
			f.mu.Unlock()
			return "FAKE ANSWER based on: " + strings.SplitN(req.Messages[1].Content, "\n", 3)[1], nil
		}}
	}
	newClassifier = func(host, model string, attempts int) agent.Model { return fakeClassifier{} }
	t.Cleanup(func() { newChatClient, newClassifier = oldChat, oldClass })
	return f
}

type fakeClassifier struct{}

func (fakeClassifier) Classify(ctx context.Context, text string) (structured.Result[intent.Command], error) {
	return structured.Result[intent.Command]{Value: intent.Command{Intent: intent.CreateTask, Title: "Pump 3 smells of gas", Site: "3", Priority: "high"}}, nil
}

func runDrain(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(append([]string{"drain"}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

// fill saves one hard request and one the model must read, the way a real
// offline session would, through the real command line. Returns the files.
func fill(t *testing.T) (db, queue string) {
	t.Helper()
	dir := t.TempDir()
	db, queue = filepath.Join(dir, "tasks.db"), filepath.Join(dir, "queue.db")
	t.Setenv("OFFGRID_DB", "")
	t.Setenv("OFFGRID_QUEUE", "")
	t.Setenv("OFFGRID_FAULT", "")
	// one message per line on standard input (arguments would be joined into one)
	code, out, e := runAgentRoute(t, "summarize this week's incidents and suggest priorities\nplease jot down that pump three smells of gas\n",
		"-seed", "-no-model", "-route", "-net", "offline", "-db", db, "-queue", queue)
	if code != 0 {
		t.Fatalf("fill: exit %d: %s", code, e)
	}
	if strings.Count(out, "[deferred to queue") != 2 {
		t.Fatalf("setup: both requests should have been saved:\n%s", out)
	}
	return db, queue
}

func queueKinds(t *testing.T, queue string) string {
	t.Helper()
	var out, errb bytes.Buffer
	run([]string{"queue", "-q", queue, "list"}, &out, &errb)
	return out.String()
}

func TestDrainOnceRunsEverythingThatNeedsNoNetwork(t *testing.T) {
	f := useFakeModels(t)
	db, queue := fill(t)

	code, out, e := runDrain(t, "-db", db, "-queue", queue, "-net", "offline", "-once", "-poll", "10ms")
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, e, out)
	}
	for _, want := range []string{
		"connectivity: OFFLINE (forced",
		"deep_think  asks the local model",
		"started (try 1 of 5)",
		"DONE:",
		"drain finished: 2 done, 0 will retry, 0 dead",
		"queue now: 0 queued, 0 running, 2 done, 0 dead",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// the answer is stored and readable
	var res bytes.Buffer
	if code := run([]string{"queue", "-q", queue, "result", "1"}, &res, &bytes.Buffer{}); code != 0 || !strings.Contains(res.String(), "FAKE ANSWER") {
		t.Errorf("queue result 1: %d %q", code, res.String())
	}
	// the interpret job really changed the task list
	var list bytes.Buffer
	run([]string{"agent", "-no-model", "-db", db, "list open tasks"}, &list, &bytes.Buffer{})
	if !strings.Contains(list.String(), "Pump 3 smells of gas") {
		t.Errorf("the deferred message was not carried out:\n%s", list.String())
	}
	if f.chatCalls != 1 {
		t.Errorf("deep_think model calls = %d", f.chatCalls)
	}
	// the model was shown the real tasks (the seeded ones), not asked to guess
	if !strings.Contains(res.String(), "T1") {
		t.Errorf("the answer was not based on the task data: %q", res.String())
	}
}

func TestDrainRunTwiceDoesNothingTheSecondTime(t *testing.T) {
	useFakeModels(t)
	db, queue := fill(t)
	runDrain(t, "-db", db, "-queue", queue, "-net", "offline", "-once", "-poll", "10ms")
	code, out, _ := runDrain(t, "-db", db, "-queue", queue, "-net", "offline", "-once", "-poll", "10ms")
	if code != 0 || !strings.Contains(out, "0 done") || strings.Contains(out, "started") {
		t.Errorf("a second drain re-ran finished jobs: %d\n%s", code, out)
	}
	var list bytes.Buffer
	run([]string{"agent", "-no-model", "-db", db, "list open tasks"}, &list, &bytes.Buffer{})
	if n := strings.Count(list.String(), "Pump 3 smells of gas"); n != 1 {
		t.Errorf("the task exists %d times", n)
	}
}

func TestDrainRemoteDeepThinkWaitsForTheNetwork(t *testing.T) {
	f := useFakeModels(t)
	db, queue := fill(t)

	code, out, e := runDrain(t, "-db", db, "-queue", queue, "-net", "offline", "-once", "-poll", "10ms", "-deep-host", "bigbox:11434")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, e)
	}
	for _, want := range []string{
		"asks model", "on bigbox:11434 (needs the network: waits while offline)",
		"network: network is down",
		"drain finished: 1 done",
		"1 still queued",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if f.chatCalls != 0 {
		t.Error("the remote model was called while offline")
	}
	if !strings.Contains(queueKinds(t, queue), "queued") {
		t.Error("the deep_think job should still be queued")
	}

	// "reconnect"
	code, out, e = runDrain(t, "-db", db, "-queue", queue, "-net", "online", "-once", "-poll", "10ms", "-deep-host", "bigbox:11434")
	if code != 0 || !strings.Contains(out, "drain finished: 1 done") || !strings.Contains(out, "network: network is up") {
		t.Fatalf("after reconnect: %d %s\n%s", code, e, out)
	}
	found := false
	for _, h := range f.chatHosts {
		found = found || h == "bigbox:11434"
	}
	if !found {
		t.Errorf("the deep model client was built for %v, want the -deep-host", f.chatHosts)
	}
	if st := queueKinds(t, queue); strings.Contains(st, "queued") {
		t.Errorf("jobs left after reconnect:\n%s", st)
	}
}

// Day 11 and Day 12 together: the network is "cut" by the fault switch, the
// worker waits, the switch is flipped back, and the queue drains by itself.
func TestDrainWaitsOutAnInjectedPartitionAndResumes(t *testing.T) {
	useFakeModels(t)
	db, queue := fill(t)
	srv := probeServer(204)
	defer srv.Close()
	fault := filepath.Join(t.TempDir(), "fault")
	if err := netfault.WriteState(fault, true); err != nil {
		t.Fatal(err)
	}

	out := &lockedBuf{}
	done := make(chan int, 1)
	go func() {
		done <- run([]string{"drain", "-db", db, "-queue", queue, "-net", "auto", "-url", srv.URL,
			"-probe-interval", "20ms", "-poll", "20ms", "-deep-host", "bigbox:11434",
			"-fault-file", fault, "-for", "4s"}, out, &lockedBuf{})
	}()
	waitFor(t, out, "network: network is down", 3*time.Second)
	waitFor(t, out, "DONE", 3*time.Second) // the interpret job needs no network
	time.Sleep(150 * time.Millisecond)
	if strings.Count(out.String(), "DONE") != 1 {
		t.Fatalf("the network job ran during the partition:\n%s", out.String())
	}

	if err := netfault.WriteState(fault, false); err != nil { // the reconnect
		t.Fatal(err)
	}
	waitFor(t, out, "network: network is up", 3*time.Second)
	deadline := time.Now().Add(3 * time.Second)
	for strings.Count(out.String(), "DONE") < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if strings.Count(out.String(), "DONE") != 2 {
		t.Fatalf("the queue did not drain after the reconnect:\n%s", out.String())
	}
	if code := <-done; code != 0 {
		t.Errorf("exit %d", code)
	}
	if !strings.Contains(out.String(), "drain stopped") {
		t.Errorf("a drain cut short by -for should say it stopped, not finished:\n%s", out.String())
	}
}

func TestDrainUsageErrors(t *testing.T) {
	useFakeModels(t)
	db, queue := fill(t)
	missing := filepath.Join(t.TempDir(), "nope.db")
	cases := []struct {
		name string
		args []string
		code int
		msg  string
	}{
		{"no task db", []string{"-queue", queue, "-net", "offline", "-once"}, 2, "needs the task database"},
		{"no queue file given", []string{"-db", db, "-net", "offline", "-once"}, 2, "no queue file"},
		{"queue file missing", []string{"-db", db, "-queue", missing, "-net", "offline", "-once"}, 1, "nothing has been saved yet"},
		{"no-model", []string{"-db", db, "-queue", queue, "-no-model"}, 2, "nothing to run"},
		{"concurrency 0", []string{"-db", db, "-queue", queue, "-concurrency", "0"}, 2, "-concurrency must be"},
		{"concurrency huge", []string{"-db", db, "-queue", queue, "-concurrency", "1000"}, 2, "-concurrency must be"},
		{"extra argument", []string{"-db", db, "-queue", queue, "hello"}, 2, "unexpected argument"},
		{"bad net mode", []string{"-db", db, "-queue", queue, "-net", "sideways", "-once"}, 2, "-net must be"},
		{"faults with forced net", []string{"-db", db, "-queue", queue, "-net", "offline", "-fault-partition", "-once"}, 2, "-fault-"},
	}
	for _, c := range cases {
		code, _, e := runDrain(t, c.args...)
		if code != c.code || !strings.Contains(e, c.msg) {
			t.Errorf("%s: exit %d, stderr %q (want %d containing %q)", c.name, code, e, c.code, c.msg)
		}
	}
}

func TestQueueResultCommand(t *testing.T) {
	useFakeModels(t)
	db, queue := fill(t)

	// before anything ran: no result, says why
	var out, errb bytes.Buffer
	if code := run([]string{"queue", "-q", queue, "result", "1"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "no result yet (it is queued") {
		t.Errorf("not yet: %d %q", code, errb.String())
	}
	runDrain(t, "-db", db, "-queue", queue, "-net", "offline", "-once", "-poll", "10ms")
	out.Reset()
	errb.Reset()
	if code := run([]string{"queue", "-q", queue, "result", "1"}, &out, &errb); code != 0 || strings.TrimSpace(out.String()) == "" {
		t.Errorf("after: %d %q %q", code, out.String(), errb.String())
	}
	for name, args := range map[string][]string{
		"unknown job": {"queue", "-q", queue, "result", "99"},
		"bad id":      {"queue", "-q", queue, "result", "abc"},
		"no id":       {"queue", "-q", queue, "result"},
	} {
		out.Reset()
		errb.Reset()
		if code := run(args, &out, &errb); code == 0 || errb.Len() == 0 {
			t.Errorf("%s: exit %d stderr %q", name, code, errb.String())
		}
	}
}

func TestTaskContextShowsTasksAndRecentNotes(t *testing.T) {
	useFakeModels(t)
	db, _ := fill(t)
	_ = db
	// via the registry the way drain builds it
	clk := newSessionClock(nil)
	store, closeStore, err := openTaskStore(context.Background(), db, clk)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore()
	got, err := taskContext(context.Background(), store)
	if err != nil || !strings.Contains(got, "Leaking pump") || !strings.Contains(got, "site 4") {
		t.Errorf("context = %q err=%v", got, err)
	}
	empty, closeEmpty, _ := openTaskStore(context.Background(), "", clk)
	defer closeEmpty()
	if s, _ := taskContext(context.Background(), empty); !strings.Contains(s, "no tasks") {
		t.Errorf("empty = %q", s)
	}
}

func TestUsageMentionsDrain(t *testing.T) {
	var b bytes.Buffer
	usage(&b)
	if !strings.Contains(b.String(), "offgrid drain") {
		t.Error("usage does not mention drain")
	}
}
