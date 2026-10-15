package router_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"offgrid/internal/agent"
	"offgrid/internal/connectivity"
	"offgrid/internal/intent"
	"offgrid/internal/jobqueue"
	"offgrid/internal/llm"
	"offgrid/internal/router"
	"offgrid/internal/store/sqlstore"
	"offgrid/internal/structured"
	"offgrid/internal/task"
	"offgrid/internal/tools"
)

var (
	ctx  = context.Background()
	noon = time.Date(2026, 10, 14, 12, 0, 0, 0, time.Local)
)

type fakeModel struct {
	mu    sync.Mutex
	calls []string
	reply func(text string) (intent.Command, error)
}

func (m *fakeModel) Classify(_ context.Context, text string) (structured.Result[intent.Command], error) {
	m.mu.Lock()
	m.calls = append(m.calls, text)
	m.mu.Unlock()
	cmd, err := m.reply(text)
	return structured.Result[intent.Command]{Value: cmd, Attempts: make([]structured.Attempt, 1)}, err
}

func (m *fakeModel) called() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

type fakeCloud struct {
	calls int
	ans   string
	err   error
}

func (c *fakeCloud) Answer(_ context.Context, text string) (string, error) {
	c.calls++
	return c.ans, c.err
}

type rig struct {
	r     *router.Router
	model *fakeModel
	queue *jobqueue.Queue
	net   *connectivity.State
	store task.Store
}

// newRig builds a router over a real queue in a temp file. The model is a
// fake that lists open tasks for anything it is given, unless reply is set.
func newRig(t *testing.T, mut func(*rig)) *rig {
	t.Helper()
	db, err := sqlstore.OpenDB(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	q, err := jobqueue.New(ctx, db, jobqueue.Config{Now: func() time.Time { return noon }})
	if err != nil {
		t.Fatal(err)
	}
	store := task.NewMemStore(func() time.Time { return noon })
	reg := tools.NewRegistry(store, func() time.Time { return noon })
	model := &fakeModel{reply: func(string) (intent.Command, error) {
		return intent.Command{Intent: intent.ListTasks}, nil
	}}
	state := connectivity.Offline
	g := &rig{model: model, queue: q, net: &state, store: store}
	if mut != nil {
		mut(g)
	}
	a := &agent.Agent{Rules: agent.DefaultRules(), Registry: reg, Model: g.model}
	if g.model == nil {
		a.Model = nil // a nil *fakeModel would be a non-nil interface
	}
	g.r = &router.Router{Agent: a, Queue: q, Net: func() connectivity.State { return *g.net }, Now: func() time.Time { return noon }}
	return g
}

// ---- the Day 10 "done when" ---------------------------------------------------

func TestOfflineSimpleAnswersNowAndHardIsQueuedWithAVisibleStatus(t *testing.T) {
	g := newRig(t, nil)
	g.store.Create(ctx, task.NewTask{Title: "Check the fuel line", Priority: "high"})

	// simple, rule-handled: answered at once, offline or not
	r := g.r.Handle(ctx, "what's still open?")
	if r.Route != router.RouteLocal || r.Outcome.Tier != agent.TierRules || !strings.Contains(r.Message, "fuel line") {
		t.Fatalf("simple request: %+v", r)
	}
	if g.model.called() != 0 {
		t.Error("a rule answer must not call the model")
	}

	// hard: queued, with a status a person can read
	r = g.r.Handle(ctx, "summarize this week's incidents and suggest priorities")
	if r.Route != router.RouteDeferred || r.Job == nil || !r.Created {
		t.Fatalf("hard request: %+v", r)
	}
	for _, want := range []string{"Saved for later as job #1", "deeper thinking", "offline", "suggest"} {
		if !strings.Contains(r.Message, want) {
			t.Errorf("status message missing %q:\n%s", want, r.Message)
		}
	}
	if g.model.called() != 0 {
		t.Error("a hard request must not waste seconds on the small model")
	}

	job, err := g.queue.Get(ctx, r.Job.ID)
	if err != nil || job.Kind != router.KindDeepThink || job.Status != jobqueue.Queued || !strings.Contains(string(job.Payload), "incidents") {
		t.Fatalf("stored job: %+v %v", job, err)
	}

	// and the queue can be asked about, instantly
	q := g.r.Handle(ctx, "what's queued?")
	if q.Route != router.RouteStatus || !strings.Contains(q.Message, "1 waiting") || !strings.Contains(q.Message, "incidents") {
		t.Fatalf("queue question: %+v", q)
	}
}

func TestRulesComeBeforeTheGate(t *testing.T) {
	// The title contains a judgement word, but a rule fully understands the
	// sentence: it must create a task, not be queued.
	g := newRig(t, nil)
	for _, text := range []string{
		"new task: suggest a new filter supplier, site 3",
		"new task: why does the pump leak, site 1",
		"note on T-1: recommend replacing the seal",
	} {
		if text[0] == 'n' && strings.HasPrefix(text, "note") {
			g.store.Create(ctx, task.NewTask{Title: "Pump"})
		}
		r := g.r.Handle(ctx, text)
		if r.Route != router.RouteLocal {
			t.Errorf("%q -> %s (%s), want local", text, r.Route, r.Message)
		}
	}
	if st, _ := g.queue.Stats(ctx); st.Total() != 0 {
		t.Errorf("nothing should have been queued, got %d", st.Total())
	}
}

func TestConnectivityOnlyChangesTheStatusWording(t *testing.T) {
	g := newRig(t, nil)
	cases := map[connectivity.State]string{
		connectivity.Offline:  "offline",
		connectivity.Degraded: "slow",
		connectivity.Online:   "online",
		connectivity.Unknown:  "not known",
	}
	i := 0
	for state, want := range cases {
		*g.net = state
		i++
		r := g.r.Handle(ctx, fmt.Sprintf("what should I prioritize this week, take %d", i))
		if r.Route != router.RouteDeferred || !strings.Contains(r.Message, want) || r.Net != state {
			t.Errorf("state %v: route %s, message %q, want it to say %q", state, r.Route, r.Message, want)
		}
	}
}

// ---- the cloud hook -----------------------------------------------------------

func TestOnlineWithACloudModelHardRequestsAreAnsweredNow(t *testing.T) {
	cloud := &fakeCloud{ans: "Fix the pump first."}
	g := newRig(t, nil)
	g.r.Cloud = cloud
	*g.net = connectivity.Online
	r := g.r.Handle(ctx, "what should I prioritize this week?")
	if r.Route != router.RouteCloud || r.Answer != "Fix the pump first." || cloud.calls != 1 {
		t.Fatalf("%+v", r)
	}
	if st, _ := g.queue.Stats(ctx); st.Total() != 0 {
		t.Error("an answered request must not also be queued")
	}
}

func TestCloudIsNeverTriedWhenNotOnline(t *testing.T) {
	for _, state := range []connectivity.State{connectivity.Offline, connectivity.Degraded, connectivity.Unknown} {
		cloud := &fakeCloud{ans: "x"}
		g := newRig(t, nil)
		g.r.Cloud = cloud
		*g.net = state
		r := g.r.Handle(ctx, "what should I prioritize this week?")
		if r.Route != router.RouteDeferred || cloud.calls != 0 {
			t.Errorf("state %v: route %s, cloud calls %d", state, r.Route, cloud.calls)
		}
	}
}

func TestCloudFailureFallsBackToTheQueue(t *testing.T) {
	cloud := &fakeCloud{err: errors.New("503 from the provider")}
	g := newRig(t, nil)
	g.r.Cloud = cloud
	*g.net = connectivity.Online
	r := g.r.Handle(ctx, "what should I prioritize this week?")
	if r.Route != router.RouteDeferred || !strings.Contains(r.Reason, "503") || cloud.calls != 1 {
		t.Fatalf("%+v", r)
	}
}

func TestNoCloudMeansOnlineStillQueues(t *testing.T) {
	g := newRig(t, nil)
	*g.net = connectivity.Online
	r := g.r.Handle(ctx, "what should I prioritize this week?")
	if r.Route != router.RouteDeferred || !strings.Contains(r.Message, "background worker") {
		t.Fatalf("%+v", r)
	}
}

// ---- output validity ----------------------------------------------------------

func TestUnreadableByTheLocalModelIsDeferredNotFailed(t *testing.T) {
	g := newRig(t, func(g *rig) {
		g.model.reply = func(string) (intent.Command, error) { return intent.Command{}, structured.ErrNoValidReply }
	})
	r := g.r.Handle(ctx, "the old compressor keeps tripping the breaker")
	if r.Route != router.RouteDeferred || r.Job == nil {
		t.Fatalf("%+v", r)
	}
	job, _ := g.queue.Get(ctx, r.Job.ID)
	if job.Kind != router.KindInterpret || !strings.Contains(r.Reason, "valid command") {
		t.Errorf("kind %s, reason %q", job.Kind, r.Reason)
	}
	if g.model.called() != 1 {
		t.Errorf("the model should have been tried once, got %d", g.model.called())
	}
}

func TestNoiseIsNotQueued(t *testing.T) {
	g := newRig(t, func(g *rig) {
		g.model.reply = func(string) (intent.Command, error) { return intent.Command{}, structured.ErrNoValidReply }
	})
	r := g.r.Handle(ctx, "asdf")
	if r.Route != router.RouteLocal || r.Outcome.Status != agent.StatusNotUnderstood {
		t.Fatalf("%+v", r)
	}
	if st, _ := g.queue.Stats(ctx); st.Total() != 0 {
		t.Error("two letters of noise must not fill the queue")
	}
}

func TestModelDownOrMissingDefersInsteadOfFailing(t *testing.T) {
	down := func(g *rig) {
		g.model.reply = func(string) (intent.Command, error) { return intent.Command{}, llm.ErrUnavailable }
	}
	for name, mut := range map[string]func(*rig){
		"ollama not running": down,
		"no model at all":    func(g *rig) { g.model = nil },
		"model too slow": func(g *rig) {
			g.model.reply = func(string) (intent.Command, error) { return intent.Command{}, context.DeadlineExceeded }
		},
	} {
		g := newRig(t, mut)
		r := g.r.Handle(ctx, "I finished replacing the gasket on the north pump")
		if r.Route != router.RouteDeferred || r.Job == nil {
			t.Errorf("%s: %+v", name, r)
			continue
		}
		if !strings.Contains(r.Message, "Saved for later") {
			t.Errorf("%s: message %q", name, r.Message)
		}
	}
}

func TestCancelledByTheUserIsNotDeferred(t *testing.T) {
	g := newRig(t, func(g *rig) {
		g.model.reply = func(string) (intent.Command, error) { return intent.Command{}, context.DeadlineExceeded }
	})
	c, cancel := context.WithCancel(ctx)
	cancel()
	r := g.r.Handle(c, "I finished replacing the gasket on the north pump")
	if r.Route != router.RouteLocal {
		t.Errorf("a cancelled request must be reported as it is, not queued: route %s", r.Route)
	}
}

func TestOtherFailuresAreNotDeferred(t *testing.T) {
	g := newRig(t, func(g *rig) {
		g.model.reply = func(string) (intent.Command, error) { return intent.Command{}, errors.New("disk on fire") }
	})
	r := g.r.Handle(ctx, "I finished replacing the gasket on the north pump")
	if r.Route != router.RouteLocal || r.Outcome.Status != agent.StatusFailed {
		t.Fatalf("an unknown error must be shown, not hidden in the queue: %+v", r)
	}
}

func TestValidSimpleModelAnswerStaysLocal(t *testing.T) {
	g := newRig(t, nil)
	r := g.r.Handle(ctx, "show tasks not done")
	if r.Route != router.RouteLocal || r.Outcome.Tier != agent.TierModel || g.model.called() != 1 {
		t.Fatalf("%+v", r)
	}
}

// ---- idempotency, queue questions, failures ---------------------------------

func TestSameRequestTwiceIsOneJob(t *testing.T) {
	g := newRig(t, nil)
	a := g.r.Handle(ctx, "what should I prioritize this week?")
	b := g.r.Handle(ctx, "what should I prioritize this week?")
	c := g.r.Handle(ctx, "  what  should I   prioritize this week? ") // spacing is normalised
	if !a.Created || b.Created || c.Created || a.Job.ID != b.Job.ID || a.Job.ID != c.Job.ID {
		t.Fatalf("created %v %v %v, ids %d %d %d", a.Created, b.Created, c.Created, a.Job.ID, b.Job.ID, c.Job.ID)
	}
	if !strings.Contains(b.Message, "Already saved as job #1") || !strings.Contains(b.Message, "Not added again") {
		t.Errorf("repeat message: %s", b.Message)
	}
	if st, _ := g.queue.Stats(ctx); st.Total() != 1 {
		t.Errorf("total %d, want 1", st.Total())
	}
}

func TestSameRequestTomorrowIsANewJob(t *testing.T) {
	g := newRig(t, nil)
	day := noon
	g.r.Now = func() time.Time { return day }
	a := g.r.Handle(ctx, "what should I prioritize this week?")
	day = day.AddDate(0, 0, 1)
	b := g.r.Handle(ctx, "what should I prioritize this week?")
	if !a.Created || !b.Created || a.Job.ID == b.Job.ID {
		t.Errorf("tomorrow's identical question should be its own job: %v %v", a.Created, b.Created)
	}
}

func TestCaseVariantsDoNotCollide(t *testing.T) {
	// Same key would mean a payload conflict; the key is built from the exact
	// text, so a different spelling is simply another job, never an error.
	g := newRig(t, nil)
	a := g.r.Handle(ctx, "what should I prioritize this week?")
	b := g.r.Handle(ctx, "What should I prioritize this week?")
	if a.Route != router.RouteDeferred || b.Route != router.RouteDeferred || a.Err != nil || b.Err != nil {
		t.Fatalf("%+v %+v", a, b)
	}
}

func TestQueueQuestionVariantsAndEmptyQueue(t *testing.T) {
	g := newRig(t, nil)
	for _, q := range []string{"queue", "Queue status", "what's queued?", "whats queued", "what is in the queue", "pending jobs", "show the queue"} {
		r := g.r.Handle(ctx, q)
		if r.Route != router.RouteStatus || r.Message != "Nothing is waiting." {
			t.Errorf("%q -> %s %q", q, r.Route, r.Message)
		}
	}
}

func TestQueueQuestionsDoNotStealTaskQuestions(t *testing.T) {
	// "pending" and "waiting" are task words; they belong to the task rules.
	g := newRig(t, nil)
	for _, q := range []string{"what's pending", "what is waiting", "pending", "show pending tasks"} {
		if r := g.r.Handle(ctx, q); r.Route == router.RouteStatus {
			t.Errorf("%q was taken as a queue question", q)
		}
	}
}

func TestQueueSummaryListsWaitingRunningAndDead(t *testing.T) {
	g := newRig(t, nil)
	g.r.Handle(ctx, "what should I prioritize this week?")
	g.r.Handle(ctx, "why does the fuel line keep cracking")
	j, ok, _ := g.queue.Claim(ctx, time.Minute)
	if !ok {
		t.Fatal("no job")
	}
	g.queue.Fail(ctx, j.ID, j.Attempts, errors.New("boom"), true) // dead
	m := g.r.Handle(ctx, "queue").Message
	for _, want := range []string{"1 waiting", "1 failed for good", "dead", "queued", "fuel line"} {
		if !strings.Contains(m, want) {
			t.Errorf("summary missing %q:\n%s", want, m)
		}
	}
}

func TestQueueFailureIsReportedNotHidden(t *testing.T) {
	g := newRig(t, nil)
	g.r.Queue = failingQueue{g.queue}
	r := g.r.Handle(ctx, "what should I prioritize this week?")
	if r.Route != router.RouteFailed || r.Err == nil || !strings.Contains(r.Message, "could not save it for later") {
		t.Fatalf("%+v", r)
	}
}

// failingQueue refuses writes but reads normally.
type failingQueue struct{ *jobqueue.Queue }

func (failingQueue) Enqueue(context.Context, string, string, json.RawMessage, jobqueue.EnqueueOptions) (jobqueue.Job, bool, error) {
	return jobqueue.Job{}, false, errors.New("disk full")
}

func TestBlankMessageIsNotQueued(t *testing.T) {
	g := newRig(t, nil)
	r := g.r.Handle(ctx, "   ")
	if r.Route == router.RouteDeferred || g.model.called() != 0 {
		t.Errorf("%+v", r)
	}
}

func TestTimingIsReported(t *testing.T) {
	g := newRig(t, nil)
	clk := noon
	g.r.Now = func() time.Time { clk = clk.Add(2 * time.Millisecond); return clk }
	if r := g.r.Handle(ctx, "what should I prioritize this week?"); r.Total <= 0 {
		t.Errorf("Total = %v", r.Total)
	}
}

// ---- layering -------------------------------------------------------------------

func TestWholeLoopWithOfflineStateFromTheRealMonitor(t *testing.T) {
	// The real connectivity.Monitor against a prober that always fails drives
	// the router's Net function: nothing faked between the monitor and router.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	mon, err := connectivity.New(connectivity.Config{
		Prober: connectivity.ProberFunc(func(context.Context) connectivity.Result {
			return connectivity.Result{Err: errors.New("no route to host")}
		}),
		Interval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	go mon.Run(ctx)
	deadline := time.After(5 * time.Second)
	for mon.State() != connectivity.Offline {
		select {
		case <-deadline:
			t.Fatal("monitor never reported OFFLINE")
		case <-time.After(time.Millisecond):
		}
	}
	g := newRig(t, nil)
	g.r.Net = mon.State
	r := g.r.Handle(ctx, "summarize this week's incidents and suggest priorities")
	if r.Route != router.RouteDeferred || r.Net != connectivity.Offline || !strings.Contains(r.Message, "offline") {
		t.Fatalf("%+v", r)
	}
}
