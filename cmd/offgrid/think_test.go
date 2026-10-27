package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"offgrid/internal/llm"
	"offgrid/internal/store/sqlstore"
)

// fakeCloud swaps the cloud client for one that records what it is asked.
func fakeCloud(t *testing.T, reply string, fail error) (calls *atomic.Int64, urls *[]string) {
	t.Helper()
	calls, urls = &atomic.Int64{}, &[]string{}
	old := newCloudClient
	newCloudClient = func(baseURL, key string) llm.Client {
		*urls = append(*urls, baseURL+"|"+key)
		return &llm.Fake{Handler: func(ctx context.Context, req llm.Request) (string, error) {
			calls.Add(1)
			if fail != nil {
				return "", fail
			}
			return reply, nil
		}}
	}
	t.Cleanup(func() { newCloudClient = old })
	return
}

func gate(t *testing.T, open bool) {
	t.Helper()
	old := newIdleGate
	newIdleGate = func(float64) func() bool { return func() bool { return open } }
	t.Cleanup(func() { newIdleGate = old })
}

func answersJSON(t *testing.T, db string) []sqlstore.Answer {
	t.Helper()
	var out, e bytes.Buffer
	if code := run([]string{"answers", "-db", db, "-json"}, &out, &e); code != 0 {
		t.Fatalf("answers: %d %s", code, e.String())
	}
	var list []sqlstore.Answer
	if err := json.Unmarshal(out.Bytes(), &list); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	return list
}

func TestADeepThinkAnswerLandsInTheTaskDatabaseAndIsListed(t *testing.T) {
	useFakeModels(t)
	gate(t, true)
	db, queue := fill(t)
	code, out, e := runDrain(t, "-db", db, "-queue", queue, "-net", "offline", "-once", "-poll", "10ms", "-deep-model", "qwen2.5:7b")
	if code != 0 {
		t.Fatalf("exit %d %s\n%s", code, e, out)
	}
	if !strings.Contains(out, "answers     saved in the task database") {
		t.Errorf("the start-up text does not say where answers go:\n%s", out)
	}
	list := answersJSON(t, db)
	if len(list) != 1 || list[0].Source != "local" || list[0].Model != "qwen2.5:7b" ||
		!strings.Contains(list[0].Question, "summarize this week") || !strings.Contains(list[0].Text, "FAKE ANSWER") {
		t.Fatalf("answers = %+v", list)
	}
	var human bytes.Buffer
	if code := run([]string{"answers", "-db", db}, &human, &bytes.Buffer{}); code != 0 || !strings.Contains(human.String(), "Q: summarize this week") || !strings.Contains(human.String(), "(local)") {
		t.Errorf("human output: %d\n%s", code, human.String())
	}
	// It is an op in the log, so it syncs: the log holds exactly one answer.recorded.
	var ops bytes.Buffer
	run([]string{"store", "-db", db, "-n", "50", "ops"}, &ops, &bytes.Buffer{})
	if n := strings.Count(ops.String(), "answer.recorded"); n != 1 {
		t.Errorf("%d answer.recorded ops in the log:\n%s", n, ops.String())
	}
	if code, _, e := func() (int, string, string) {
		var o, er bytes.Buffer
		c := run([]string{"store", "-db", db, "verify"}, &o, &er)
		return c, o.String(), er.String()
	}(); code != 0 {
		t.Errorf("verify: %s", e)
	}
}

func TestDrainingTwiceNeverAsksTheModelAgainOrDuplicatesTheAnswer(t *testing.T) {
	f := useFakeModels(t)
	gate(t, true)
	db, queue := fill(t)
	runDrain(t, "-db", db, "-queue", queue, "-net", "offline", "-once", "-poll", "10ms")
	runDrain(t, "-db", db, "-queue", queue, "-net", "offline", "-once", "-poll", "10ms")
	if f.chatCalls != 1 || len(answersJSON(t, db)) != 1 {
		t.Fatalf("model calls %d, answers %d", f.chatCalls, len(answersJSON(t, db)))
	}
}

func TestTheIdleGateHoldsTheSlowJobBackAndCostsNoAttempt(t *testing.T) {
	f := useFakeModels(t)
	gate(t, false)
	db, queue := fill(t)
	code, out, _ := runDrain(t, "-db", db, "-queue", queue, "-net", "offline", "-once", "-poll", "10ms", "-idle-wait", "300ms")
	if code != 0 || f.chatCalls != 0 || len(answersJSON(t, db)) != 0 {
		t.Fatalf("the slow model ran on a busy machine: code %d calls %d\n%s", code, f.chatCalls, out)
	}
	if !strings.Contains(out, "1 done") || !strings.Contains(out, "1 still queued") && !strings.Contains(out, "queue now: 1 queued") {
		t.Errorf("the fast job should finish and the slow one stay queued:\n%s", out)
	}
	if kinds := queueKinds(t, queue); !strings.Contains(kinds, "deep_think") {
		t.Errorf("queue:\n%s", kinds)
	}
}

func TestTheCloudIsOffUnlessConfiguredAndUsedOnlyWhileOnline(t *testing.T) {
	f := useFakeModels(t)
	gate(t, true)
	calls, urls := fakeCloud(t, "CLOUD ANSWER", nil)
	db, queue := fill(t)

	// Not configured: the cloud client is never even built.
	runDrain(t, "-db", db, "-queue", queue, "-net", "offline", "-once", "-poll", "10ms")
	if len(*urls) != 0 {
		t.Fatalf("a cloud client was built without being asked for: %v", *urls)
	}

	// Configured but OFFLINE: the local model answers; the cloud is not called.
	db, queue = fill(t)
	t.Setenv("OFFGRID_CLOUD_KEY", "sk-test")
	code, out, e := runDrain(t, "-db", db, "-queue", queue, "-net", "offline", "-once", "-poll", "10ms",
		"-cloud-url", "https://api.example.test", "-cloud-model", "big-1")
	if code != 0 || calls.Load() != 0 {
		t.Fatalf("offline with a cloud configured: code %d cloud calls %d %s\n%s", code, calls.Load(), e, out)
	}
	if !strings.Contains(out, "CLOUD ON") || !strings.Contains(out, "your task data is sent there") {
		t.Errorf("the start-up text must say loudly that data leaves the laptop:\n%s", out)
	}
	if a := answersJSON(t, db); len(a) != 1 || a[0].Source != "local" {
		t.Fatalf("offline answer: %+v", a)
	}

	// Configured and ONLINE: the cloud answers, and the key reached the client.
	db, queue = fill(t)
	before := f.chatCalls
	code, _, e = runDrain(t, "-db", db, "-queue", queue, "-net", "online", "-once", "-poll", "10ms",
		"-cloud-url", "https://api.example.test", "-cloud-model", "big-1")
	if code != 0 || calls.Load() != 1 || f.chatCalls != before {
		t.Fatalf("online: code %d cloud calls %d local calls %d %s", code, calls.Load(), f.chatCalls-before, e)
	}
	if a := answersJSON(t, db); len(a) != 1 || a[0].Source != "cloud" || a[0].Text != "CLOUD ANSWER" || a[0].Model != "big-1" {
		t.Fatalf("online answer: %+v", a)
	}
	if got := (*urls)[len(*urls)-1]; got != "https://api.example.test|sk-test" {
		t.Errorf("client built with %q", got)
	}
}

func TestACloudThatFailsFallsBackToTheLocalModel(t *testing.T) {
	useFakeModels(t)
	gate(t, true)
	calls, _ := fakeCloud(t, "", llm.ErrUnavailable)
	db, queue := fill(t)
	code, _, e := runDrain(t, "-db", db, "-queue", queue, "-net", "online", "-once", "-poll", "10ms",
		"-cloud-url", "https://api.example.test", "-cloud-model", "big-1")
	if code != 0 || calls.Load() != 1 {
		t.Fatalf("code %d calls %d %s", code, calls.Load(), e)
	}
	if a := answersJSON(t, db); len(a) != 1 || a[0].Source != "local" {
		t.Fatalf("%+v", a)
	}
}

func TestDrainRefusesHalfAConfiguredCloudAndBadGateValues(t *testing.T) {
	useFakeModels(t)
	db, queue := fill(t)
	for _, args := range [][]string{
		{"-cloud-url", "https://x.test"},
		{"-cloud-model", "m"},
		{"-idle-max-busy", "0"},
		{"-idle-max-busy", "1.5"},
		{"-cloud-url", "https://x.test", "-cloud-model", "m", "-cloud-key-file", "/nonexistent/key"},
	} {
		code, _, e := runDrain(t, append([]string{"-db", db, "-queue", queue, "-once"}, args...)...)
		if code != 2 || !strings.Contains(e, "error:") {
			t.Errorf("%v: code %d %q", args, code, e)
		}
	}
}

func TestAnswersCommandErrorsAndEmptyState(t *testing.T) {
	var o, e bytes.Buffer
	if code := run([]string{"answers"}, &o, &e); code != 2 {
		t.Errorf("no db: %d", code)
	}
	if code := run([]string{"answers", "-db", "/nonexistent/x.db"}, &o, &e); code != 1 {
		t.Errorf("missing db: %d", code)
	}
	db, _ := fill(t)
	o.Reset()
	if code := run([]string{"answers", "-db", db}, &o, &e); code != 0 || !strings.Contains(o.String(), "no answers yet") {
		t.Errorf("empty: %d %q", code, o.String())
	}
	if a := answersJSON(t, db); a == nil || len(a) != 0 {
		t.Errorf("-json of nothing should be []: %#v", a)
	}
}

func TestOnceWaitsForTheMachineToGoQuietAndThenAnswers(t *testing.T) {
	useFakeModels(t)
	start := time.Now()
	old := newIdleGate
	newIdleGate = func(float64) func() bool { return func() bool { return time.Since(start) > 400*time.Millisecond } }
	t.Cleanup(func() { newIdleGate = old })
	db, queue := fill(t)
	code, out, e := runDrain(t, "-db", db, "-queue", queue, "-net", "offline", "-once", "-poll", "20ms", "-idle-wait", "10s")
	if code != 0 || len(answersJSON(t, db)) != 1 {
		t.Fatalf("code %d %s\n%s", code, e, out)
	}
	if !strings.Contains(out, "machine is busy") || !strings.Contains(out, "machine is quiet") {
		t.Errorf("the output never said why the slow job waited:\n%s", out)
	}
}
