package sqlstore

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"offgrid/internal/task"
)

func TestClipTextBoundaryAndUnicode(t *testing.T) {
	for name, tc := range map[string]struct {
		input string
		n     int
		want  string
	}{
		"empty":                {"", 10, ""},
		"zero n":               {"hello", 0, ""},
		"negative n":           {"hello", -5, ""},
		"n=1 short":            {"a", 1, "a"},
		"n=1 long":             {"hello", 1, "…"},
		"n=2":                  {"hello", 2, "h…"},
		"exact length":         {"hello", 5, "hello"},
		"longer than n":        {"hello world", 8, "hello w…"},
		"extra whitespace":     {"  hello   world  ", 20, "hello world"},
		"multiline":            {"hello\n\tworld", 20, "hello world"},
		"unicode runes":        {"世界你好！", 4, "世界你…"},
		"emoji handling":       {"Hello 🌍🚀🎉", 8, "Hello 🌍…"},
		"combining characters": {"café latte", 6, "café …"},
	} {
		t.Run(name, func(t *testing.T) {
			got := clipText(tc.input, tc.n)
			if got != tc.want {
				t.Errorf("clipText(%q, %d) = %q, want %q", tc.input, tc.n, got, tc.want)
			}
		})
	}
}

func TestDescribeAllOpKindsAndMalformedPayloads(t *testing.T) {
	cases := []struct {
		name string
		op   Op
		want string
	}{
		{
			name: "malformed json payload",
			op:   Op{Kind: KindTaskCreated, Payload: []byte("{not json")},
			want: `created  ""`,
		},
		{
			name: "empty json payload",
			op:   Op{Kind: KindTaskCreated, Payload: []byte("{}")},
			want: `created  ""`,
		},
		{
			name: "task created with title",
			op:   Op{Kind: KindTaskCreated, Payload: []byte(`{"id":"t-123456","title":"Fix leak"}`)},
			want: `created t-123456 "Fix leak"`,
		},
		{
			name: "task updated with only title",
			op:   Op{Kind: KindTaskUpdated, Payload: []byte(`{"id":"t-123456","title":"New title"}`)},
			want: `changed t-123456: `,
		},
		{
			name: "task updated with status and priority",
			op:   Op{Kind: KindTaskUpdated, Payload: []byte(`{"id":"t-123456","status":"done","priority":"high"}`)},
			want: `changed t-123456: status done, priority high`,
		},
		{
			name: "task updated with nothing",
			op:   Op{Kind: KindTaskUpdated, Payload: []byte(`{"id":"t-123456"}`)},
			want: `changed t-123456: `,
		},
		{
			name: "note added and removed",
			op:   Op{Kind: KindNoteAdded, Payload: []byte(`{"id":"t-123456"}`)},
			want: `added a note to t-123456`,
		},
		{
			name: "note removed without id",
			op:   Op{Kind: KindNoteRemoved, Payload: []byte(`{"task":"t-123456"}`)},
			want: `removed a note from `,
		},
		{
			name: "task removed",
			op:   Op{Kind: KindTaskRemoved, Payload: []byte(`{"id":"t-123456"}`)},
			want: `deleted t-123456`,
		},
		{
			name: "answer recorded with long question",
			op:   Op{Kind: KindAnswerRecorded, Payload: []byte(`{"question":"What is the meaning of the universe and everything else in it?"}`)},
			want: `recorded an answer to "What is the meaning of the universe and…"`,
		},
		{
			name: "suggestion made with author",
			op:   Op{Kind: KindSuggestionMade, Payload: []byte(`{"task":"t-123456","by":"llama3"}`)},
			want: `llama3 suggested a change to t-123456`,
		},
		{
			name: "suggestion made without author",
			op:   Op{Kind: KindSuggestionMade, Payload: []byte(`{"task":"t-123456"}`)},
			want: `an AI suggested a change to t-123456`,
		},
		{
			name: "suggestion resolved with resolution",
			op:   Op{Kind: KindSuggestionResolved, Payload: []byte(`{"resolution":"accepted"}`)},
			want: `a suggestion was accepted`,
		},
		{
			name: "suggestion resolved without resolution",
			op:   Op{Kind: KindSuggestionResolved, Payload: []byte(`{}`)},
			want: `a suggestion was `,
		},
		{
			name: "unknown kind returns raw kind",
			op:   Op{Kind: "custom.event.v1", Payload: []byte(`{"foo":"bar"}`)},
			want: `custom.event.v1`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.op.Describe()
			if got != tc.want {
				t.Errorf("Describe() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReplicasEmptyDB(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(context.Background(), filepath.Join(dir, "empty.db"), Options{NodeID: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	reps, err := st.Replicas(context.Background())
	if err != nil {
		t.Fatalf("Replicas() error = %v", err)
	}
	if len(reps) != 0 {
		t.Errorf("expected 0 replicas, got %d", len(reps))
	}
}

func TestReplicasMultiNodeOrderingAndTimestamps(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	st, err := Open(context.Background(), filepath.Join(dir, "multi.db"), Options{NodeID: "node-a", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	_, _, err = st.Create(ctx, task.NewTask{Title: "Task 1", Priority: task.PriorityHigh})
	if err != nil {
		t.Fatal(err)
	}

	// Apply ops from remote nodes
	ts1 := now.Add(time.Second).UnixNano()
	ts2 := now.Add(2 * time.Second).UnixNano()
	remoteOps := []Op{
		{NodeID: "node-z", Seq: 1, TS: fromNanos(ts1), Kind: KindTaskCreated, Payload: []byte(`{"id":"T1.node-z","title":"Z task","site":"","priority":"medium"}`)},
		{NodeID: "node-b", Seq: 1, TS: fromNanos(ts1), Kind: KindTaskCreated, Payload: []byte(`{"id":"T1.node-b","title":"B task","site":"","priority":"medium"}`)},
		{NodeID: "node-b", Seq: 2, TS: fromNanos(ts2), Kind: KindTaskUpdated, Payload: []byte(`{"id":"T1.node-b","status":"done"}`)},
	}
	if _, err := st.ApplyRemote(ctx, remoteOps); err != nil {
		t.Fatal(err)
	}

	reps, err := st.Replicas(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(reps) != 3 {
		t.Fatalf("expected 3 replicas, got %d: %+v", len(reps), reps)
	}
	// Replicas are ORDER BY node_id: node-a, node-b, node-z
	if reps[0].Node != "node-a" || reps[1].Node != "node-b" || reps[2].Node != "node-z" {
		t.Errorf("replicas not ordered by node_id: %+v", reps)
	}
	if reps[1].Ops != 2 {
		t.Errorf("node-b ops = %d, want 2", reps[1].Ops)
	}
	if reps[2].Ops != 1 {
		t.Errorf("node-z ops = %d, want 1", reps[2].Ops)
	}
	if !reps[1].Newest.Equal(fromNanos(ts2)) {
		t.Errorf("node-b newest = %v, want %v", reps[1].Newest, fromNanos(ts2))
	}
}
