package drain

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"offgrid/internal/agent"
	"offgrid/internal/intent"
	"offgrid/internal/jobqueue"
	"offgrid/internal/llm"
	"offgrid/internal/structured"
	"offgrid/internal/task"
	"offgrid/internal/tools"
)

var thursday = time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)

type fakeModel struct {
	mu    sync.Mutex
	calls int
	reply func(text string) (intent.Command, error)
}

func (m *fakeModel) Classify(ctx context.Context, text string) (structured.Result[intent.Command], error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	cmd, err := m.reply(text)
	return structured.Result[intent.Command]{Value: cmd}, err
}

func (m *fakeModel) called() int { m.mu.Lock(); defer m.mu.Unlock(); return m.calls }

type progressLog struct {
	saved   []string
	failing bool
}

func (p *progressLog) Save(ctx context.Context, data string) error {
	if p.failing {
		return errors.New("disk full")
	}
	p.saved = append(p.saved, data)
	return nil
}

type rig struct {
	agent *agent.Agent
	store task.Store
	model *fakeModel
}

func newRig(m *fakeModel) *rig {
	store := task.NewMemStore(func() time.Time { return thursday })
	reg := tools.NewRegistry(store, func() time.Time { return thursday })
	a := &agent.Agent{Rules: agent.DefaultRules(), Registry: reg}
	if m != nil {
		a.Model = m
	}
	return &rig{agent: a, store: store, model: m}
}

func (r *rig) tasks(t *testing.T) []task.Task {
	t.Helper()
	got, err := r.store.List(context.Background(), task.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func job(text, progress string) jobqueue.Job {
	b, _ := json.Marshal(map[string]string{"text": text})
	return jobqueue.Job{ID: 7, Kind: KindInterpret, Payload: b, Progress: progress}
}

func creates(title string) *fakeModel {
	return &fakeModel{reply: func(string) (intent.Command, error) {
		return intent.Command{Intent: intent.CreateTask, Title: title, Site: "2"}, nil
	}}
}

// ---- Interpret ---------------------------------------------------------------------------

func TestInterpretRunsTheCommandAndSavesTheMarkerFirst(t *testing.T) {
	r := newRig(creates("Check the fuel line"))
	p := &progressLog{}
	out, err := Interpret(r.agent).Handle(bg, job("please make a note to check the fuel line", ""), p)
	if err != nil || !strings.Contains(out, "Check the fuel line") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if len(p.saved) != 1 || p.saved[0] != markExecuting {
		t.Errorf("checkpoints = %q, want the executing marker before the change", p.saved)
	}
	if n := len(r.tasks(t)); n != 1 {
		t.Errorf("%d tasks", n)
	}
}

func TestInterpretReadOnlyCommandsNeedNoMarker(t *testing.T) {
	m := &fakeModel{reply: func(string) (intent.Command, error) { return intent.Command{Intent: intent.ListTasks}, nil }}
	r := newRig(m)
	p := &progressLog{}
	if _, err := Interpret(r.agent).Handle(bg, job("what is open", ""), p); err != nil {
		t.Fatal(err)
	}
	if len(p.saved) != 0 {
		t.Errorf("a read-only command saved a marker: %q", p.saved)
	}
}

// The at-most-once rule: a rerun that finds the marker must not run again.
func TestInterpretDoesNotRepeatAChangeThatMayHaveHappened(t *testing.T) {
	m := creates("Check the fuel line")
	r := newRig(m)
	out, err := Interpret(r.agent).Handle(bg, job("make a task", markExecuting), &progressLog{})
	if !errors.Is(err, ErrPermanent) || out != "" {
		t.Fatalf("err=%v out=%q, want a permanent refusal", err, out)
	}
	if !strings.Contains(err.Error(), "may already have changed") || !strings.Contains(err.Error(), "queue retry 7") {
		t.Errorf("the message must tell a person what to do: %v", err)
	}
	if m.called() != 0 || len(r.tasks(t)) != 0 {
		t.Errorf("the job ran again: model calls=%d tasks=%d", m.called(), len(r.tasks(t)))
	}
}

func TestInterpretFailsBeforeAnyChangeWhenTheMarkerCannotBeSaved(t *testing.T) {
	r := newRig(creates("x"))
	_, err := Interpret(r.agent).Handle(bg, job("make a task", ""), &progressLog{failing: true})
	if err == nil || errors.Is(err, ErrPermanent) {
		t.Fatalf("err = %v, want a retryable error", err)
	}
	if len(r.tasks(t)) != 0 {
		t.Error("the command ran although its safety marker could not be saved")
	}
}

func TestInterpretModelProblemsAreRetryableAndLeaveNoMarker(t *testing.T) {
	for name, cause := range map[string]error{
		"unavailable": llm.ErrUnavailable,
		"too slow":    context.DeadlineExceeded,
	} {
		m := &fakeModel{reply: func(string) (intent.Command, error) { return intent.Command{}, cause }}
		r := newRig(m)
		p := &progressLog{}
		_, err := Interpret(r.agent).Handle(bg, job("do something", ""), p)
		if err == nil || errors.Is(err, ErrPermanent) || !errors.Is(err, cause) {
			t.Errorf("%s: err = %v", name, err)
		}
		if len(p.saved) != 0 {
			t.Errorf("%s: a marker was saved although nothing ran", name)
		}
	}
}

func TestInterpretWithoutAModelIsRetryable(t *testing.T) {
	r := newRig(nil)
	_, err := Interpret(r.agent).Handle(bg, job("do something", ""), &progressLog{})
	if !errors.Is(err, agent.ErrNoModel) || errors.Is(err, ErrPermanent) {
		t.Errorf("err = %v", err)
	}
}

func TestInterpretUnreadableMessageIsPermanent(t *testing.T) {
	m := &fakeModel{reply: func(string) (intent.Command, error) { return intent.Command{}, structured.ErrNoValidReply }}
	r := newRig(m)
	_, err := Interpret(r.agent).Handle(bg, job("blorp", ""), &progressLog{})
	if !errors.Is(err, ErrPermanent) {
		t.Errorf("err = %v", err)
	}
}

func TestInterpretRefusalsAndUnknownIntentsAreAnswersNotFailures(t *testing.T) {
	cases := map[string]intent.Command{
		"nothing matches": {Intent: intent.UpdateTask, TaskRef: "the flux capacitor", Status: "done"},
		"unknown intent":  {Intent: intent.Unknown},
	}
	for name, cmd := range cases {
		cmd := cmd
		m := &fakeModel{reply: func(string) (intent.Command, error) { return cmd, nil }}
		r := newRig(m)
		out, err := Interpret(r.agent).Handle(bg, job("whatever", ""), &progressLog{})
		if err != nil || out == "" {
			t.Errorf("%s: out=%q err=%v (a refusal is a finished job with an explanation)", name, out, err)
		}
		if len(r.tasks(t)) != 0 {
			t.Errorf("%s: changed something", name)
		}
	}
}

func TestBadPayloadsArePermanent(t *testing.T) {
	r := newRig(creates("x"))
	for name, raw := range map[string]string{
		"empty text":    `{"text":"   "}`,
		"extra field":   `{"text":"hi","sneaky":1}`,
		"not an object": `["hi"]`,
		"no text":       `{}`,
	} {
		j := jobqueue.Job{ID: 1, Payload: json.RawMessage(raw)}
		for _, spec := range []Spec{Interpret(r.agent), DeepThink(DeepThinkConfig{Client: &llm.Fake{}, Model: "m"})} {
			if _, err := spec.Handle(bg, j, &progressLog{}); !errors.Is(err, ErrPermanent) {
				t.Errorf("%s: err = %v", name, err)
			}
		}
	}
}

// The whole story with a real queue: the process dies after the change was made
// but before it reported. Exactly one task must exist at the end, and the job
// must be in the dead-letter state for a person to look at.
func TestCrashAfterTheChangeNeverCreatesADuplicateTask(t *testing.T) {
	e := newEnv(t)
	m := creates("Check the fuel line")
	r := newRig(m)
	inner := Interpret(r.agent)
	j, _, err := e.q.Enqueue(bg, "k", KindInterpret, json.RawMessage(`{"text":"add a task about the fuel line"}`), jobqueue.EnqueueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	attempt := 0
	crashing := Spec{Handle: func(ctx context.Context, jb jobqueue.Job, p Progress) (string, error) {
		attempt++
		out, err := inner.Handle(ctx, jb, p)
		if attempt == 1 && err == nil {
			return "", errors.New("the process died before reporting") // stands in for kill -9
		}
		return out, err
	}}
	e.start(Config{Specs: map[string]Spec{KindInterpret: crashing}})
	eventually(t, "dead-lettered", 5*time.Second, func() bool { return e.get(j.ID).Status == jobqueue.Dead })
	if n := len(r.tasks(t)); n != 1 {
		t.Errorf("%d tasks exist, want exactly 1", n)
	}
	if m.called() != 1 {
		t.Errorf("the model was asked %d times", m.called())
	}
	if !strings.Contains(e.get(j.ID).LastError, "may already have changed") {
		t.Errorf("last error %q", e.get(j.ID).LastError)
	}
}

// ---- DeepThink ---------------------------------------------------------------------------

func TestDeepThinkGroundsTheAnswerInTheTaskData(t *testing.T) {
	var got llm.Request
	f := &llm.Fake{Handler: func(ctx context.Context, req llm.Request) (string, error) {
		got = req
		return "  1. Fix the pump first.\n", nil
	}}
	spec := DeepThink(DeepThinkConfig{Client: f, Model: "big", Context: func(context.Context) (string, error) {
		return "T1 Leaking pump (high)", nil
	}})
	out, err := spec.Handle(bg, job("what should I prioritize this week?", ""), &progressLog{})
	if err != nil || out != "1. Fix the pump first." {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if got.Model != "big" || len(got.Messages) != 2 {
		t.Fatalf("request %+v", got)
	}
	user := got.Messages[1].Content
	if !strings.Contains(user, "T1 Leaking pump (high)") || !strings.Contains(user, "what should I prioritize this week?") {
		t.Errorf("the prompt must contain the data and the question:\n%s", user)
	}
	if !strings.Contains(got.Messages[0].Content, "ONLY the task data") {
		t.Errorf("system prompt: %s", got.Messages[0].Content)
	}
}

func TestDeepThinkNeedsNetworkFollowsTheConfig(t *testing.T) {
	if DeepThink(DeepThinkConfig{Client: &llm.Fake{}, Model: "m", NeedsNetwork: true}).NeedsNetwork != true {
		t.Error("NeedsNetwork lost")
	}
	if DeepThink(DeepThinkConfig{Client: &llm.Fake{}, Model: "m"}).NeedsNetwork {
		t.Error("NeedsNetwork invented")
	}
}

func TestDeepThinkErrors(t *testing.T) {
	run := func(h func(context.Context, llm.Request) (string, error), ctxFn func(context.Context) (string, error)) error {
		spec := DeepThink(DeepThinkConfig{Client: &llm.Fake{Handler: h}, Model: "m", Context: ctxFn})
		_, err := spec.Handle(bg, job("why", ""), &progressLog{})
		return err
	}
	if err := run(func(context.Context, llm.Request) (string, error) { return "", llm.ErrUnavailable }, nil); !errors.Is(err, llm.ErrUnavailable) || errors.Is(err, ErrPermanent) {
		t.Errorf("unavailable: %v", err)
	}
	err := run(func(context.Context, llm.Request) (string, error) { return "", &llm.ModelNotFoundError{Model: "m"} }, nil)
	if !errors.Is(err, ErrPermanent) || !strings.Contains(err.Error(), "ollama pull m") {
		t.Errorf("missing model: %v", err)
	}
	if err := run(func(context.Context, llm.Request) (string, error) { return "   ", nil }, nil); err == nil || errors.Is(err, ErrPermanent) {
		t.Errorf("empty answer: %v", err)
	}
	if err := run(nil, func(context.Context) (string, error) { return "", errors.New("db busy") }); err == nil || errors.Is(err, ErrPermanent) || !strings.Contains(err.Error(), "db busy") {
		t.Errorf("context failure: %v", err)
	}
}

func TestDeepThinkTruncatesHugeAnswers(t *testing.T) {
	f := &llm.Fake{Handler: func(context.Context, llm.Request) (string, error) { return strings.Repeat("x", maxResultBytes*3), nil }}
	out, err := DeepThink(DeepThinkConfig{Client: f, Model: "m"}).Handle(bg, job("why", ""), &progressLog{})
	if err != nil || len(out) != maxResultBytes {
		t.Errorf("len=%d err=%v", len(out), err)
	}
}

func TestDeepThinkHonoursCancellation(t *testing.T) {
	f := &llm.Fake{Handler: func(ctx context.Context, req llm.Request) (string, error) { <-ctx.Done(); return "", ctx.Err() }}
	ctx, cancel := context.WithTimeout(bg, 30*time.Millisecond)
	defer cancel()
	_, err := DeepThink(DeepThinkConfig{Client: f, Model: "m"}).Handle(ctx, job("why", ""), &progressLog{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v", err)
	}
}
