package agent_test

import (
	"reflect"
	"testing"

	"offgrid/internal/agent"
	"offgrid/internal/intent"
)

func cmd(i intent.Intent, f func(*intent.Command)) intent.Command {
	c := intent.Command{Intent: i}
	if f != nil {
		f(&c)
	}
	return c
}

// Everything here is something the rules must understand, and exactly how.
func TestRulesUnderstandPlainRequests(t *testing.T) {
	cases := []struct {
		text string
		rule string
		want intent.Command
	}{
		// create
		{"new task: replace the gasket", "create-task", cmd(intent.CreateTask, func(c *intent.Command) { c.Title = "replace the gasket" })},
		{"Create task check fire extinguishers, medium priority", "create-task", cmd(intent.CreateTask, func(c *intent.Command) { c.Title = "check fire extinguishers"; c.Priority = "medium" })},
		{"add a task to inspect the fuel line at site 2", "create-task", cmd(intent.CreateTask, func(c *intent.Command) { c.Title = "inspect the fuel line"; c.Site = "2" })},
		{"new ticket: Valve leak, site B7, high priority", "create-task", cmd(intent.CreateTask, func(c *intent.Command) { c.Title = "Valve leak"; c.Site = "B7"; c.Priority = "high" })},
		{"Please add a new job for painting the gate with priority low", "create-task", cmd(intent.CreateTask, func(c *intent.Command) { c.Title = "painting the gate"; c.Priority = "low" })},
		// note
		{"note on T3: the seal is cracked", "add-note", cmd(intent.AddNote, func(c *intent.Command) { c.TaskRef = "T3"; c.Note = "the seal is cracked" })},
		{"add a note to the pump task: back on Thursday", "add-note", cmd(intent.AddNote, func(c *intent.Command) { c.TaskRef = "pump"; c.Note = "back on Thursday" })},
		// update: status
		{"mark T3 as done", "update-status", cmd(intent.UpdateTask, func(c *intent.Command) { c.TaskRef = "T3"; c.Status = "done" })},
		{"set the pump task to in progress", "update-status", cmd(intent.UpdateTask, func(c *intent.Command) { c.TaskRef = "pump"; c.Status = "in_progress" })},
		{"T-12 is now in progress", "update-is-status", cmd(intent.UpdateTask, func(c *intent.Command) { c.TaskRef = "T-12"; c.Status = "in_progress" })},
		{"close the flooded road ticket", "update-verb", cmd(intent.UpdateTask, func(c *intent.Command) { c.TaskRef = "flooded road"; c.Status = "done" })},
		{"reopen T4", "update-verb", cmd(intent.UpdateTask, func(c *intent.Command) { c.TaskRef = "T4"; c.Status = "open" })},
		{"could you start T2 please", "update-verb", cmd(intent.UpdateTask, func(c *intent.Command) { c.TaskRef = "T2"; c.Status = "in_progress" })},
		// update: priority
		{"set the priority of T-7 to low", "update-priority-of", cmd(intent.UpdateTask, func(c *intent.Command) { c.TaskRef = "T-7"; c.Priority = "low" })},
		{"bump the generator task to high priority", "update-priority", cmd(intent.UpdateTask, func(c *intent.Command) { c.TaskRef = "generator"; c.Priority = "high" })},
		// list
		{"list open tasks", "list", cmd(intent.ListTasks, func(c *intent.Command) { c.Status = "open" })},
		{"what's still open?", "list", cmd(intent.ListTasks, func(c *intent.Command) { c.Status = "open" })},
		{"what's still open from yesterday?", "list", cmd(intent.ListTasks, func(c *intent.Command) { c.Status = "open"; c.When = "yesterday" })},
		{"show all the high priority tasks", "list", cmd(intent.ListTasks, func(c *intent.Command) { c.Priority = "high" })},
		{"which tasks got done this week", "list", cmd(intent.ListTasks, func(c *intent.Command) { c.Status = "done"; c.When = "this_week" })},
		{"show my in progress tasks at site 2", "list", cmd(intent.ListTasks, func(c *intent.Command) { c.Status = "in_progress"; c.Site = "2" })},
		{"what is done", "list", cmd(intent.ListTasks, func(c *intent.Command) { c.Status = "done" })}, // a question, not a status change
		{"list tasks", "list", cmd(intent.ListTasks, nil)},
		// summary
		{"summary", "summary", cmd(intent.DailySummary, nil)},
		{"summarize today", "summary", cmd(intent.DailySummary, func(c *intent.Command) { c.When = "today" })},
		{"give me the end of day report", "summary", cmd(intent.DailySummary, func(c *intent.Command) { c.When = "today" })},
		{"weekly summary", "summary", cmd(intent.DailySummary, func(c *intent.Command) { c.When = "this_week" })},
		{"summary of this week please", "summary", cmd(intent.DailySummary, func(c *intent.Command) { c.When = "this_week" })},
		{"what happened yesterday?", "summary-what-happened", cmd(intent.DailySummary, func(c *intent.Command) { c.When = "yesterday" })},
	}
	rules := agent.DefaultRules()
	for _, c := range cases {
		got, rule, ok := rules.Match(c.text)
		if !ok {
			t.Errorf("%q: no rule matched", c.text)
			continue
		}
		if rule != c.rule || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q:\n  got  rule %q %+v\n  want rule %q %+v", c.text, rule, got, c.rule, c.want)
		}
	}
}

// This is the important list. Each message below LOOKS like something a rule
// handles but means something else, or is something the app does not do. The
// rules must say nothing, so the message goes to the model (or is declined).
// A failure here means a rule would silently misread a worker.
func TestRulesStaySilentWhenTheyAreNotSure(t *testing.T) {
	silent := []string{
		// negation and exceptions flip the meaning of a list request
		"show tasks not done",
		"list all tasks except done",
		"what's open but not urgent",
		"show tasks that are not in progress",
		// unfamiliar words
		"list open tasks sorted by site",
		"show me everything for site 7",
		"list tasks last week",
		"show tasks created tomorrow",
		"list urgent tasks", // "urgent" is not a priority we have
		// two conflicting values
		"show open done tasks",
		"list tasks today yesterday",
		"high low priority tasks",
		// not an app request at all
		"what's the weather tomorrow",
		"tell me a joke",
		"delete everything",
		"what do we do",
		"show me the way",
		"hello",
		"",
		"   ",
		// questions are not status changes
		"who is open",
		// pronouns and long phrases are not task references
		"it is done",
		"this is finished",
		"mark everything as done",
		"close all tickets",
		"mark the pump at site 4 as done",
		"close the pump ticket because it leaks",
		"finish the report and then mark T3 done",
		"start the day with coffee and cake for everyone here",
		// "site" is an ordinary word too: only a code with a digit is a site
		"list tasks at site for today",
		"show open tasks at site north",
		"list tasks site",
		"list tasks site !!!",
		"list open tasks at site " + "a1234567890123456789012345",
		// create needs an explicit "task"
		"log a leaking pump at site 4",
		"we need to order more gloves",
		"open task list",
		"add task list",
		"create task",
		"new task:",
		// note needs a colon and a reference
		"add a note to the pump task",
		"note: the gasket is wrong",
		"note on : something",
		// priority needs one of the three words
		"set the priority of T3 to urgent",
		"bump T3 up",
		// summaries with extra meaning
		"summary of site 4",
		"summarize the pump task",
		"what's the summary of everything",
	}
	rules := agent.DefaultRules()
	for _, text := range silent {
		if got, rule, ok := rules.Match(text); ok {
			t.Errorf("%q: rule %q wrongly matched and proposed %+v", text, rule, got)
		}
	}
}

func TestPolitenessAndPunctuationDoNotChangeTheMeaning(t *testing.T) {
	rules := agent.DefaultRules()
	base, _, _ := rules.Match("list open tasks")
	for _, text := range []string{
		"list open tasks", "List open tasks.", "  list   open   tasks  ", "please list open tasks",
		"could you list open tasks?", "list open tasks please!", "Pls list open tasks", "LIST OPEN TASKS",
	} {
		got, _, ok := rules.Match(text)
		if !ok || got != base {
			t.Errorf("%q = %+v, %v; want %+v", text, got, ok, base)
		}
	}
}

func TestTitlesAndNotesKeepTheWorkersOwnWords(t *testing.T) {
	// Lower-casing is for matching only. A title or note reaches the store
	// exactly as written.
	c, _, _ := agent.DefaultRules().Match("NEW TASK: Check the Gate-3 Key, site A4")
	if c.Title != "Check the Gate-3 Key" || c.Site != "A4" {
		t.Errorf("title %q site %q", c.Title, c.Site)
	}
	c, _, _ = agent.DefaultRules().Match("note on T3: Waiting for Mr. Bekele's reply")
	if c.Note != "Waiting for Mr. Bekele's reply" {
		t.Errorf("note %q", c.Note)
	}
}

func TestTheWordSiteInsideATitleIsNotASite(t *testing.T) {
	rules := agent.DefaultRules()
	for text, want := range map[string]intent.Command{
		"new task: check the site visit":                    cmd(intent.CreateTask, func(c *intent.Command) { c.Title = "check the site visit" }),
		"new task: repaint site office, site 4":             cmd(intent.CreateTask, func(c *intent.Command) { c.Title = "repaint site office"; c.Site = "4" }),
		"new task: survey the site entrance, high priority": cmd(intent.CreateTask, func(c *intent.Command) { c.Title = "survey the site entrance"; c.Priority = "high" }),
	} {
		got, _, ok := rules.Match(text)
		if !ok || !reflect.DeepEqual(got, want) {
			t.Errorf("%q:\n  got  %+v (%v)\n  want %+v", text, got, ok, want)
		}
	}
}

func TestEveryNoteAndTitleIsTrimmedAndNonEmpty(t *testing.T) {
	for _, text := range []string{"new task:    ", "new task: ,,", "add a note to T1:   ", "new task: site 4", "new task: high priority"} {
		if got, rule, ok := agent.DefaultRules().Match(text); ok {
			t.Errorf("%q matched %q with %+v; it has no content", text, rule, got)
		}
	}
}

// Whatever a person types, the rules must not panic, and anything they accept
// must name a real intent. (Run longer with: go test -fuzz FuzzRules ./internal/agent)
func FuzzRules(f *testing.F) {
	for _, s := range []string{
		"list open tasks", "new task: x, site 4, high priority", "note on T1: hi", "close T1", "mark a as done",
		"set the priority of T1 to low", "T-1 is done", "what's still open?", "summary", "",
		"\x00\xff", "new task: " + string(make([]byte, 500)), "site site site", "priority priority", ":::", "T-",
	} {
		f.Add(s)
	}
	valid := map[intent.Intent]bool{}
	for _, i := range intent.Intents() {
		valid[i] = true
	}
	rules := agent.DefaultRules()
	f.Fuzz(func(t *testing.T, text string) {
		c, rule, ok := rules.Match(text)
		if !ok {
			return
		}
		if rule == "" || !valid[c.Intent] || c.Intent == intent.Unknown {
			t.Fatalf("%q: rule %q proposed %+v", text, rule, c)
		}
	})
}
