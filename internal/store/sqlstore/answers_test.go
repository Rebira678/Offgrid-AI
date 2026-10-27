package sqlstore

import (
	"context"
	"strings"
	"testing"
	"time"
)

func answersOf(t *testing.T, s *Store) []Answer {
	t.Helper()
	a, err := s.Answers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestRecordingAnAnswerTwiceWritesOneOp(t *testing.T) {
	ctx := context.Background()
	a, _ := replicaAt(t, "node-a", epoch)
	create(t, a, "pump")
	before, _ := a.Ops(ctx, 0, 100)
	ok, err := a.RecordAnswer(ctx, "k1", "what is open?", "T1 is open.", "qwen2.5:7b", "local")
	if err != nil || !ok {
		t.Fatalf("first record: ok=%v err=%v", ok, err)
	}
	ok, err = a.RecordAnswer(ctx, "k1", "what is open?", "different words", "qwen2.5:7b", "local")
	if err != nil || ok {
		t.Fatalf("second record: ok=%v err=%v, want no write", ok, err)
	}
	after, _ := a.Ops(ctx, 0, 100)
	if len(after) != len(before)+1 {
		t.Fatalf("ops %d -> %d, want exactly one new op", len(before), len(after))
	}
	got := answersOf(t, a)
	if len(got) != 1 || got[0].Text != "T1 is open." || got[0].Source != "local" || got[0].Model != "qwen2.5:7b" {
		t.Fatalf("answers = %+v", got)
	}
	mustVerify(t, a)
}

func TestAnAnswerNeverTouchesTasks(t *testing.T) {
	ctx := context.Background()
	a, _ := replicaAt(t, "node-a", epoch)
	create(t, a, "pump")
	h := hashOf(t, a)
	tasks := ids(t, a)
	if _, err := a.RecordAnswer(ctx, "k", "q", "an answer", "m", "cloud"); err != nil {
		t.Fatal(err)
	}
	if got := ids(t, a); strings.Join(got, ",") != strings.Join(tasks, ",") {
		t.Fatalf("tasks changed: %v -> %v", tasks, got)
	}
	if hashOf(t, a) == h {
		t.Fatal("the state hash ignores answers: replicas could differ without noticing")
	}
}

func TestAnswersSyncAndTwoReplicasAnsweringTheSameQuestionAgreeOnTheFirst(t *testing.T) {
	ctx := context.Background()
	a, b, aclk, bclk := pair(t)
	aclk.tick(time.Minute)
	bclk.tick(2 * time.Minute) // b answers later
	if _, err := a.RecordAnswer(ctx, "same", "q", "from a", "m", "local"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.RecordAnswer(ctx, "same", "q", "from b", "m", "cloud"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.RecordAnswer(ctx, "only-b", "q2", "b alone", "m", "cloud"); err != nil {
		t.Fatal(err)
	}
	meet(t, a, b)
	for _, s := range []*Store{a, b} {
		got := answersOf(t, s)
		if len(got) != 2 {
			t.Fatalf("%s holds %d answers, want 2: %+v", s.node, len(got), got)
		}
		by := map[string]string{}
		for _, x := range got {
			by[x.Key] = x.Text
		}
		if by["same"] != "from a" || by["only-b"] != "b alone" {
			t.Fatalf("%s: %v (the earlier answer must win on both)", s.node, by)
		}
	}
}

func TestAnAnswerArrivingOutOfOrderStillConvergesAndRebuildKeepsIt(t *testing.T) {
	ctx := context.Background()
	a, b, aclk, bclk := pair(t)
	aclk.tick(time.Minute)
	bclk.tick(time.Hour)
	if _, err := b.RecordAnswer(ctx, "k", "q", "late", "m", "cloud"); err != nil {
		t.Fatal(err)
	}
	pull(t, a, b, 100)
	if _, err := a.RecordAnswer(ctx, "k2", "q", "x", "m", "local"); err != nil { // a already knows k only via b
		t.Fatal(err)
	}
	meet(t, a, b)
	before := hashOf(t, a)
	if _, err := a.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	if hashOf(t, a) != before {
		t.Fatal("a rebuild changed the state: answers are not derived purely from the log")
	}
	if len(answersOf(t, a)) != 2 {
		t.Fatalf("answers after rebuild: %+v", answersOf(t, a))
	}
}

func TestMalformedAnswersAreRefused(t *testing.T) {
	ctx := context.Background()
	a, _ := replicaAt(t, "node-a", epoch)
	if _, err := a.RecordAnswer(ctx, "", "q", "text", "m", "local"); err == nil {
		t.Fatal("empty key accepted")
	}
	if _, err := a.RecordAnswer(ctx, "k", "q", "  ", "m", "local"); err == nil {
		t.Fatal("empty text accepted")
	}
	if _, err := a.RecordAnswer(ctx, "k", "q", "text", "m", "somewhere"); err == nil {
		t.Fatal("unknown source accepted")
	}
	if len(answersOf(t, a)) != 0 {
		t.Fatal("a refused answer left a row behind")
	}
}

func TestADatabaseWithoutAnswersKeepsItsOldHash(t *testing.T) {
	// The hash of a store with tasks and no answers must not depend on the
	// answers feature existing: replicas of different ages must still agree.
	a, _ := replicaAt(t, "node-a", epoch)
	create(t, a, "pump")
	raw, err := stateHashLegacy(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if raw != hashOf(t, a) {
		t.Fatalf("hash changed for a store with no answers: %s vs %s", raw, hashOf(t, a))
	}
}

// stateHashLegacy is the Day 20 formula, written out independently: a plain
// JSON array of tasks.
func stateHashLegacy(ctx context.Context, s *Store) (string, error) {
	return legacyHash(ctx, s)
}

func TestAnswersAreListedOldestFirstAndInTheSameOrderOnEveryReplica(t *testing.T) {
	ctx := context.Background()
	a, b, aclk, bclk := pair(t)
	aclk.tick(time.Minute)
	bclk.tick(2 * time.Minute)
	// b's answer has the LOWER sequence number but the LATER time.
	if _, err := b.RecordAnswer(ctx, "y", "q", "second in time", "m", "local"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.RecordAnswer(ctx, "x", "q", "first in time", "m", "local"); err != nil {
		t.Fatal(err)
	}
	meet(t, a, b)
	for _, s := range []*Store{a, b} {
		got := answersOf(t, s)
		if len(got) != 2 || got[0].Key != "x" || got[1].Key != "y" {
			t.Fatalf("%s lists %+v, want x (earlier) then y", s.node, got)
		}
	}
}
