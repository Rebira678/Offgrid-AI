package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"offgrid/internal/agent"
	"offgrid/internal/eval"
	"offgrid/internal/intent"
	"offgrid/internal/llm"
	"offgrid/internal/structured"
	"offgrid/internal/task"
	"offgrid/internal/tools"
)

var thursday = time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)

// fakeModel stands in for Tier 1. It records every call.
type fakeModel struct {
	mu    sync.Mutex
	calls []string
	reply func(text string) (intent.Command, int, error) // command, attempts used, error
}

func (m *fakeModel) Classify(ctx context.Context, text string) (structured.Result[intent.Command], error) {
	m.mu.Lock()
	m.calls = append(m.calls, text)
	m.mu.Unlock()
	cmd, attempts, err := m.reply(text)
	res := structured.Result[intent.Command]{Value: cmd, Attempts: make([]structured.Attempt, attempts)}
	return res, err
}

func (m *fakeModel) called() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// ticker is a clock that moves forward a fixed step every time it is read, so
// latencies are exact and tests never sleep.
type ticker struct {
	mu   sync.Mutex
	t    time.Time
	step time.Duration
}

func (c *ticker) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(c.step)
	return c.t
}

type rig struct {
	agent *agent.Agent
	model *fakeModel
	store task.Store
}

func newRig(t *testing.T, model *fakeModel) *rig {
	t.Helper()
	store := task.NewMemStore(func() time.Time { return thursday })
	reg := tools.NewRegistry(store, func() time.Time { return thursday })
	clk := &ticker{t: thursday, step: time.Millisecond}
	a := &agent.Agent{Rules: agent.DefaultRules(), Registry: reg, Now: clk.now}
	if model != nil {
		a.Model = model
	}
	return &rig{agent: a, model: model, store: store}
}

func (r *rig) tasks(t *testing.T) []task.Task {
	t.Helper()
	got, err := r.store.List(context.Background(), task.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func neverCalled(t *testing.T) *fakeModel {
	return &fakeModel{reply: func(text string) (intent.Command, int, error) {
		t.Errorf("the model was called for %q, but the rules should have handled it", text)
		return intent.Command{}, 1, errors.New("should not be called")
	}}
}

func TestRulesHandleASimpleRequestWithoutTheModel(t *testing.T) {
	m := neverCalled(t)
	r := newRig(t, m)
	o := r.agent.Handle(context.Background(), "new task: replace filter, high priority")
	if o.Tier != agent.TierRules || o.Rule != "create-task" || o.Status != agent.StatusOK {
		t.Fatalf("outcome = %+v", o)
	}
	if m.called() != 0 {
		t.Fatalf("the model was called %d times", m.called())
	}
	if ts := r.tasks(t); len(ts) != 1 || ts[0].Title != "replace filter" || ts[0].Priority != task.PriorityHigh {
		t.Fatalf("tasks = %+v", ts)
	}
}

func TestMessagesTheRulesCannotReadGoToTheModel(t *testing.T) {
	m := &fakeModel{reply: func(text string) (intent.Command, int, error) {
		return intent.Command{Intent: intent.CreateTask, Title: "Leaking pump", Site: "4", Priority: "high"}, 2, nil
	}}
	r := newRig(t, m)
	o := r.agent.Handle(context.Background(), "log a leaking pump at site 4, high priority")
	if o.Tier != agent.TierModel || o.Status != agent.StatusOK || o.Attempts != 2 || o.Rule != "" {
		t.Fatalf("outcome = %+v", o)
	}
	if m.called() != 1 || len(r.tasks(t)) != 1 {
		t.Fatalf("model calls %d, tasks %d", m.called(), len(r.tasks(t)))
	}
}

func TestLatencyIsMeasuredPerStep(t *testing.T) {
	// The fake clock advances 1 ms per reading, so the numbers are exact.
	r := newRig(t, neverCalled(t))
	o := r.agent.Handle(context.Background(), "list open tasks")
	if o.Understand <= 0 || o.Execute <= 0 || o.Total < o.Understand+o.Execute {
		t.Errorf("understand %v, execute %v, total %v", o.Understand, o.Execute, o.Total)
	}
	// Five readings: start, understood, execute begins, execute ends, finish.
	if o.Understand != time.Millisecond || o.Execute != time.Millisecond || o.Total != 4*time.Millisecond {
		t.Errorf("understand %v, execute %v, total %v; want exactly 1ms, 1ms, 4ms", o.Understand, o.Execute, o.Total)
	}
}

func TestLatencyOnTheModelPathIsMeasuredPerStepToo(t *testing.T) {
	m := &fakeModel{reply: func(string) (intent.Command, int, error) {
		return intent.Command{Intent: intent.ListTasks}, 1, nil
	}}
	o := newRig(t, m).agent.Handle(context.Background(), "what is on my plate")
	if o.Tier != agent.TierModel {
		t.Fatalf("outcome = %+v", o)
	}
	if o.Understand != time.Millisecond || o.Execute != time.Millisecond || o.Total != 4*time.Millisecond {
		t.Errorf("understand %v, execute %v, total %v; want exactly 1ms, 1ms, 4ms", o.Understand, o.Execute, o.Total)
	}
}

func TestTheRulesStillWorkWhenTheModelIsDown(t *testing.T) {
	down := &fakeModel{reply: func(string) (intent.Command, int, error) {
		return intent.Command{}, 0, llm.ErrUnavailable
	}}
	r := newRig(t, down)
	ok := r.agent.Handle(context.Background(), "new task: check the fuel line")
	if ok.Status != agent.StatusOK || ok.Tier != agent.TierRules {
		t.Fatalf("a rule-readable message must not depend on the model: %+v", ok)
	}
	bad := r.agent.Handle(context.Background(), "I finished replacing the gasket")
	if bad.Status != agent.StatusFailed || !errors.Is(bad.Err, llm.ErrUnavailable) || bad.Tier != agent.TierNone {
		t.Fatalf("outcome = %+v", bad)
	}
	if !strings.Contains(bad.Message(), "not reachable") || !strings.Contains(bad.Message(), "Ollama") {
		t.Errorf("message = %q", bad.Message())
	}
}

func TestWithNoModelConfiguredOnlyRulesWork(t *testing.T) {
	r := newRig(t, nil)
	o := r.agent.Handle(context.Background(), "I finished replacing the gasket")
	if o.Status != agent.StatusFailed || !errors.Is(o.Err, agent.ErrNoModel) {
		t.Fatalf("outcome = %+v", o)
	}
	if !strings.Contains(o.Message(), "plainer phrase") {
		t.Errorf("message should say what to do: %q", o.Message())
	}
	if ok := r.agent.Handle(context.Background(), "list open tasks"); ok.Status != agent.StatusOK {
		t.Errorf("rules must work without a model: %+v", ok)
	}
}

func TestAModelThatNeverProducesAValidCommandIsNotUnderstood(t *testing.T) {
	m := &fakeModel{reply: func(string) (intent.Command, int, error) {
		return intent.Command{}, 3, structured.ErrNoValidReply
	}}
	o := newRig(t, m).agent.Handle(context.Background(), "gibberish blah")
	if o.Status != agent.StatusNotUnderstood || o.Attempts != 3 || o.Tier != agent.TierNone {
		t.Fatalf("outcome = %+v", o)
	}
}

func TestTheUnknownIntentIsDeclinedNotFailed(t *testing.T) {
	m := &fakeModel{reply: func(string) (intent.Command, int, error) {
		return intent.Command{Intent: intent.Unknown}, 1, nil
	}}
	r := newRig(t, m)
	o := r.agent.Handle(context.Background(), "tell me a joke")
	if o.Status != agent.StatusDeclined || o.Tier != agent.TierModel || !errors.Is(o.Err, tools.ErrNoTool) {
		t.Fatalf("outcome = %+v", o)
	}
	if len(r.tasks(t)) != 0 {
		t.Error("a declined request changed something")
	}
}

func TestRegistryRefusesWhatEitherTierProposes(t *testing.T) {
	m := &fakeModel{reply: func(string) (intent.Command, int, error) {
		return intent.Command{Intent: intent.CreateTask, Title: "x", Priority: "urgent"}, 1, nil
	}}
	r := newRig(t, m)
	o := r.agent.Handle(context.Background(), "make something urgent happen")
	var argErr *tools.ArgumentError
	if o.Status != agent.StatusRefused || !errors.As(o.Err, &argErr) {
		t.Fatalf("outcome = %+v", o)
	}
	if len(r.tasks(t)) != 0 {
		t.Error("a refused command changed something")
	}
}

func TestAnUnknownTaskFromARuleIsRefusedWithoutAskingTheModel(t *testing.T) {
	m := neverCalled(t)
	o := newRig(t, m).agent.Handle(context.Background(), "close the boiler ticket")
	if o.Tier != agent.TierRules || o.Status != agent.StatusRefused || !errors.Is(o.Err, task.ErrNotFound) {
		t.Fatalf("outcome = %+v", o)
	}
	if m.called() != 0 {
		t.Error("the model cannot know which tasks exist; asking it would only cost time")
	}
	if !strings.Contains(o.Message(), "Nothing was changed") {
		t.Errorf("message = %q", o.Message())
	}
}

func TestARuleThatProposesAnInvalidCommandFallsBackToTheModel(t *testing.T) {
	buggy := agent.Rules{{Name: "buggy", Match: func(string) (intent.Command, bool) {
		return intent.Command{Intent: intent.CreateTask}, true // no title: the registry refuses
	}}}
	m := &fakeModel{reply: func(string) (intent.Command, int, error) {
		return intent.Command{Intent: intent.CreateTask, Title: "from the model"}, 1, nil
	}}
	r := newRig(t, m)
	r.agent.Rules = buggy
	o := r.agent.Handle(context.Background(), "anything")
	if o.Tier != agent.TierModel || o.Status != agent.StatusOK {
		t.Fatalf("outcome = %+v", o)
	}
	if !strings.Contains(o.Fallback, `rule "buggy"`) {
		t.Errorf("the fallback must be recorded so the rule can be fixed: %q", o.Fallback)
	}
}

func TestEmptyMessageNeverReachesTheModel(t *testing.T) {
	m := neverCalled(t)
	r := newRig(t, m)
	for _, text := range []string{"", "   ", "\n\t "} {
		if o := r.agent.Handle(context.Background(), text); o.Status != agent.StatusNotUnderstood {
			t.Errorf("Handle(%q) = %+v", text, o)
		}
	}
}

func TestACancelledContextIsAFailureNotAPanic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m := &fakeModel{reply: func(string) (intent.Command, int, error) { return intent.Command{}, 0, context.Canceled }}
	r := newRig(t, m)
	for _, text := range []string{"list open tasks", "something only the model reads"} {
		o := r.agent.Handle(ctx, text)
		if o.Status != agent.StatusFailed || !errors.Is(o.Err, context.Canceled) {
			t.Errorf("Handle(%q) = %+v", text, o)
		}
	}
}

func TestStatsGroupByTierAndReportMedians(t *testing.T) {
	outs := []agent.Outcome{
		{Tier: agent.TierRules, Status: agent.StatusOK, Total: 10 * time.Microsecond},
		{Tier: agent.TierRules, Status: agent.StatusRefused, Total: 30 * time.Microsecond},
		{Tier: agent.TierRules, Status: agent.StatusOK, Total: 20 * time.Microsecond},
		{Tier: agent.TierModel, Status: agent.StatusOK, Total: 2 * time.Second},
		{Tier: agent.TierModel, Status: agent.StatusOK, Total: 4 * time.Second},
		{Tier: agent.TierNone, Status: agent.StatusFailed, Total: time.Millisecond},
	}
	stats := agent.Stats(outs)
	if len(stats) != 3 || stats[0].Tier != agent.TierRules || stats[1].Tier != agent.TierModel || stats[2].Tier != agent.TierNone {
		t.Fatalf("stats = %+v", stats)
	}
	if stats[0].Count != 3 || stats[0].Median != 20*time.Microsecond || stats[0].Max != 30*time.Microsecond ||
		stats[0].Statuses[agent.StatusRefused] != 1 {
		t.Errorf("rules = %+v", stats[0])
	}
	if stats[1].Median != 3*time.Second { // the median of an even count is the mean of the middle two
		t.Errorf("model median = %v, want 3s", stats[1].Median)
	}
	var b bytes.Buffer
	agent.WriteStats(&b, outs)
	for _, want := range []string{"tier 0 (rules)", "tier 1 (model)", "20 µs", "3.00 s", "2 ok, 1 refused"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("table lacks %q:\n%s", want, b.String())
		}
	}
	if len(agent.Stats(nil)) != 0 {
		t.Error("no outcomes, no rows")
	}
}

func TestFormatDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                       "0 µs",
		42 * time.Microsecond:   "42 µs",
		1500 * time.Microsecond: "1.5 ms",
		2410 * time.Millisecond: "2.41 s",
	} {
		if got := agent.FormatDuration(d); got != want {
			t.Errorf("FormatDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestLogEntryIsOneValidJSONLine(t *testing.T) {
	o := agent.Outcome{
		Text: `say "hi"`, Tier: agent.TierModel, Status: agent.StatusOK, Attempts: 2,
		Command:    intent.Command{Intent: intent.ListTasks},
		Understand: 2 * time.Second, Execute: 150 * time.Microsecond, Total: 2*time.Second + 150*time.Microsecond,
	}
	var b bytes.Buffer
	if err := agent.WriteLog(&b, thursday, o); err != nil {
		t.Fatal(err)
	}
	if strings.Count(b.String(), "\n") != 1 {
		t.Fatalf("want exactly one line: %q", b.String())
	}
	var e agent.LogEntry
	if err := json.Unmarshal(b.Bytes(), &e); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if e.Tier != 1 || e.Intent != "list_tasks" || e.Status != "ok" || e.Attempts != 2 || e.Text != `say "hi"` ||
		e.UnderstandMS != 2000 || e.TotalMS < 2000.1 || e.Error != "" {
		t.Errorf("entry = %+v", e)
	}
	o = agent.Outcome{Tier: agent.TierNone, Status: agent.StatusFailed, Err: errors.New("boom")}
	b.Reset()
	agent.WriteLog(&b, thursday, o)
	json.Unmarshal(b.Bytes(), &e)
	if e.Tier != -1 || e.Error != "boom" {
		t.Errorf("entry = %+v", e)
	}
}

// labelModel plays a perfect model: it answers every labelled message with its
// label, so the test exercises the loop and not any real model.
func labelModel(cases []eval.Case) *fakeModel {
	byText := map[string]eval.Case{}
	for _, c := range cases {
		byText[c.Text] = c
	}
	return &fakeModel{reply: func(text string) (intent.Command, int, error) {
		c, ok := byText[text]
		if !ok {
			return intent.Command{}, 1, structured.ErrNoValidReply
		}
		cmd := intent.Command{Intent: c.Intent, Site: c.Site, Priority: c.Priority, Status: c.Status, When: c.When}
		switch c.Intent {
		case intent.CreateTask:
			cmd.Title = "from the model"
		case intent.UpdateTask:
			cmd.TaskRef = "T1"
			if cmd.Status == "" && cmd.Priority == "" {
				cmd.Status = "done"
			}
		case intent.AddNote:
			cmd.TaskRef, cmd.Note = "T1", "a note"
		}
		return cmd, 1, nil
	}}
}

func TestEvaluationCasesRunEndToEndAndAreCountedByTier(t *testing.T) {
	cases, err := eval.LoadCases()
	if err != nil {
		t.Fatal(err)
	}
	m := labelModel(cases)
	r := newRig(t, m)
	if _, err := r.agent.Registry.Execute(context.Background(), intent.Command{Intent: intent.CreateTask, Title: "Leaking pump"}); err != nil {
		t.Fatal(err)
	}
	var logged int
	results := agent.RunCases(context.Background(), r.agent, cases, func(agent.Outcome) { logged++ })
	if len(results) != len(cases) || logged != len(cases) {
		t.Fatalf("%d results, %d logged, %d cases", len(results), logged, len(cases))
	}
	s := agent.CountCases(results)
	if s.Rules+s.Model+s.Unhandled != s.Total || s.Total != 30 {
		t.Errorf("stats do not add up: %+v", s)
	}
	if s.Rules == 0 || s.Model == 0 {
		t.Errorf("both tiers should have handled something: %+v", s)
	}
	if s.ModelCorrect != s.Model {
		t.Errorf("the perfect model was marked wrong %d times", s.Model-s.ModelCorrect)
	}
	if m.called() != s.Model {
		t.Errorf("the model was called %d times but handled %d: the rules must not call it", m.called(), s.Model)
	}
	// The headline requirement: at least 20 of the 30 messages run end to end.
	if s.Completed < 20 {
		t.Errorf("only %d of 30 ran to a final outcome", s.Completed)
	}
	var b bytes.Buffer
	agent.WriteCases(&b, results)
	for _, want := range []string{"TIER", "LATENCY", "0 rules", "1 model", "Tier 0 (rules) handled", "ran to a final outcome"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("report lacks %q", want)
		}
	}
}

// A rule must never be more confident than it deserves. Every message the rules
// accept in the evaluation set must produce exactly the labelled command: this
// is the test that catches a rule that misreads.
func TestEveryRuleAnswerOnTheEvaluationSetMatchesItsLabel(t *testing.T) {
	cases, _ := eval.LoadCases()
	rules := agent.DefaultRules()
	handled := 0
	for _, c := range cases {
		cmd, rule, ok := rules.Match(c.Text)
		if !ok {
			continue
		}
		handled++
		if !c.Correct(cmd) {
			t.Errorf("%s %q: rule %q proposed %+v, label says %+v", c.ID, c.Text, rule, cmd, c)
		}
	}
	if handled < 10 {
		t.Errorf("rules handled only %d of %d cases; something broke", handled, len(cases))
	}
}

// The loop's own decisions must not depend on a network library or a concrete
// model client: tiers talk to a Model interface.
func TestAgentImportsNoNetworkOrConcreteModelClient(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			switch p := strings.Trim(imp.Path.Value, `"`); p {
			case "net/http", "offgrid/internal/ollama":
				t.Errorf("%s imports %s", f, p)
			}
		}
	}
}
