package sqlstore

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"offgrid/internal/task"
)

func TestReplicasListsEveryNodeWithItsCountAndNewestStamp(t *testing.T) {
	ctx := context.Background()
	ca, cb := &clock{t: epoch}, &clock{t: epoch.Add(10 * time.Minute)}
	a := open(t, newDB(t), Options{NodeID: "laptop", Now: ca.now})
	b := open(t, newDB(t), Options{NodeID: "phone", Now: cb.now})

	if got, err := a.Replicas(ctx); err != nil || len(got) != 0 {
		t.Fatalf("an empty log: %v, %v", got, err)
	}
	mk(t, a, "one")
	ca.tick(time.Minute)
	mk(t, a, "two")
	mk(t, b, "three")
	pull(t, a, b, 1000)

	got, err := a.Replicas(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Node != "laptop" || got[1].Node != "phone" { // by name
		t.Fatalf("replicas = %+v", got)
	}
	if got[0].Ops != 2 || got[1].Ops != 1 {
		t.Errorf("ops held = %d and %d, want 2 and 1", got[0].Ops, got[1].Ops)
	}
	if !got[0].Newest.Equal(epoch.Add(time.Minute)) {
		t.Errorf("laptop newest = %v, want %v", got[0].Newest, epoch.Add(time.Minute))
	}
	if !got[1].Newest.Equal(cb.t) {
		t.Errorf("phone newest = %v, want %v", got[1].Newest, cb.t)
	}
}

func TestDescribeSaysWhatEachKindOfOpDid(t *testing.T) {
	op := func(kind, payload string) Op { return Op{Kind: kind, Payload: json.RawMessage(payload)} }
	long := strings.Repeat("x", 100)
	for _, c := range []struct {
		op   Op
		want string
	}{
		{op(KindTaskCreated, `{"id":"T3.phone","title":"Fix  the\npump"}`), `created T3 "Fix the pump"`},
		{op(KindTaskCreated, `{"id":"T3.phone","title":"`+long+`"}`), `created T3 "` + strings.Repeat("x", 59) + `…"`},
		{op(KindTaskUpdated, `{"id":"T3.phone","status":"done","priority":"high"}`), "changed T3: status done, priority high"},
		{op(KindTaskUpdated, `{"id":"T3.phone","priority":"low"}`), "changed T3: priority low"},
		{op(KindNoteAdded, `{"id":"T3.phone","text":"secret"}`), "added a note to T3"},
		{op(KindNoteRemoved, `{"id":"T3.phone"}`), "removed a note from T3"},
		{op(KindTaskRemoved, `{"id":"T3.phone"}`), "deleted T3"},
		{op(KindAnswerRecorded, `{"question":"why is the pump loud?"}`), `recorded an answer to "why is the pump loud?"`},
		{op(KindSuggestionMade, `{"task":"T2.laptop","by":"qwen2.5:7b"}`), "qwen2.5:7b suggested a change to T2"},
		{op(KindSuggestionMade, `{"task":"T2.laptop"}`), "an AI suggested a change to T2"},
		{op(KindSuggestionResolved, `{"resolution":"dismissed"}`), "a suggestion was dismissed"},
		{op("something.new", `{}`), "something.new"},
		{op(KindTaskRemoved, `not json`), "deleted " + task.ShortID("")}, // never fails
	} {
		if got := c.op.Describe(); got != c.want {
			t.Errorf("%s: got %q, want %q", c.op.Kind, got, c.want)
		}
	}
	if strings.Contains(op(KindNoteAdded, `{"text":"secret"}`).Describe(), "secret") {
		t.Error("a note's text must not appear in a one-line description")
	}
}

// mk creates a task and fails the test if that did not work.
func mk(t *testing.T, s *Store, title string) task.Task {
	t.Helper()
	got, _, err := s.Create(context.Background(), task.NewTask{Title: title, Priority: task.PriorityMedium})
	if err != nil {
		t.Fatal(err)
	}
	return got
}
