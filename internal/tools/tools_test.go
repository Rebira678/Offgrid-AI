package tools_test

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"offgrid/internal/intent"
	"offgrid/internal/task"
	"offgrid/internal/tools"
)

var thursday = time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)

// spy wraps a store and counts every call, so tests can prove that a rejected
// command never reached the store at all (not even to read).
type spy struct {
	task.Store
	reads, writes int
}

func (s *spy) Create(ctx context.Context, n task.NewTask) (task.Task, bool, error) {
	s.writes++
	return s.Store.Create(ctx, n)
}
func (s *spy) Update(ctx context.Context, id string, c task.Changes) (task.Task, bool, error) {
	s.writes++
	return s.Store.Update(ctx, id, c)
}
func (s *spy) AddNote(ctx context.Context, id, text string) (task.Task, error) {
	s.writes++
	return s.Store.AddNote(ctx, id, text)
}
func (s *spy) Get(ctx context.Context, id string) (task.Task, error) {
	s.reads++
	return s.Store.Get(ctx, id)
}
func (s *spy) List(ctx context.Context, f task.Filter) ([]task.Task, error) {
	s.reads++
	return s.Store.List(ctx, f)
}

type rig struct {
	t     *testing.T
	reg   *tools.Registry
	store *spy
	clock *time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	now := thursday
	r := &rig{t: t, clock: &now}
	r.store = &spy{Store: task.NewMemStore(func() time.Time { return *r.clock })}
	r.reg = tools.NewRegistry(r.store, func() time.Time { return *r.clock })
	return r
}

func (r *rig) run(c intent.Command) (tools.Result, error) {
	r.t.Helper()
	return r.reg.Execute(context.Background(), c)
}

func (r *rig) mustRun(c intent.Command) tools.Result {
	r.t.Helper()
	res, err := r.run(c)
	if err != nil {
		r.t.Fatalf("Execute(%+v): %v", c, err)
	}
	return res
}

func (r *rig) all() []task.Task {
	r.t.Helper()
	got, err := r.store.Store.List(context.Background(), task.Filter{})
	if err != nil {
		r.t.Fatal(err)
	}
	return got
}

func create(title, site, priority string) intent.Command {
	return intent.Command{Intent: intent.CreateTask, Title: title, Site: site, Priority: priority}
}

// ---- create_task ----------------------------------------------------------

func TestCreateTask(t *testing.T) {
	r := newRig(t)
	res := r.mustRun(create("Leaking pump", "4", "high"))
	if !res.Changed || res.Task == nil || res.Tool != intent.CreateTask {
		t.Fatalf("result = %+v", res)
	}
	if res.Task.ID != "T1" || res.Task.Title != "Leaking pump" || res.Task.Site != "4" ||
		res.Task.Priority != task.PriorityHigh || res.Task.Status != task.StatusOpen {
		t.Errorf("task = %+v", res.Task)
	}
	if !strings.Contains(res.Message, "Created") || !strings.Contains(res.Message, "Leaking pump") {
		t.Errorf("message = %q", res.Message)
	}
}

func TestCreateTaskDefaultsAndTidiesInput(t *testing.T) {
	r := newRig(t)
	res := r.mustRun(create("  Replace    filter ", "", "  HIGH "))
	if res.Task.Title != "Replace filter" {
		t.Errorf("title = %q, want spaces collapsed", res.Task.Title)
	}
	if res.Task.Priority != task.PriorityHigh {
		t.Errorf("priority = %q; the validator normalizes case", res.Task.Priority)
	}
	res = r.mustRun(create("Another", "", ""))
	if res.Task.Priority != task.PriorityMedium {
		t.Errorf("default priority = %q, want medium", res.Task.Priority)
	}
}

func TestCreateTaskIsIdempotent(t *testing.T) {
	r := newRig(t)
	first := r.mustRun(create("Leaking pump", "4", "high"))
	again := r.mustRun(create("leaking pump", "4", "low"))
	if again.Changed || again.Task.ID != first.Task.ID {
		t.Fatalf("repeat = %+v; want no change and the same task", again)
	}
	if !strings.Contains(again.Message, "Already") {
		t.Errorf("message = %q", again.Message)
	}
	if n := len(r.all()); n != 1 {
		t.Errorf("%d tasks, want 1", n)
	}
}

// ---- update_task ----------------------------------------------------------

func TestUpdateTask(t *testing.T) {
	r := newRig(t)
	r.mustRun(create("Leaking pump", "4", "low"))

	*r.clock = thursday.Add(time.Hour)
	res := r.mustRun(intent.Command{Intent: intent.UpdateTask, TaskRef: "pump", Status: "In Progress", Priority: "high"})
	if !res.Changed || res.Task.Status != task.StatusInProgress || res.Task.Priority != task.PriorityHigh {
		t.Fatalf("result = %+v", res)
	}

	res = r.mustRun(intent.Command{Intent: intent.UpdateTask, TaskRef: "T1", Status: "done"})
	if res.Task.Status != task.StatusDone || res.Task.DoneAt.IsZero() {
		t.Errorf("done task = %+v", res.Task)
	}
	if res.Task.Priority != task.PriorityHigh {
		t.Errorf("changing only the status changed the priority to %q", res.Task.Priority)
	}
}

func TestUpdateWithNothingNewReportsNoChange(t *testing.T) {
	r := newRig(t)
	r.mustRun(create("a", "", "low"))
	res := r.mustRun(intent.Command{Intent: intent.UpdateTask, TaskRef: "a", Status: "open"})
	if res.Changed || !strings.Contains(res.Message, "Already up to date") {
		t.Fatalf("result = %+v", res)
	}
}

func TestUpdateUnknownTaskChangesNothing(t *testing.T) {
	r := newRig(t)
	r.mustRun(create("Leaking pump", "", "low"))
	before := r.store.writes
	_, err := r.run(intent.Command{Intent: intent.UpdateTask, TaskRef: "boiler", Status: "done"})
	if !errors.Is(err, task.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
	if r.store.writes != before {
		t.Error("an unresolvable reference caused a write")
	}
}

func TestAmbiguousReferenceIsRefusedNotGuessed(t *testing.T) {
	r := newRig(t)
	r.mustRun(create("Leaking pump", "4", "low"))
	r.mustRun(create("Pump inspection", "5", "low"))
	before := r.store.writes

	_, err := r.run(intent.Command{Intent: intent.UpdateTask, TaskRef: "pump", Status: "done"})
	var amb *task.AmbiguousError
	if !errors.Is(err, task.ErrAmbiguous) || !errors.As(err, &amb) || len(amb.Candidates) != 2 {
		t.Fatalf("error = %v, want an ambiguity error with 2 candidates", err)
	}
	_, err = r.run(intent.Command{Intent: intent.AddNote, TaskRef: "pump", Note: "x"})
	if !errors.Is(err, task.ErrAmbiguous) {
		t.Fatalf("add_note error = %v, want ErrAmbiguous", err)
	}
	if r.store.writes != before {
		t.Error("an ambiguous reference caused a write")
	}
	for _, tk := range r.all() {
		if tk.Status != task.StatusOpen || len(tk.Notes) != 0 {
			t.Errorf("task changed: %+v", tk)
		}
	}
}

// ---- add_note -------------------------------------------------------------

func TestAddNote(t *testing.T) {
	r := newRig(t)
	r.mustRun(create("Leaking pump", "4", "low"))
	res := r.mustRun(intent.Command{Intent: intent.AddNote, TaskRef: "pump", Note: "  seal is cracked "})
	if !res.Changed || len(res.Task.Notes) != 1 || res.Task.Notes[0].Text != "seal is cracked" {
		t.Fatalf("result = %+v", res)
	}
	res = r.mustRun(intent.Command{Intent: intent.AddNote, TaskRef: "T1", Note: "line one\nline two"})
	if len(res.Task.Notes) != 2 || res.Task.Notes[1].Text != "line one\nline two" {
		t.Errorf("a note may span lines: %+v", res.Task.Notes)
	}
}

// ---- list_tasks -----------------------------------------------------------

func TestListTasks(t *testing.T) {
	r := newRig(t)
	*r.clock = thursday.AddDate(0, 0, -1)
	r.mustRun(create("old low", "4", "low"))
	r.mustRun(create("old high", "5", "high"))
	*r.clock = thursday
	r.mustRun(create("new medium", "4", "medium"))
	r.mustRun(create("new done", "4", "low"))
	r.mustRun(intent.Command{Intent: intent.UpdateTask, TaskRef: "new done", Status: "done"})

	titles := func(c intent.Command) []string {
		c.Intent = intent.ListTasks
		res := r.mustRun(c)
		out := []string{}
		for _, tk := range res.Tasks {
			out = append(out, tk.Title)
		}
		return out
	}
	check := func(name string, got []string, want ...string) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	check("default hides done, urgent first", titles(intent.Command{}), "old high", "new medium", "old low")
	check("status done", titles(intent.Command{Status: "done"}), "new done")
	check("site", titles(intent.Command{Site: "4"}), "new medium", "old low")
	check("priority", titles(intent.Command{Priority: "high"}), "old high")
	check("yesterday", titles(intent.Command{When: "yesterday"}), "old high", "old low")
	check("today", titles(intent.Command{When: "today"}), "new medium")
	check("this week, status open", titles(intent.Command{When: "this_week", Status: "open"}), "old high", "new medium", "old low")
}

func TestListWithNoMatchesSaysSo(t *testing.T) {
	r := newRig(t)
	res := r.mustRun(intent.Command{Intent: intent.ListTasks})
	if res.Message != "No matching tasks." || len(res.Tasks) != 0 || res.Changed {
		t.Fatalf("result = %+v", res)
	}
}

func TestListNeverWrites(t *testing.T) {
	r := newRig(t)
	r.mustRun(create("a", "", "low"))
	before := r.store.writes
	r.mustRun(intent.Command{Intent: intent.ListTasks})
	r.mustRun(intent.Command{Intent: intent.DailySummary})
	if r.store.writes != before {
		t.Error("a read-only tool wrote to the store")
	}
}

// ---- daily_summary --------------------------------------------------------

func TestDailySummaryCounts(t *testing.T) {
	r := newRig(t)
	*r.clock = thursday.AddDate(0, 0, -1)
	r.mustRun(create("finished yesterday", "", "low"))
	r.mustRun(create("still open, high", "", "high"))
	r.mustRun(intent.Command{Intent: intent.UpdateTask, TaskRef: "finished yesterday", Status: "done"})

	*r.clock = thursday
	r.mustRun(create("made today", "", "high"))
	r.mustRun(create("finished today", "", "low"))
	r.mustRun(intent.Command{Intent: intent.UpdateTask, TaskRef: "finished today", Status: "done"})
	r.mustRun(intent.Command{Intent: intent.AddNote, TaskRef: "made today", Note: "n1"})
	r.mustRun(intent.Command{Intent: intent.AddNote, TaskRef: "still open", Note: "n2"})

	today := r.mustRun(intent.Command{Intent: intent.DailySummary}).Summary // no "when" means today
	want := tools.Summary{Range: "today", Created: 2, Completed: 1, NotesAdded: 2, StillOpen: 2, HighPriorityOpen: 2}
	if *today != want {
		t.Errorf("today = %+v\nwant    %+v", *today, want)
	}
	yest := r.mustRun(intent.Command{Intent: intent.DailySummary, When: "yesterday"}).Summary
	want = tools.Summary{Range: "yesterday", Created: 2, Completed: 1, NotesAdded: 0, StillOpen: 2, HighPriorityOpen: 2}
	if *yest != want {
		t.Errorf("yesterday = %+v\nwant       %+v", *yest, want)
	}
	week := r.mustRun(intent.Command{Intent: intent.DailySummary, When: "this_week"}).Summary
	if week.Created != 4 || week.Completed != 2 || week.NotesAdded != 2 {
		t.Errorf("this week = %+v", *week)
	}
}

func TestSummaryCountsACompletionWhenItHappenedNotWhenTheTaskWasLastTouched(t *testing.T) {
	r := newRig(t)
	*r.clock = thursday.AddDate(0, 0, -1)
	r.mustRun(create("a", "", "low"))
	r.mustRun(intent.Command{Intent: intent.UpdateTask, TaskRef: "a", Status: "done"})
	*r.clock = thursday
	r.mustRun(intent.Command{Intent: intent.AddNote, TaskRef: "a", Note: "late note"}) // moves UpdatedAt to today

	if got := r.mustRun(intent.Command{Intent: intent.DailySummary}).Summary.Completed; got != 0 {
		t.Errorf("today completed = %d; the task was finished yesterday", got)
	}
	if got := r.mustRun(intent.Command{Intent: intent.DailySummary, When: "yesterday"}).Summary.Completed; got != 1 {
		t.Errorf("yesterday completed = %d, want 1", got)
	}
}

// ---- the safety properties --------------------------------------------------

// Every command below is something a model might emit. Each must be refused,
// and refused BEFORE the store is touched: zero reads, zero writes.
func TestRejectedCommandsNeverTouchTheStore(t *testing.T) {
	bad := map[string]intent.Command{
		"empty create":       {Intent: intent.CreateTask},
		"whitespace title":   create("   ", "", ""),
		"unknown priority":   create("x", "", "urgent"),
		"title too long":     create(strings.Repeat("a", 121), "", ""),
		"newline in title":   create("line one\nline two", "", ""),
		"escape code":        create("\x1b[31mred", "", ""),
		"NUL byte":           create("a\x00b", "", ""),
		"invalid UTF-8":      create("a\xffb", "", ""),
		"site injection":     create("x", "4; DROP TABLE tasks", ""),
		"site with slash":    create("x", "../etc", ""),
		"unknown intent":     {Intent: "delete_everything"},
		"empty intent":       {},
		"update no ref":      {Intent: intent.UpdateTask, Status: "done"},
		"update nothing":     {Intent: intent.UpdateTask, TaskRef: "a"},
		"update bad status":  {Intent: intent.UpdateTask, TaskRef: "a", Status: "finished"},
		"note no text":       {Intent: intent.AddNote, TaskRef: "a"},
		"note no ref":        {Intent: intent.AddNote, Note: "x"},
		"note control char":  {Intent: intent.AddNote, TaskRef: "a", Note: "bell\a"},
		"note too long":      {Intent: intent.AddNote, TaskRef: "a", Note: strings.Repeat("n", 501)},
		"list bad when":      {Intent: intent.ListTasks, When: "tomorrow"},
		"list bad status":    {Intent: intent.ListTasks, Status: "weird"},
		"list injected site": {Intent: intent.ListTasks, Site: "' OR 1=1"},
		"summary bad when":   {Intent: intent.DailySummary, When: "next_year"},
		"ref with control":   {Intent: intent.UpdateTask, TaskRef: "a\x00", Status: "done"},
	}
	for name, cmd := range bad {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			_, err := r.run(cmd)
			if err == nil {
				t.Fatalf("command %+v was accepted", cmd)
			}
			var argErr *tools.ArgumentError
			if !errors.As(err, &argErr) && !errors.Is(err, tools.ErrNoTool) {
				t.Errorf("error %v is neither an ArgumentError nor ErrNoTool", err)
			}
			if r.store.reads != 0 || r.store.writes != 0 {
				t.Errorf("store touched: %d reads, %d writes", r.store.reads, r.store.writes)
			}
		})
	}
}

func TestArgumentErrorListsEveryProblem(t *testing.T) {
	r := newRig(t)
	_, err := r.run(intent.Command{Intent: intent.CreateTask, Priority: "urgent", Status: "finished"})
	var argErr *tools.ArgumentError
	if !errors.As(err, &argErr) {
		t.Fatalf("error = %v", err)
	}
	if argErr.Tool != intent.CreateTask || len(argErr.Problems) < 3 {
		t.Errorf("problems = %v; want the empty title, the priority and the status all reported", argErr.Problems)
	}
}

func TestRegistryValidatesEvenWhenTheCallerClaimsToHave(t *testing.T) {
	// A command that "looks" valid but was never passed through intent.Validate:
	// wrong-case enum values. The registry must normalize and accept it, and a
	// truly bad one must be refused. The caller's own value is never modified.
	r := newRig(t)
	cmd := intent.Command{Intent: "CREATE_TASK", Title: "x", Priority: "High"}
	res := r.mustRun(cmd)
	if res.Task.Priority != task.PriorityHigh {
		t.Errorf("priority = %q", res.Task.Priority)
	}
	if cmd.Intent != "CREATE_TASK" || cmd.Priority != "High" {
		t.Errorf("Execute modified the caller's command: %+v", cmd)
	}
}

func TestUnknownIntentIsADeclinedRequestNotAFailure(t *testing.T) {
	r := newRig(t)
	res, err := r.run(intent.Command{Intent: intent.Unknown})
	if !errors.Is(err, tools.ErrNoTool) {
		t.Fatalf("error = %v, want ErrNoTool", err)
	}
	if res.Message == "" || res.Changed {
		t.Errorf("result = %+v; want a friendly message and no change", res)
	}
	if r.store.reads != 0 || r.store.writes != 0 {
		t.Error("the unknown intent touched the store")
	}
}

func TestCancelledContextDoesNothing(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.reg.Execute(ctx, create("a", "", ""))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if r.store.reads != 0 || r.store.writes != 0 {
		t.Error("a cancelled command touched the store")
	}
}

func TestOneCommandSeesOneMoment(t *testing.T) {
	// The clock is read once per Execute. Here it jumps a day on every call; a
	// tool that read it twice would compare against two different "todays".
	n := 0
	clock := func() time.Time { n++; return thursday.AddDate(0, 0, n) }
	store := task.NewMemStore(func() time.Time { return thursday })
	reg := tools.NewRegistry(store, clock)
	if _, err := reg.Execute(context.Background(), intent.Command{Intent: intent.DailySummary}); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("the clock was read %d times in one command, want 1", n)
	}
}

// ---- the registry itself ------------------------------------------------------

func TestEveryIntentExceptUnknownHasExactlyOneTool(t *testing.T) {
	reg := newRig(t).reg
	have := map[intent.Intent]int{}
	for _, s := range reg.Specs() {
		have[s.Intent]++
	}
	for _, in := range intent.Intents() {
		switch {
		case in == intent.Unknown && have[in] != 0:
			t.Errorf("%q must have no tool", in)
		case in != intent.Unknown && have[in] != 1:
			t.Errorf("intent %q has %d tools, want 1", in, have[in])
		}
	}
	if len(reg.Specs()) != 5 {
		t.Errorf("%d tools, want 5", len(reg.Specs()))
	}
}

func TestSpecsOnlyNameRealCommandFields(t *testing.T) {
	fields := map[string]bool{}
	rt := reflect.TypeOf(intent.Command{})
	for i := 0; i < rt.NumField(); i++ {
		fields[strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]] = true
	}
	for _, s := range newRig(t).reg.Specs() {
		if s.Does == "" || len(s.Args) == 0 {
			t.Errorf("%s: spec is incomplete", s.Intent)
		}
		for _, a := range s.Args {
			if !fields[a.Field] {
				t.Errorf("%s documents an argument %q that Command does not have", s.Intent, a.Field)
			}
			if a.Doc == "" {
				t.Errorf("%s.%s has no description", s.Intent, a.Field)
			}
		}
	}
}

// The rules must not depend on a model. This reads the real import lists.
func TestRulesPackagesImportNoModelCode(t *testing.T) {
	forbidden := []string{"offgrid/internal/llm", "offgrid/internal/ollama", "offgrid/internal/structured", "offgrid/internal/eval", "net/http"}
	for _, dir := range []string{".", "../task"} {
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
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
				path := strings.Trim(imp.Path.Value, `"`)
				for _, bad := range forbidden {
					if path == bad {
						t.Errorf("%s imports %s; the rules must not depend on a model or the network", f, path)
					}
				}
			}
		}
	}
}
