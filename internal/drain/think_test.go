package drain

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"offgrid/internal/jobqueue"
	"offgrid/internal/llm"
	"offgrid/internal/store/sqlstore"
)

// memBook is a plain in-memory AnswerBook.
type memBook struct {
	mu   sync.Mutex
	m    map[string]Answer
	recs int
	// before, if set, runs inside Record before the answer is looked at:
	// it lets a test play "another attempt got there first".
	before func()
}

func (b *memBook) Lookup(ctx context.Context, key string) (string, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	a, ok := b.m[key]
	return a.Text, ok, nil
}

func (b *memBook) Record(ctx context.Context, a Answer) (string, error) {
	if b.before != nil {
		b.before()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.m == nil {
		b.m = map[string]Answer{}
	}
	b.recs++
	if old, ok := b.m[a.Key]; ok {
		return old.Text, nil
	}
	b.m[a.Key] = a
	return a.Text, nil
}

func says(text string, calls *atomic.Int64) *llm.Fake {
	return &llm.Fake{Handler: func(ctx context.Context, r llm.Request) (string, error) {
		if calls != nil {
			calls.Add(1)
		}
		return text, nil
	}}
}

func failing(err error, calls *atomic.Int64) *llm.Fake {
	return &llm.Fake{Handler: func(ctx context.Context, r llm.Request) (string, error) {
		calls.Add(1)
		return "", err
	}}
}

func mkjob(key, text string) jobqueue.Job {
	return jobqueue.Job{ID: 1, Key: key, Kind: KindDeepThink, Payload: []byte(fmt.Sprintf(`{"text":%q}`, text))}
}

func run(t *testing.T, s Spec, j jobqueue.Job) (string, error) {
	t.Helper()
	return s.Handle(context.Background(), j, nil)
}

func TestThinkAnswersWithTheLocalModelAndRecordsIt(t *testing.T) {
	var local atomic.Int64
	book := &memBook{}
	var asked llm.Request
	loc := &llm.Fake{Handler: func(ctx context.Context, r llm.Request) (string, error) {
		local.Add(1)
		asked = r
		return "T1 is the only open job.", nil
	}}
	spec := Think(ThinkConfig{Local: loc, LocalModel: "qwen2.5:7b", Book: book,
		Context: func(context.Context) (string, error) { return "T1 [high, open] pump", nil }})
	if !spec.Background {
		t.Fatal("slow thinking must be a Background job")
	}
	got, err := run(t, spec, mkjob("k1", "what is open?"))
	if err != nil || got != "T1 is the only open job." {
		t.Fatalf("got %q, %v", got, err)
	}
	a := book.m["k1"]
	if a.Source != "local" || a.Model != "qwen2.5:7b" || a.Question != "what is open?" || a.Text != got {
		t.Fatalf("recorded %+v", a)
	}
	if asked.Model != "qwen2.5:7b" || !strings.Contains(asked.Messages[1].Content, "T1 [high, open] pump") {
		t.Fatalf("the model was not given the task data: %+v", asked)
	}
}

func TestThinkUsesTheCloudOnlyWhenConfiguredAndOnline(t *testing.T) {
	var local, cloud atomic.Int64
	var online atomic.Bool
	book := &memBook{}
	spec := Think(ThinkConfig{Local: says("local words", &local), LocalModel: "l", Cloud: says("cloud words", &cloud), CloudModel: "c",
		Online: online.Load, Book: book})
	got, err := run(t, spec, mkjob("offline", "q"))
	if err != nil || got != "local words" || cloud.Load() != 0 {
		t.Fatalf("offline: %q %v cloud calls %d", got, err, cloud.Load())
	}
	online.Store(true)
	got, err = run(t, spec, mkjob("online", "q"))
	if err != nil || got != "cloud words" || local.Load() != 1 {
		t.Fatalf("online: %q %v local calls %d", got, err, local.Load())
	}
	if book.m["online"].Source != "cloud" || book.m["online"].Model != "c" || book.m["offline"].Source != "local" {
		t.Fatalf("sources: %+v", book.m)
	}
	// Not configured at all: never reaches for the network, whatever Online says.
	spec = Think(ThinkConfig{Local: says("only local", &local), LocalModel: "l", Online: func() bool { return true }})
	if got, _ := run(t, spec, mkjob("x", "q")); got != "only local" {
		t.Fatalf("got %q", got)
	}
}

func TestThinkFallsBackToTheLocalModelWhenTheCloudFails(t *testing.T) {
	for name, cerr := range map[string]error{
		"unreachable": llm.ErrUnavailable, "bad key": llm.ErrRejected,
		"no such model": &llm.ModelNotFoundError{Model: "c"}, "garbage": errors.New("weird"),
	} {
		t.Run(name, func(t *testing.T) {
			var cloud, local atomic.Int64
			spec := Think(ThinkConfig{Local: says("local saves the day", &local), LocalModel: "l",
				Cloud: failing(cerr, &cloud), CloudModel: "c", Online: func() bool { return true }, Book: &memBook{}})
			got, err := run(t, spec, mkjob("k", "q"))
			if err != nil || got != "local saves the day" || cloud.Load() != 1 || local.Load() != 1 {
				t.Fatalf("got %q %v cloud=%d local=%d", got, err, cloud.Load(), local.Load())
			}
		})
	}
}

// countingClient counts calls at the door, before looking at the context, so a
// test can tell "not called" from "called and refused because cancelled".
type countingClient struct {
	n    *atomic.Int64
	text string
}

func (c countingClient) Chat(ctx context.Context, req llm.Request, _ func(string)) (llm.Response, error) {
	c.n.Add(1)
	return llm.Response{Text: c.text}, nil
}

func TestThinkDoesNotFallBackWhenTheJobWasCancelled(t *testing.T) {
	var local atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	cl := &llm.Fake{Handler: func(c context.Context, r llm.Request) (string, error) { cancel(); return "", c.Err() }}
	spec := Think(ThinkConfig{Local: countingClient{&local, "must not run"}, LocalModel: "l", Cloud: cl, CloudModel: "c", Online: func() bool { return true }})
	_, err := spec.Handle(ctx, mkjob("k", "q"), nil)
	if !errors.Is(err, context.Canceled) || local.Load() != 0 {
		t.Fatalf("err=%v local calls=%d: a cancelled job must stop, not start a minute of local inference", err, local.Load())
	}
}

func TestThinkAsksNoModelWhenTheAnswerIsAlreadyHeld(t *testing.T) {
	var local, cloud atomic.Int64
	book := &memBook{m: map[string]Answer{"k": {Key: "k", Text: "already known"}}}
	spec := Think(ThinkConfig{Local: says("new", &local), LocalModel: "l", Cloud: says("new", &cloud), CloudModel: "c", Online: func() bool { return true }, Book: book})
	got, err := run(t, spec, mkjob("k", "q"))
	if err != nil || got != "already known" || local.Load()+cloud.Load() != 0 || book.recs != 0 {
		t.Fatalf("got %q %v calls=%d recs=%d", got, err, local.Load()+cloud.Load(), book.recs)
	}
}

func TestThinkReturnsTheStoredAnswerWhenAnotherAttemptRecordedFirst(t *testing.T) {
	book := &memBook{}
	book.before = func() {
		book.mu.Lock()
		book.m = map[string]Answer{"k": {Key: "k", Text: "the first answer"}}
		book.mu.Unlock()
	}
	spec := Think(ThinkConfig{Local: says("the second answer", nil), LocalModel: "l", Book: book})
	got, err := run(t, spec, mkjob("k", "q"))
	if err != nil || got != "the first answer" {
		t.Fatalf("got %q %v: the job's result and the synced answer must be the same words", got, err)
	}
}

func TestThinkErrors(t *testing.T) {
	var n atomic.Int64
	// the 7-8B model is not installed: retrying cannot fix it
	spec := Think(ThinkConfig{Local: failing(&llm.ModelNotFoundError{Model: "qwen2.5:7b"}, &n), LocalModel: "qwen2.5:7b"})
	_, err := run(t, spec, mkjob("k", "q"))
	if !errors.Is(err, ErrPermanent) || !strings.Contains(err.Error(), "ollama pull qwen2.5:7b") {
		t.Fatalf("%v", err)
	}
	// the server is down: retry later
	spec = Think(ThinkConfig{Local: failing(llm.ErrUnavailable, &n), LocalModel: "m"})
	if _, err = run(t, spec, mkjob("k", "q")); err == nil || errors.Is(err, ErrPermanent) {
		t.Fatalf("an outage must be retryable: %v", err)
	}
	// an empty answer is not an answer
	spec = Think(ThinkConfig{Local: says("   ", nil), LocalModel: "m", Book: &memBook{}})
	if _, err = run(t, spec, mkjob("k", "q")); err == nil {
		t.Fatal("empty answer accepted")
	}
	// a broken payload is permanent
	if _, err = run(t, spec, jobqueue.Job{Payload: []byte(`{"nope":1}`)}); !errors.Is(err, ErrPermanent) {
		t.Fatalf("%v", err)
	}
	// the book failing must not lose the model's answer silently
	spec = Think(ThinkConfig{Local: says("words", nil), LocalModel: "m", Book: badBook{}})
	if _, err = run(t, spec, mkjob("k", "q")); err == nil {
		t.Fatal("a failed save was reported as success")
	}
}

type badBook struct{}

func (badBook) Lookup(context.Context, string) (string, bool, error) { return "", false, nil }
func (badBook) Record(context.Context, Answer) (string, error)       { return "", errors.New("disk full") }

func TestACloudOnlyBrainWaitsForTheNetwork(t *testing.T) {
	spec := Think(ThinkConfig{Cloud: says("c", nil), CloudModel: "c", Online: func() bool { return true }})
	if !spec.NeedsNetwork || !spec.Background {
		t.Fatalf("%+v", spec)
	}
	if got, err := run(t, spec, mkjob("k", "q")); err != nil || got != "c" {
		t.Fatalf("%q %v", got, err)
	}
	if Think(ThinkConfig{Local: says("x", nil)}).NeedsNetwork {
		t.Fatal("a local brain must not wait for the network")
	}
}

// ---- the whole path, with a real queue, a real worker and real stores -----------------

type storeBook struct{ s *sqlstore.Store }

func (b storeBook) Lookup(ctx context.Context, key string) (string, bool, error) {
	a, ok, err := b.s.Answer(ctx, key)
	return a.Text, ok, err
}
func (b storeBook) Record(ctx context.Context, a Answer) (string, error) {
	if _, err := b.s.RecordAnswer(ctx, a.Key, a.Question, a.Text, a.Model, a.Source); err != nil {
		return "", err
	}
	got, _, err := b.s.Answer(ctx, a.Key)
	return got.Text, err
}

func TestAQueuedQuestionIsAnsweredWhenIdleAndTheAnswerSyncsToAnotherDevice(t *testing.T) {
	ctx := context.Background()
	open := func(node string) *sqlstore.Store {
		s, err := sqlstore.Open(ctx, filepath.Join(t.TempDir(), node+".db"), sqlstore.Options{NodeID: node})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	phone, laptop := open("phone"), open("laptop")

	e := newEnv(t)
	j, _, err := e.q.Enqueue(ctx, "route:deep_think:abc", KindDeepThink, []byte(`{"text":"summarize this week"}`), jobqueue.EnqueueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var idle atomic.Bool
	var calls atomic.Int64
	e.start(Config{Idle: idle.Load, Specs: map[string]Spec{
		KindDeepThink: Think(ThinkConfig{Local: says("Three jobs finished, one is open.", &calls), LocalModel: "qwen2.5:7b", Book: storeBook{laptop}}),
	}})
	time.Sleep(150 * time.Millisecond)
	if calls.Load() != 0 || e.get(j.ID).Status != jobqueue.Queued {
		t.Fatal("the slow model started on a busy machine")
	}
	idle.Store(true)
	eventually(t, "the queued question is answered", 3*time.Second, allDone(e, j.ID))

	if r, _ := e.result(j.ID); r.Text != "Three jobs finished, one is open." {
		t.Fatalf("queue result %q", r.Text)
	}
	ans, err := laptop.Answers(ctx)
	if err != nil || len(ans) != 1 || ans[0].Key != "route:deep_think:abc" || ans[0].Source != "local" {
		t.Fatalf("laptop answers %+v %v", ans, err)
	}
	// Sync to the phone the way the protocol does.
	have, _ := phone.Summary(ctx)
	ops, _, err := laptop.OpsSince(ctx, have, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := phone.ApplyRemote(ctx, ops); err != nil {
		t.Fatal(err)
	}
	pa, _ := phone.Answers(ctx)
	if len(pa) != 1 || pa[0].Text != "Three jobs finished, one is open." {
		t.Fatalf("phone answers %+v", pa)
	}
	h1, _ := laptop.StateHash(ctx)
	h2, _ := phone.StateHash(ctx)
	if h1 != h2 {
		t.Fatal("the two devices disagree after the answer synced")
	}
	// The job is finished; running the same handler again asks no model.
	n := calls.Load()
	if got, err := Think(ThinkConfig{Local: says("different", &calls), LocalModel: "m", Book: storeBook{phone}}).Handle(ctx, mkjob("route:deep_think:abc", "x"), nil); err != nil || got != "Three jobs finished, one is open." || calls.Load() != n {
		t.Fatalf("phone re-answered: %q %v", got, err)
	}
}

func TestACrashAfterSavingTheAnswerDoesNotRunTheModelAgain(t *testing.T) {
	ctx := context.Background()
	s, err := sqlstore.Open(ctx, filepath.Join(t.TempDir(), "a.db"), sqlstore.Options{NodeID: "n"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var calls atomic.Int64
	spec := Think(ThinkConfig{Local: says("answer", &calls), LocalModel: "m", Book: storeBook{s}})
	// attempt 1 records the answer, then "the process dies" before the queue is told
	if _, err := spec.Handle(ctx, mkjob("k", "q"), nil); err != nil {
		t.Fatal(err)
	}
	// attempt 2 (after restart)
	if got, err := spec.Handle(ctx, mkjob("k", "q"), nil); err != nil || got != "answer" || calls.Load() != 1 {
		t.Fatalf("%q %v model calls %d", got, err, calls.Load())
	}
	if ops, _ := s.Ops(ctx, 0, 100); len(ops) != 1 {
		t.Fatalf("%d ops in the log, want exactly 1", len(ops))
	}
}

func TestThinkTreatsAnEmptyCloudAnswerAsAFailureAndAsksTheLocalModel(t *testing.T) {
	var local atomic.Int64
	spec := Think(ThinkConfig{Local: says("local words", &local), LocalModel: "l", Cloud: says("   ", nil), CloudModel: "c",
		Online: func() bool { return true }, Book: &memBook{}})
	got, err := run(t, spec, mkjob("k", "q"))
	if err != nil || got != "local words" || local.Load() != 1 {
		t.Fatalf("%q %v local calls %d", got, err, local.Load())
	}
}

func TestARemoteLocalBrainWaitsForTheNetworkLikeTheDay12DeepHost(t *testing.T) {
	spec := Think(ThinkConfig{Local: says("x", nil), LocalModel: "m", LocalNeedsNetwork: true})
	if !spec.NeedsNetwork {
		t.Fatal("a model on another machine must wait for the network")
	}
}
