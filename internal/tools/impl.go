package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"offgrid/internal/intent"
	"offgrid/internal/task"
)

// ---- create_task ----------------------------------------------------------

var createTaskSpec = Spec{
	Intent: intent.CreateTask,
	Does:   "Create a task. If an unfinished task with the same title and site exists, return that one instead.",
	Args: []ArgSpec{
		{"title", true, "what needs doing, one line"},
		{"site", false, "site code, e.g. 4"},
		{"priority", false, "low, medium or high (default medium)"},
	},
}

// CreateTaskArgs is the typed form of a create_task command.
type CreateTaskArgs struct {
	Title    string
	Site     string
	Priority task.Priority
}

func parseCreate(c intent.Command) (CreateTaskArgs, error) {
	var p problems
	a := CreateTaskArgs{
		Title:    oneLine("title", c.Title, &p),
		Site:     site(c.Site, &p),
		Priority: priority(c.Priority, &p),
	}
	if a.Title == "" && len(p) == 0 {
		p.addf("title must not be empty")
	}
	if a.Priority == "" {
		a.Priority = task.PriorityMedium
	}
	return a, p.err(intent.CreateTask)
}

func doCreate(ctx context.Context, e env, a CreateTaskArgs) (Result, error) {
	t, created, err := e.store.Create(ctx, task.NewTask{Title: a.Title, Site: a.Site, Priority: a.Priority})
	if err != nil {
		return Result{}, err
	}
	if !created {
		return Result{Message: "Already on the list, nothing added: " + describe(t), Task: &t}, nil
	}
	return Result{Message: "Created " + describe(t), Changed: true, Task: &t}, nil
}

// ---- update_task ----------------------------------------------------------

var updateTaskSpec = Spec{
	Intent: intent.UpdateTask,
	Does:   "Change the status or priority of one existing task.",
	Args: []ArgSpec{
		{"task_ref", true, "which task: its ID (T3) or words from its title"},
		{"status", false, "open, in_progress or done"},
		{"priority", false, "low, medium or high"},
	},
}

// UpdateTaskArgs is the typed form of an update_task command. A nil pointer
// means "leave this alone", which a plain string could not say clearly.
type UpdateTaskArgs struct {
	Ref      string
	Status   *task.Status
	Priority *task.Priority
}

func parseUpdate(c intent.Command) (UpdateTaskArgs, error) {
	var p problems
	a := UpdateTaskArgs{Ref: oneLine("task_ref", c.TaskRef, &p)}
	if s := status(c.Status, &p); s != "" {
		a.Status = &s
	}
	if pr := priority(c.Priority, &p); pr != "" {
		a.Priority = &pr
	}
	if a.Ref == "" && len(p) == 0 {
		p.addf("task_ref must not be empty")
	}
	if a.Status == nil && a.Priority == nil && len(p) == 0 {
		p.addf("there is nothing to change: give a status or a priority")
	}
	return a, p.err(intent.UpdateTask)
}

func doUpdate(ctx context.Context, e env, a UpdateTaskArgs) (Result, error) {
	target, err := findTask(ctx, e, a.Ref)
	if err != nil {
		return Result{}, err
	}
	t, changed, err := e.store.Update(ctx, target.ID, task.Changes{Status: a.Status, Priority: a.Priority})
	if err != nil {
		return Result{}, err
	}
	if !changed {
		return Result{Message: "Already up to date, nothing changed: " + describe(t), Task: &t}, nil
	}
	return Result{Message: "Updated " + describe(t), Changed: true, Task: &t}, nil
}

// ---- list_tasks -----------------------------------------------------------

var listTasksSpec = Spec{
	Intent: intent.ListTasks,
	Does:   "List tasks. With no status given, finished tasks are left out.",
	Args: []ArgSpec{
		{"status", false, "only this status (default: everything not done)"},
		{"priority", false, "only this priority"},
		{"site", false, "only this site"},
		{"when", false, "only tasks created today, yesterday or this_week"},
	},
}

// ListTasksArgs is the typed form of a list_tasks command.
type ListTasksArgs struct {
	Status   task.Status
	Priority task.Priority
	Site     string
	When     string
}

func parseList(c intent.Command) (ListTasksArgs, error) {
	var p problems
	a := ListTasksArgs{
		Status:   status(c.Status, &p),
		Priority: priority(c.Priority, &p),
		Site:     site(c.Site, &p),
		When:     when(c.When, &p),
	}
	return a, p.err(intent.ListTasks)
}

func doList(ctx context.Context, e env, a ListTasksArgs) (Result, error) {
	f := task.Filter{Status: a.Status, ExcludeDone: a.Status == "", Priority: a.Priority, Site: a.Site}
	if a.When != "" {
		rg, err := task.RangeFor(a.When, e.now)
		if err != nil {
			return Result{}, err
		}
		f.Within = &rg
	}
	tasks, err := e.store.List(ctx, f)
	if err != nil {
		return Result{}, err
	}
	// Most urgent first; the sort is stable, so equal priorities stay in
	// creation order.
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].Priority.Rank() > tasks[j].Priority.Rank() })

	if len(tasks) == 0 {
		return Result{Message: "No matching tasks.", Tasks: tasks}, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s:", plural(len(tasks), "task", "tasks"))
	for _, t := range tasks {
		b.WriteString("\n  " + describe(t))
	}
	return Result{Message: b.String(), Tasks: tasks}, nil
}

// ---- add_note -------------------------------------------------------------

var addNoteSpec = Spec{
	Intent: intent.AddNote,
	Does:   "Attach a note to one existing task.",
	Args: []ArgSpec{
		{"task_ref", true, "which task: its ID (T3) or words from its title"},
		{"note", true, "the note text"},
	},
}

// AddNoteArgs is the typed form of an add_note command.
type AddNoteArgs struct {
	Ref  string
	Text string
}

func parseNote(c intent.Command) (AddNoteArgs, error) {
	var p problems
	a := AddNoteArgs{
		Ref:  oneLine("task_ref", c.TaskRef, &p),
		Text: freeText("note", c.Note, &p),
	}
	if a.Ref == "" && len(p) == 0 {
		p.addf("task_ref must not be empty")
	}
	if a.Text == "" && len(p) == 0 {
		p.addf("note must not be empty")
	}
	return a, p.err(intent.AddNote)
}

func doNote(ctx context.Context, e env, a AddNoteArgs) (Result, error) {
	target, err := findTask(ctx, e, a.Ref)
	if err != nil {
		return Result{}, err
	}
	t, err := e.store.AddNote(ctx, target.ID, a.Text)
	if err != nil {
		return Result{}, err
	}
	return Result{Message: fmt.Sprintf("Added a note to %s (%s now has %s)", t.ID, t.ID, plural(len(t.Notes), "note", "notes")),
		Changed: true, Task: &t}, nil
}

// ---- daily_summary --------------------------------------------------------

var dailySummarySpec = Spec{
	Intent: intent.DailySummary,
	Does:   "Count what happened in a day or week, and what is still open. Pure counting: no model writes it.",
	Args: []ArgSpec{
		{"when", false, "today, yesterday or this_week (default today)"},
	},
}

// DailySummaryArgs is the typed form of a daily_summary command.
type DailySummaryArgs struct{ When string }

func parseSummary(c intent.Command) (DailySummaryArgs, error) {
	var p problems
	a := DailySummaryArgs{When: when(c.When, &p)}
	if a.When == "" {
		a.When = "today"
	}
	return a, p.err(intent.DailySummary)
}

func doSummary(ctx context.Context, e env, a DailySummaryArgs) (Result, error) {
	rg, err := task.RangeFor(a.When, e.now)
	if err != nil {
		return Result{}, err
	}
	all, err := e.store.List(ctx, task.Filter{})
	if err != nil {
		return Result{}, err
	}
	s := Summary{Range: rg.Label}
	for _, t := range all {
		if rg.Contains(t.CreatedAt) {
			s.Created++
		}
		if t.Status == task.StatusDone && rg.Contains(t.DoneAt) {
			s.Completed++
		}
		for _, n := range t.Notes {
			if rg.Contains(n.At) {
				s.NotesAdded++
			}
		}
		if t.Status != task.StatusDone {
			s.StillOpen++
			if t.Priority == task.PriorityHigh {
				s.HighPriorityOpen++
			}
		}
	}
	msg := fmt.Sprintf("Summary for %s:\n  %s created, %s finished, %s added\n  %s still open, %d of them high priority",
		s.Range, plural(s.Created, "task", "tasks"), plural(s.Completed, "task", "tasks"),
		plural(s.NotesAdded, "note", "notes"), plural(s.StillOpen, "task", "tasks"), s.HighPriorityOpen)
	return Result{Message: msg, Summary: &s}, nil
}

// ---- shared ---------------------------------------------------------------

// findTask resolves a reference against every task. It reads, never writes:
// a reference that cannot be resolved changes nothing.
func findTask(ctx context.Context, e env, ref string) (task.Task, error) {
	all, err := e.store.List(ctx, task.Filter{})
	if err != nil {
		return task.Task{}, err
	}
	return task.Resolve(all, ref)
}
