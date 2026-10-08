package intent

import (
	"encoding/json"

	"offgrid/internal/llm"
)

// schema is the JSON Schema sent to Ollama, which constrains the model so it
// cannot produce anything else. It is written out by hand, not built from a Go
// map, for one reason: Go maps lose their key order, and here the order
// matters. "intent" comes first, so the model decides what the message means
// before it fills in the details. A test checks this text stays in step with
// the Command struct and the validator, so the three cannot drift apart.
const schema = `{
  "type": "object",
  "properties": {
    "intent":   {"type": "string", "enum": ["create_task", "update_task", "list_tasks", "add_note", "daily_summary", "unknown"]},
    "title":    {"type": "string"},
    "task_ref": {"type": "string"},
    "site":     {"type": "string"},
    "priority": {"type": "string", "enum": ["", "low", "medium", "high"]},
    "status":   {"type": "string", "enum": ["", "open", "in_progress", "done"]},
    "when":     {"type": "string", "enum": ["", "today", "yesterday", "this_week"]},
    "note":     {"type": "string"}
  },
  "required": ["intent", "title", "task_ref", "site", "priority", "status", "when", "note"],
  "additionalProperties": false
}`

// Schema returns the JSON Schema for a Command.
func Schema() json.RawMessage { return json.RawMessage(schema) }

// The prompt puts everything that never changes first and the worker's message
// last. Ollama reuses the processed start of a prompt it has just seen, so the
// long fixed part is read once and every later request only pays for the new
// message. Reordering this would make every call slower.
const systemPrompt = `You turn one message from a field worker into one JSON command for a task-tracking app.
Reply with a single JSON object and nothing else.

Intents:
- create_task: the worker reports a new job or problem to log. Set title; optionally site and priority.
- update_task: change the status or priority of an existing task. Set task_ref, and status and/or priority.
- list_tasks: show existing tasks. Optionally filter by status, site, priority or when.
- add_note: attach a note to an existing task. Set task_ref and note.
- daily_summary: the worker wants a summary or report. Optionally set when.
- unknown: anything else, including things this app cannot do (weather, jokes, deleting data).

Always include every field, using "" when a field does not apply:
{"intent":"","title":"","task_ref":"","site":"","priority":"","status":"","when":"","note":""}

Allowed values: priority is low, medium or high. status is open, in_progress or done. when is today, yesterday or this_week.
site is only the number or code, for example "4". task_ref identifies an existing task, for example "T-12" or "the pump task".
Only fill in what the message says. Never invent values.`

type example struct {
	text string
	cmd  Command
}

// examples are shown to the model as earlier turns of the conversation. They
// teach the format by demonstration, which works far better on small models
// than description alone. None of them may appear in the evaluation set, or
// the test would measure memory instead of understanding; a test enforces it.
var examples = []example{
	{"log a broken ladder at the depot, medium priority",
		Command{Intent: CreateTask, Title: "Broken ladder at the depot", Priority: "medium"}},
	{"T-3 is finished",
		Command{Intent: UpdateTask, TaskRef: "T-3", Status: "done"}},
	{"show open tasks at site 9",
		Command{Intent: ListTasks, Site: "9", Status: "open"}},
	{"add note to the valve task: part arrives Monday",
		Command{Intent: AddNote, TaskRef: "the valve task", Note: "part arrives Monday"}},
	{"what is the capital of France?",
		Command{Intent: Unknown}},
	{"rundown of how today went",
		Command{Intent: DailySummary, When: "today"}},
}

// ExampleTexts returns the worker messages used as examples in the prompt.
func ExampleTexts() []string {
	out := make([]string, len(examples))
	for i, e := range examples {
		out[i] = e.text
	}
	return out
}

// Messages builds the full conversation for one worker message: the fixed
// instructions, the worked examples, then the message itself.
func Messages(text string) []llm.Message {
	msgs := []llm.Message{{Role: llm.RoleSystem, Content: systemPrompt}}
	for _, e := range examples {
		// Marshalling a Command cannot fail: it only contains strings.
		reply, _ := json.Marshal(e.cmd)
		msgs = append(msgs,
			llm.Message{Role: llm.RoleUser, Content: e.text},
			llm.Message{Role: llm.RoleAssistant, Content: string(reply)},
		)
	}
	return append(msgs, llm.Message{Role: llm.RoleUser, Content: text})
}
