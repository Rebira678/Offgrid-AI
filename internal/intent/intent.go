// Package intent turns a field worker's free-text message into one typed,
// validated command, using a small language model for the understanding and
// ordinary code for the checking.
//
// The split is the point of Day 3. A model is good at "what does this person
// mean?" and unreliable at "follow this format exactly". So the model proposes
// and this package decides: nothing the model says is trusted until Validate
// has accepted it. (Day 4 extends the rule: the model proposes, the code
// disposes.)
package intent

import (
	"fmt"
	"strings"
)

// Intent is what the worker wants to do.
type Intent string

const (
	CreateTask   Intent = "create_task"
	UpdateTask   Intent = "update_task"
	ListTasks    Intent = "list_tasks"
	AddNote      Intent = "add_note"
	DailySummary Intent = "daily_summary"

	// Unknown is a real, valid answer. Giving the model an honest way to say
	// "this is not something the app does" stops it from forcing "delete
	// everything" or "tell me a joke" into the nearest real intent.
	Unknown Intent = "unknown"
)

// Intents lists every valid intent.
func Intents() []Intent {
	return []Intent{CreateTask, UpdateTask, ListTasks, AddNote, DailySummary, Unknown}
}

// The allowed values of the enumerated fields. An empty string always means
// "not specified" and is allowed in addition to these.
var (
	priorities = []string{"low", "medium", "high"}
	statuses   = []string{"open", "in_progress", "done"}
	whens      = []string{"today", "yesterday", "this_week"}
)

func Priorities() []string { return append([]string(nil), priorities...) }
func Statuses() []string   { return append([]string(nil), statuses...) }
func Whens() []string      { return append([]string(nil), whens...) }

// Command is the model's answer. It is deliberately flat: small models are
// noticeably better at filling in a fixed list of keys than at nested
// structures, and every field is a string so "not applicable" is just "".
type Command struct {
	Intent   Intent `json:"intent"`
	Title    string `json:"title"`
	TaskRef  string `json:"task_ref"`
	Site     string `json:"site"`
	Priority string `json:"priority"`
	Status   string `json:"status"`
	When     string `json:"when"`
	Note     string `json:"note"`
}

// Size limits stop a runaway model from storing a novel in a task title.
const (
	maxTitle   = 120
	maxNote    = 500
	maxTaskRef = 80
	maxSite    = 20
)

// ValidationError lists everything wrong with a command, so the model can be
// told all of it at once instead of one problem per retry.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string { return strings.Join(e.Problems, "; ") }

// Normalize tidies a command in place: trims spaces, lower-cases and
// standardises enumerated values ("In Progress" becomes "in_progress"), and
// removes a leading word "site" from the site ("Site 4" becomes "4"). It
// never invents information, only reshapes what is there.
func (c *Command) Normalize() {
	c.Intent = Intent(enumKey(string(c.Intent)))
	c.Priority = enumKey(c.Priority)
	c.Status = enumKey(c.Status)
	c.When = enumKey(c.When)

	c.Title = strings.TrimSpace(c.Title)
	c.TaskRef = strings.TrimSpace(c.TaskRef)
	c.Note = strings.TrimSpace(c.Note)
	c.Site = strings.TrimSpace(c.Site)
	// Only a separate word is removed: "Site 4" and "site-3" become "4" and
	// "3", but a code like "Siteworks-2" is left alone.
	if len(c.Site) > 4 && strings.EqualFold(c.Site[:4], "site") && strings.ContainsRune(" #:-_", rune(c.Site[4])) {
		c.Site = strings.TrimSpace(strings.TrimLeft(c.Site[4:], " #:-_"))
	}
}

func enumKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.NewReplacer(" ", "_", "-", "_").Replace(s)
}

// Validate normalizes c and then checks that it makes sense for its intent.
// A nil result means the command is safe to act on. Otherwise the error is a
// *ValidationError whose text is written to be read by the model on a retry.
func Validate(c *Command) error {
	c.Normalize()

	var problems []string
	add := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }

	if !validIntent(c.Intent) {
		return &ValidationError{Problems: []string{
			fmt.Sprintf("intent %q is not allowed; use one of: %s", c.Intent, joinIntents(Intents())),
		}}
	}

	checkEnum := func(field, value string, allowed []string) {
		if value != "" && !contains(allowed, value) {
			add("%s %q is not allowed; use one of: %s, or leave it empty", field, value, strings.Join(allowed, ", "))
		}
	}
	checkEnum("priority", c.Priority, priorities)
	checkEnum("status", c.Status, statuses)
	checkEnum("when", c.When, whens)

	checkLen := func(field, value string, max int) {
		if len(value) > max {
			add("%s is too long (%d characters, at most %d)", field, len(value), max)
		}
	}
	checkLen("title", c.Title, maxTitle)
	checkLen("task_ref", c.TaskRef, maxTaskRef)
	checkLen("site", c.Site, maxSite)
	checkLen("note", c.Note, maxNote)

	switch c.Intent {
	case CreateTask:
		if c.Title == "" {
			add("create_task needs a non-empty title")
		}
	case UpdateTask:
		if c.TaskRef == "" {
			add("update_task needs task_ref, the task to change")
		}
		if c.Status == "" && c.Priority == "" {
			add("update_task needs a status or a priority to change")
		}
	case AddNote:
		if c.TaskRef == "" {
			add("add_note needs task_ref, the task to attach the note to")
		}
		if c.Note == "" {
			add("add_note needs a non-empty note")
		}
	}

	if len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}
	return nil
}

func validIntent(i Intent) bool {
	for _, v := range Intents() {
		if v == i {
			return true
		}
	}
	return false
}

func joinIntents(list []Intent) string {
	parts := make([]string, len(list))
	for i, v := range list {
		parts[i] = string(v)
	}
	return strings.Join(parts, ", ")
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
