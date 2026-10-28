package drain

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"offgrid/internal/agent"
	"offgrid/internal/intent"
	"offgrid/internal/jobqueue"
	"offgrid/internal/store/sqlstore"
	"offgrid/internal/structured"
	"offgrid/internal/task"
)

type safeRig struct {
	st    *sqlstore.Store
	model *fakeModel
	spec  Spec
}

func newSafeRig(t *testing.T, reply func(string) (intent.Command, error)) *safeRig {
	t.Helper()
	st, err := sqlstore.Open(bg, filepath.Join(t.TempDir(), "t.db"), sqlstore.Options{NodeID: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := &fakeModel{reply: reply}
	r := &safeRig{st: st, model: m}
	r.spec = InterpretSafely(InterpretSafelyConfig{Agent: &agent.Agent{Model: m}, Store: st, Sink: st, Clock: func() time.Time { return thursday }, By: "qwen"})
	if _, _, err := st.Create(bg, task.NewTask{Title: "Leaking pump", Site: "4", Priority: task.PriorityMedium}); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *safeRig) run(key, text string, prog Progress) (string, error) {
	return r.spec.Handle(bg, jobqueue.Job{ID: 1, Key: key, Kind: KindInterpret, Payload: []byte(fmt.Sprintf(`{"text":%q}`, text))}, prog)
}

func (r *safeRig) task() task.Task {
	t, _ := r.st.Get(bg, "T1")
	return t
}

func say(c intent.Command) func(string) (intent.Command, error) {
	return func(string) (intent.Command, error) { return c, nil }
}

func TestAQueuedUpdateBecomesASuggestionAndTheTaskIsUntouched(t *testing.T) {
	r := newSafeRig(t, say(intent.Command{Intent: intent.UpdateTask, TaskRef: "T1", Status: "done"}))
	before := r.task()
	out, err := r.run("job-1", "pump is fixed", &progressLog{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Suggested, not applied") || !strings.Contains(out, "set status to done on T1") || !strings.Contains(out, "laptop.") {
		t.Fatalf("%q", out)
	}
	if after := r.task(); fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("the task changed: %+v", after)
	}
	l, _ := r.st.Suggestions(bg, false)
	if len(l) != 1 || l[0].By != "qwen" || l[0].Key != "job-1" || *l[0].Status != task.StatusDone {
		t.Fatalf("%+v", l)
	}
}

func TestRunningTheSameJobAgainProposesOnce(t *testing.T) {
	r := newSafeRig(t, say(intent.Command{Intent: intent.UpdateTask, TaskRef: "T1", Priority: "high"}))
	for i := 0; i < 3; i++ {
		if _, err := r.run("job-1", "pump is urgent", &progressLog{}); err != nil {
			t.Fatal(err)
		}
	}
	if l, _ := r.st.Suggestions(bg, false); len(l) != 1 {
		t.Fatalf("%d suggestions after three attempts", len(l))
	}
}

func TestAQueuedNoteBecomesASuggestion(t *testing.T) {
	r := newSafeRig(t, say(intent.Command{Intent: intent.AddNote, TaskRef: "T1", Note: "seal cracked"}))
	out, err := r.run("job-2", "note: seal cracked", &progressLog{})
	if err != nil || !strings.Contains(out, "add the note") {
		t.Fatalf("%q %v", out, err)
	}
	if len(r.task().Notes) != 0 {
		t.Fatal("a note was written")
	}
}

func TestCreatingANewTaskStillWorksDirectlyAndKeepsTheCrashMarker(t *testing.T) {
	r := newSafeRig(t, say(intent.Command{Intent: intent.CreateTask, Title: "Check valve", Site: "2", Priority: "high"}))
	prog := &progressLog{}
	out, err := r.run("job-3", "new task: check valve at site 2", prog)
	if err != nil || !strings.Contains(out, "Created") {
		t.Fatalf("%q %v", out, err)
	}
	if len(prog.saved) != 1 || prog.saved[0] != markExecuting {
		t.Fatalf("progress %v", prog.saved)
	}
	if l, _ := r.st.List(bg, task.Filter{}); len(l) != 2 {
		t.Fatalf("%d tasks", len(l))
	}
	// after a crash between the marker and the end, a rerun must not create a second one
	_, err = r.spec.Handle(bg, jobqueue.Job{ID: 1, Key: "job-3", Progress: markExecuting, Payload: []byte(`{"text":"x"}`)}, &progressLog{})
	if !errors.Is(err, ErrPermanent) {
		t.Fatalf("%v", err)
	}
}

func TestASuggestionJobNeedsNoCrashMarker(t *testing.T) {
	r := newSafeRig(t, say(intent.Command{Intent: intent.UpdateTask, TaskRef: "T1", Status: "done"}))
	prog := &progressLog{}
	if _, err := r.run("job-4", "x", prog); err != nil || len(prog.saved) != 0 {
		t.Fatalf("%v %v: a repeatable proposal must not park the job behind a marker", err, prog.saved)
	}
}

func TestRefusalsAndNoOpsProposeNothing(t *testing.T) {
	r := newSafeRig(t, say(intent.Command{Intent: intent.UpdateTask, TaskRef: "T9", Status: "done"}))
	if out, err := r.run("a", "x", &progressLog{}); err != nil || !strings.HasPrefix(out, "Not done:") {
		t.Fatalf("%q %v", out, err)
	}
	r = newSafeRig(t, say(intent.Command{Intent: intent.UpdateTask, TaskRef: "T1", Priority: "medium"}))
	if out, err := r.run("b", "x", &progressLog{}); err != nil || !strings.Contains(out, "Already up to date") {
		t.Fatalf("%q %v", out, err)
	}
	r = newSafeRig(t, say(intent.Command{Intent: intent.Unknown}))
	if out, err := r.run("c", "delete everything", &progressLog{}); err != nil || out == "" {
		t.Fatalf("%q %v", out, err)
	}
	for _, rr := range []*safeRig{r} {
		if l, _ := rr.st.Suggestions(bg, true); len(l) != 0 {
			t.Fatalf("%+v", l)
		}
	}
}

func TestReadingStillWorks(t *testing.T) {
	r := newSafeRig(t, say(intent.Command{Intent: intent.ListTasks}))
	if out, err := r.run("l", "what is open", &progressLog{}); err != nil || !strings.Contains(out, "Leaking pump") {
		t.Fatalf("%q %v", out, err)
	}
}

func TestModelFailuresAreClassifiedLikeInterpret(t *testing.T) {
	r := newSafeRig(t, func(string) (intent.Command, error) { return intent.Command{}, structured.ErrNoValidReply })
	if _, err := r.run("a", "x", &progressLog{}); !errors.Is(err, ErrPermanent) {
		t.Fatalf("%v", err)
	}
	r = newSafeRig(t, func(string) (intent.Command, error) { return intent.Command{}, errors.New("server down") })
	if _, err := r.run("b", "x", &progressLog{}); err == nil || errors.Is(err, ErrPermanent) {
		t.Fatalf("%v", err)
	}
	r.spec = InterpretSafely(InterpretSafelyConfig{Agent: &agent.Agent{}, Store: r.st, Sink: r.st})
	if _, err := r.run("c", "x", &progressLog{}); !errors.Is(err, agent.ErrNoModel) {
		t.Fatalf("%v", err)
	}
	if _, err := r.spec.Handle(bg, jobqueue.Job{Payload: []byte(`{}`)}, &progressLog{}); !errors.Is(err, ErrPermanent) {
		t.Fatalf("%v", err)
	}
	if _, err := r.spec.Handle(bg, jobqueue.Job{Payload: []byte(`{"text":"x"}`)}, &progressLog{failing: true}); err == nil {
		t.Log("(no marker is saved for a non-create command, so a failing Save is not reached)")
	}
}

// Scenario: a human edit made offline survives an AI job that finishes later, through the real queue and worker.
func TestAHumanEditSurvivesAnAIJobThatFinishesAfterIt(t *testing.T) {
	r := newSafeRig(t, say(intent.Command{Intent: intent.UpdateTask, TaskRef: "T1", Priority: "low"}))
	e := newEnv(t)
	j, _, err := e.q.Enqueue(bg, "k1", KindInterpret, []byte(`{"text":"pump can wait"}`), jobqueue.EnqueueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// While the job waits (offline), a person edits the task.
	hi := task.PriorityHigh
	if _, _, err := r.st.Update(bg, "T1", task.Changes{Priority: &hi}); err != nil {
		t.Fatal(err)
	}
	e.start(Config{Specs: map[string]Spec{KindInterpret: r.spec}})
	eventually(t, "the AI job finishes", 3*time.Second, allDone(e, j.ID))
	if got := r.task().Priority; got != task.PriorityHigh {
		t.Fatalf("the AI job overwrote the person: priority is %s", got)
	}
	// And now the person changes it again: the waiting suggestion becomes stale and cannot be accepted.
	l, _ := r.st.Suggestions(bg, false)
	if len(l) != 1 {
		t.Fatalf("%+v", l)
	}
	med := task.PriorityMedium
	if _, _, err := r.st.Update(bg, "T1", task.Changes{Priority: &med}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.Resolve(bg, l[0].ID, true); !errors.Is(err, task.ErrStale) {
		t.Fatalf("%v", err)
	}
}
