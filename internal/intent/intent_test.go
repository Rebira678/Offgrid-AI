package intent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"offgrid/internal/llm"
	"offgrid/internal/structured"
)

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		cmd     Command
		wantErr string // substring of the error; empty means it must be valid
	}{
		{"create ok", Command{Intent: CreateTask, Title: "Leaking pump", Site: "4", Priority: "high"}, ""},
		{"create without title", Command{Intent: CreateTask, Priority: "high"}, "create_task needs a non-empty title"},
		{"update ok by status", Command{Intent: UpdateTask, TaskRef: "T-12", Status: "done"}, ""},
		{"update ok by priority", Command{Intent: UpdateTask, TaskRef: "T-12", Priority: "low"}, ""},
		{"update without task", Command{Intent: UpdateTask, Status: "done"}, "update_task needs task_ref"},
		{"update without change", Command{Intent: UpdateTask, TaskRef: "T-12"}, "needs a status or a priority"},
		{"list with nothing", Command{Intent: ListTasks}, ""},
		{"list with filters", Command{Intent: ListTasks, Status: "open", When: "yesterday", Site: "2"}, ""},
		{"note ok", Command{Intent: AddNote, TaskRef: "pump", Note: "wrong size"}, ""},
		{"note without text", Command{Intent: AddNote, TaskRef: "pump"}, "add_note needs a non-empty note"},
		{"note without task", Command{Intent: AddNote, Note: "x"}, "add_note needs task_ref"},
		{"summary ok", Command{Intent: DailySummary, When: "this_week"}, ""},
		{"unknown ok", Command{Intent: Unknown}, ""},
		{"made-up intent", Command{Intent: "delete_all"}, `intent "delete_all" is not allowed`},
		{"empty intent", Command{}, "is not allowed"},
		{"bad priority", Command{Intent: CreateTask, Title: "x", Priority: "urgent"}, `priority "urgent" is not allowed`},
		{"bad status", Command{Intent: ListTasks, Status: "finished"}, `status "finished" is not allowed`},
		{"bad when", Command{Intent: DailySummary, When: "tomorrow"}, `when "tomorrow" is not allowed`},
		{"title too long", Command{Intent: CreateTask, Title: strings.Repeat("x", 121)}, "title is too long"},
		{"note too long", Command{Intent: AddNote, TaskRef: "a", Note: strings.Repeat("x", 501)}, "note is too long"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := tc.cmd
			err := Validate(&cmd)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %v, want a ValidationError containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	cmd := Command{Intent: UpdateTask, Priority: "urgent", Status: "finished"}
	err := Validate(&cmd)
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Problems) != 3 {
		t.Fatalf("got %v, want exactly 3 problems listed together (bad priority, bad status, no task_ref)", err)
	}
}

func TestNormalize(t *testing.T) {
	c := Command{
		Intent: " Create_Task ", Title: "  Leaking pump \n", Site: "Site 4",
		Priority: "HIGH", Status: "In Progress", When: "this-week", TaskRef: " T-1 ",
	}
	c.Normalize()
	want := Command{
		Intent: CreateTask, Title: "Leaking pump", Site: "4",
		Priority: "high", Status: "in_progress", When: "this_week", TaskRef: "T-1",
	}
	if c != want {
		t.Errorf("got  %+v\nwant %+v", c, want)
	}

	// The word "site" is removed only when it is a separate word.
	sites := map[string]string{
		"Site 4":      "4",
		"site: 7":     "7",
		"SITE #12":    "12",
		"site-3":      "3",
		"4":           "4",
		"Siteworks-2": "Siteworks-2", // merely starts with the same letters
		"Site":        "Site",
		"  A3 ":       "A3",
	}
	for in, want := range sites {
		c := Command{Site: in}
		c.Normalize()
		if c.Site != want {
			t.Errorf("site %q normalized to %q, want %q", in, c.Site, want)
		}
	}
}

// The three descriptions of the command (the Go struct, the JSON schema sent
// to the model, and the validator) are written separately. This test fails the
// moment they disagree, so a change to one cannot silently break another.
func TestSchemaMatchesStructAndValidator(t *testing.T) {
	var s struct {
		Properties map[string]struct {
			Type string   `json:"type"`
			Enum []string `json:"enum"`
		} `json:"properties"`
		Required             []string `json:"required"`
		AdditionalProperties bool     `json:"additionalProperties"`
	}
	if err := json.Unmarshal(Schema(), &s); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}

	// 1. Same field names as the struct's JSON tags, all required.
	var fields []string
	rt := reflect.TypeOf(Command{})
	for i := 0; i < rt.NumField(); i++ {
		fields = append(fields, strings.Split(rt.Field(i).Tag.Get("json"), ",")[0])
	}
	var props []string
	for k := range s.Properties {
		props = append(props, k)
	}
	sort.Strings(fields)
	sort.Strings(props)
	if !reflect.DeepEqual(fields, props) {
		t.Errorf("schema properties %v differ from struct fields %v", props, fields)
	}
	req := append([]string(nil), s.Required...)
	sort.Strings(req)
	if !reflect.DeepEqual(fields, req) {
		t.Errorf("required %v should list every field %v", req, fields)
	}
	if s.AdditionalProperties {
		t.Error("additionalProperties must be false")
	}

	// 2. Same allowed values as the validator.
	var intents []string
	for _, i := range Intents() {
		intents = append(intents, string(i))
	}
	enums := map[string][]string{
		"intent":   intents,
		"priority": append([]string{""}, priorities...),
		"status":   append([]string{""}, statuses...),
		"when":     append([]string{""}, whens...),
	}
	for field, want := range enums {
		if got := s.Properties[field].Enum; !reflect.DeepEqual(got, want) {
			t.Errorf("%s enum in schema = %v, validator allows %v", field, got, want)
		}
	}

	// 3. Intent comes first in the file, because the model fills keys in order.
	text := string(Schema())
	if strings.Index(text, `"intent"`) > strings.Index(text, `"title"`) {
		t.Error(`"intent" must be listed before the other properties`)
	}
}

func TestEveryExampleInThePromptIsValid(t *testing.T) {
	msgs := Messages("hello")
	for i := 1; i+1 < len(msgs)-1; i += 2 {
		user, reply := msgs[i], msgs[i+1]
		if user.Role != llm.RoleUser || reply.Role != llm.RoleAssistant {
			t.Fatalf("messages %d and %d have roles %s and %s", i, i+1, user.Role, reply.Role)
		}
		var cmd Command
		dec := json.NewDecoder(strings.NewReader(reply.Content))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cmd); err != nil {
			t.Errorf("example %q is not strict-parseable: %v", user.Content, err)
			continue
		}
		if err := Validate(&cmd); err != nil {
			t.Errorf("example %q teaches the model an invalid command: %v", user.Content, err)
		}
	}
}

func TestMessagesShape(t *testing.T) {
	msgs := Messages("fix the pump")
	if msgs[0].Role != llm.RoleSystem {
		t.Error("the conversation must start with the system prompt")
	}
	last := msgs[len(msgs)-1]
	if last.Role != llm.RoleUser || last.Content != "fix the pump" {
		t.Errorf("last message = %+v, want the worker's text as a user message", last)
	}
	if (len(msgs)-2)%2 != 0 {
		t.Error("examples must come in user/assistant pairs")
	}
	// The fixed part must be identical for every message, so the backend can reuse it.
	other := Messages("something else entirely")
	for i := 0; i < len(msgs)-1; i++ {
		if msgs[i] != other[i] {
			t.Fatalf("message %d differs between calls; only the last message may change", i)
		}
	}
}

func TestClassifySettings(t *testing.T) {
	good := `{"intent":"list_tasks","title":"","task_ref":"","site":"","priority":"","status":"open","when":"","note":""}`
	for _, useSchema := range []bool{true, false} {
		f := &llm.Fake{Handler: func(context.Context, llm.Request) (string, error) { return good, nil }}
		c := Classifier{Client: f, Model: "m", UseSchema: useSchema}
		res, err := c.Classify(context.Background(), "what's open?")
		if err != nil {
			t.Fatal(err)
		}
		if res.Value.Intent != ListTasks || res.Value.Status != "open" {
			t.Errorf("value = %+v", res.Value)
		}

		req := f.Calls()[0]
		if (len(req.ResponseSchema) > 0) != useSchema {
			t.Errorf("UseSchema=%v but schema present=%v", useSchema, len(req.ResponseSchema) > 0)
		}
		if req.Temperature != 0 || req.Seed == 0 || req.MaxTokens <= 0 {
			t.Errorf("request = temp %v, seed %d, max %d; want temperature 0, a fixed non-zero seed and a token limit",
				req.Temperature, req.Seed, req.MaxTokens)
		}
	}
}

func TestClassifyRetriesOnInvalidCommand(t *testing.T) {
	replies := []string{
		`{"intent":"create_task","title":"","task_ref":"","site":"","priority":"","status":"","when":"","note":""}`,
		`{"intent":"create_task","title":"Leaking pump","task_ref":"","site":"4","priority":"high","status":"","when":"","note":""}`,
	}
	i := 0
	f := &llm.Fake{Handler: func(context.Context, llm.Request) (string, error) { i++; return replies[i-1], nil }}
	res, err := Classifier{Client: f, Model: "m", UseSchema: true}.Classify(context.Background(), "log a leaking pump at site 4")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Attempts) != 2 || res.Value.Title != "Leaking pump" {
		t.Errorf("attempts = %d, value = %+v", len(res.Attempts), res.Value)
	}
	if !strings.Contains(f.Calls()[1].Messages[len(f.Calls()[1].Messages)-1].Content, "non-empty title") {
		t.Error("the retry should tell the model the title was missing")
	}
}

func TestClassifyGivesUpWithAUsefulError(t *testing.T) {
	f := &llm.Fake{Handler: func(context.Context, llm.Request) (string, error) { return "I am not sure.", nil }}
	_, err := Classifier{Client: f, Model: "m", MaxAttempts: 2}.Classify(context.Background(), "hello")
	if !errors.Is(err, structured.ErrNoValidReply) {
		t.Fatalf("got %v, want ErrNoValidReply", err)
	}
	if len(f.Calls()) != 2 {
		t.Errorf("made %d calls, want 2", len(f.Calls()))
	}
}
