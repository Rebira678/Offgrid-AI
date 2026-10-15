package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func runAgentRoute(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	full := append([]string{"agent"}, args...)
	code := runIn(full, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func routeQueue(t *testing.T) string {
	t.Setenv("OFFGRID_QUEUE", "")
	t.Setenv("OFFGRID_DB", "")
	return filepath.Join(t.TempDir(), "data", "queue.db")
}

// The Day 10 "done when", end to end through the command line: no network, no
// model. Simple requests are answered at once; the hard one waits in the queue
// with a status a person can read.
func TestRouteOfflineSimpleNowHardQueued(t *testing.T) {
	q := routeQueue(t)
	msgs := "what's still open?\n" +
		"new task: check the fuel line, site 2, high priority\n" +
		"summarize this week's incidents and suggest priorities\n" +
		"queue\n"
	code, out, errOut := runAgentRoute(t, msgs, "-seed", "-no-model", "-route", "-net", "offline", "-queue", q)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{
		"routing on. connectivity: OFFLINE (forced with -net offline)",
		"[tier 0 (rules), rule list,",
		"[tier 0 (rules), rule create-task,",
		"[deferred to queue,",
		"Saved for later as job #1",
		"You are offline",
		"[queue status,",
		"1 waiting, 0 running",
		"ROUTES  answered now: 2   deferred to the queue: 1   queue questions: 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	// the job really is in the durable queue, readable by the queue command
	var o, e bytes.Buffer
	if code := run([]string{"queue", "-q", q, "list"}, &o, &e); code != 0 || !strings.Contains(o.String(), "deep_think") || !strings.Contains(o.String(), "suggest priorities") {
		t.Errorf("queue list: %d\n%s%s", code, o.String(), e.String())
	}
}

func TestRouteQueuePersistsAcrossRunsAndRepeatsDoNotDuplicate(t *testing.T) {
	q := routeQueue(t)
	hard := "what should I prioritize this week?\n"
	runAgentRoute(t, hard, "-no-model", "-route", "-net", "offline", "-queue", q)
	_, out, _ := runAgentRoute(t, hard, "-no-model", "-route", "-net", "offline", "-queue", q) // a new process, same question
	if !strings.Contains(out, "Already saved as job #1") || !strings.Contains(out, "Not added again") {
		t.Errorf("second run:\n%s", out)
	}
	_, out, _ = runAgentRoute(t, "queue\n", "-no-model", "-route", "-net", "offline", "-queue", q)
	if !strings.Contains(out, "1 waiting") || !strings.Contains(out, "prioritize") {
		t.Errorf("a later run must still see the job:\n%s", out)
	}
}

func TestRouteDeferredMessagesAreNotCountedAsFailures(t *testing.T) {
	q := routeQueue(t)
	// three messages so the tier table prints; the third needs a model that is absent
	msgs := "list open tasks\nsummary\nI finished replacing the gasket on the north pump\n"
	_, out, _ := runAgentRoute(t, msgs, "-seed", "-no-model", "-route", "-net", "offline", "-queue", q)
	table := out[strings.Index(out, "TIER"):strings.Index(out, "ROUTES")]
	if strings.Contains(table, "none") || strings.Contains(table, "failed") || !strings.Contains(table, "2 ok") {
		t.Errorf("a message the queue took over is not a failed answer; table:\n%s", table)
	}
	if !strings.Contains(out, "deferred to the queue: 1") || !strings.Contains(out, "I could not read it with the local model now") {
		t.Errorf("output:\n%s", out)
	}
}

func TestRouteOnlineWithoutACloudStillQueues(t *testing.T) {
	q := routeQueue(t)
	_, out, _ := runAgentRoute(t, "why does the fuel line keep cracking\n", "-no-model", "-route", "-net", "online", "-queue", q)
	if !strings.Contains(out, "ONLINE (forced with -net online)") || !strings.Contains(out, "background worker") {
		t.Errorf("%s", out)
	}
}

func TestRouteAutoUsesTheRealProbe(t *testing.T) {
	q := routeQueue(t)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	good := s.URL
	_, out, _ := runAgentRoute(t, "why does the fuel line keep cracking\n", "-no-model", "-route", "-net", "auto", "-url", good, "-queue", q)
	if !strings.Contains(out, "probing the network") || !strings.Contains(out, "You are online") {
		t.Errorf("server up:\n%s", out)
	}
	s.Close() // nothing is listening now
	q2 := filepath.Join(t.TempDir(), "q2.db")
	_, out, _ = runAgentRoute(t, "why does the fuel line keep cracking\n", "-no-model", "-route", "-net", "auto", "-url", good, "-queue", q2)
	if !strings.Contains(out, "You are offline") {
		t.Errorf("server down:\n%s", out)
	}
}

func TestRouteQueueFileFallsBackToTheDBFolder(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OFFGRID_QUEUE", "")
	t.Setenv("OFFGRID_DB", filepath.Join(dir, "tasks.db"))
	code, _, errOut := runAgentRoute(t, "why does the fuel line keep cracking\n", "-no-model", "-route", "-net", "offline")
	if code != 0 {
		t.Fatalf("%d %s", code, errOut)
	}
	var o, e bytes.Buffer
	if run([]string{"queue", "-q", filepath.Join(dir, "queue.db"), "stats"}, &o, &e) != 0 || !strings.Contains(o.String(), "total    1") {
		t.Errorf("queue.db should be next to the tasks database: %s%s", o.String(), e.String())
	}
}

func TestRouteUsageErrors(t *testing.T) {
	q := routeQueue(t)
	if code, _, errOut := runAgentRoute(t, "x\n", "-route", "-net", "sideways", "-queue", q); code != 2 || !strings.Contains(errOut, "-net must be") {
		t.Errorf("bad -net: %d %q", code, errOut)
	}
	if code, _, errOut := runAgentRoute(t, "x\n", "-route", "-net", "offline"); code != 2 || !strings.Contains(errOut, "needs a queue file") {
		t.Errorf("no queue file: %d %q", code, errOut)
	}
}

func TestWithoutRouteNothingChanges(t *testing.T) {
	// Without -route the agent behaves exactly as on Day 5: a message the rules
	// cannot read fails politely instead of being queued, and no queue exists.
	q := routeQueue(t)
	t.Setenv("OFFGRID_QUEUE", q)
	_, out, _ := runAgentRoute(t, "summarize this week's incidents and suggest priorities\n", "-no-model")
	if !strings.Contains(out, "failed") || strings.Contains(out, "deferred") {
		t.Errorf("%s", out)
	}
	if _, err := filepath.Glob(q); err != nil {
		t.Fatal(err)
	}
	var o, e bytes.Buffer
	if code := run([]string{"queue", "-q", q, "stats"}, &o, &e); code != 1 {
		t.Errorf("no queue file should have been created without -route (exit %d)", code)
	}
}

func TestRouteEvalReportsHonestly(t *testing.T) {
	code, out, _ := runAgentRoute(t, "", "-route-eval")
	if code != 0 {
		t.Fatal(code)
	}
	for _, want := range []string{"tuned-on cases", "held-out cases", "false defers:", "missed hard:", "written by the project author", "never used to tune"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
}

func TestRouteUsageListsRoute(t *testing.T) {
	var out bytes.Buffer
	run([]string{"help"}, &out, &out)
	if !strings.Contains(out.String(), "-route") {
		t.Error("usage does not mention -route")
	}
}
