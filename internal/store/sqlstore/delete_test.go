package sqlstore

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	"offgrid/internal/crdt"
	"offgrid/internal/task"
)

func ids(t *testing.T, s *Store) []string {
	t.Helper()
	l, err := s.List(context.Background(), task.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, tk := range l {
		out = append(out, tk.ID)
	}
	return out
}

func shownNotes(t *testing.T, s *Store, id string) []string {
	t.Helper()
	tk, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("%s: Get %s: %v", s.node, id, err)
	}
	out := []string{}
	for _, n := range tk.Notes {
		out = append(out, n.Text)
	}
	return out
}

// pair opens replicas a and b that already share one task, T1.
func pair(t *testing.T) (a, b *Store, aclk, bclk *clock) {
	t.Helper()
	a, aclk = replicaAt(t, "node-a", epoch)
	b, bclk = replicaAt(t, "node-b", epoch)
	create(t, a, "pump")
	pull(t, b, a, 100)
	return
}

func meet(t *testing.T, a, b *Store) {
	t.Helper()
	pull(t, a, b, 100)
	pull(t, b, a, 100)
	pull(t, a, b, 100)
	if hashOf(t, a) != hashOf(t, b) {
		t.Fatal("replicas differ after meeting")
	}
	mustVerify(t, a)
	mustVerify(t, b)
}

func TestADeleteTheOtherSideHadSeenStaysDeletedOnBoth(t *testing.T) {
	ctx := context.Background()
	a, b, aclk, bclk := pair(t)
	bclk.tick(time.Minute)
	setStatusOf(t, b, "T1.node-a", task.StatusInProgress)
	pull(t, a, b, 100) // a has seen b's edit
	aclk.tick(time.Minute)
	if err := a.DeleteTask(ctx, "T1.node-a"); err != nil {
		t.Fatal(err)
	}
	meet(t, a, b)
	if got := ids(t, a); len(got) != 0 {
		t.Fatalf("tasks after meeting = %v, want none", got)
	}
}

func TestEditingATaskWhileAnotherReplicaDeletesItKeepsTheTask(t *testing.T) {
	ctx := context.Background()
	a, b, aclk, bclk := pair(t)
	// offline: a deletes T1; b, not knowing, edits it AND adds a note
	aclk.tick(2 * time.Minute)
	if err := a.DeleteTask(ctx, "T1.node-a"); err != nil {
		t.Fatal(err)
	}
	bclk.tick(time.Minute) // b's edit is EARLIER than a's delete: timestamps alone would lose it
	setStatusOf(t, b, "T1.node-a", task.StatusDone)
	if _, err := b.AddNote(ctx, "T1.node-a", "still needed"); err != nil {
		t.Fatal(err)
	}
	if len(ids(t, a)) != 0 {
		t.Fatal("a should see the task as deleted before meeting")
	}
	meet(t, a, b)
	for _, s := range []*Store{a, b} {
		tk, err := s.Get(ctx, "T1.node-a")
		if err != nil || tk.Status != task.StatusDone || len(tk.Notes) != 1 {
			t.Fatalf("%s: T1 = %+v, %v; want it alive with b's edit and note", s.node, tk, err)
		}
	}
}

func TestADeleteThatMissedAnEditLosesToItButADeleteThatSawEverythingWins(t *testing.T) {
	ctx := context.Background()
	a, b, aclk, bclk := pair(t)
	aclk.tick(time.Minute)
	if err := a.DeleteTask(ctx, "T1.node-a"); err != nil {
		t.Fatal(err)
	}
	bclk.tick(time.Minute)
	setStatusOf(t, b, "T1.node-a", task.StatusDone)
	meet(t, a, b) // alive again: b's edit was unseen
	if len(ids(t, a)) != 1 {
		t.Fatal("T1 should be alive after meeting")
	}
	// now a deletes again, this time having seen everything
	aclk.tick(time.Minute)
	if err := a.DeleteTask(ctx, "T1.node-a"); err != nil {
		t.Fatal(err)
	}
	meet(t, a, b)
	if len(ids(t, a)) != 0 || len(ids(t, b)) != 0 {
		t.Fatalf("after a delete that saw everything: a=%v b=%v, want none", ids(t, a), ids(t, b))
	}
}

func TestTwoReplicasDeletingTheSameTaskIsFine(t *testing.T) {
	ctx := context.Background()
	a, b, aclk, bclk := pair(t)
	aclk.tick(time.Minute)
	bclk.tick(2 * time.Minute)
	if err := a.DeleteTask(ctx, "T1.node-a"); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteTask(ctx, "T1.node-a"); err != nil {
		t.Fatal(err)
	}
	meet(t, a, b)
	if len(ids(t, a)) != 0 {
		t.Fatal("both deleted it; it should be gone")
	}
	// an edit unseen by ONE of the two deletes is not enough: it must be unseen by all
	c, cclk := replicaAt(t, "node-c", epoch)
	pull(t, c, a, 100)
	cclk.tick(time.Hour)
	_ = c.Clock()
	if _, _, err := c.Update(ctx, "T1.node-a", task.Changes{Status: ptr(task.StatusDone)}); err == nil {
		t.Fatal("c holds the deletes, so T1 is gone for it and cannot be updated")
	}
}

func ptr[T any](v T) *T { return &v }

func TestRemovingANoteRemovesOnlyThatNoteEvenIfAnotherIsAddedAtTheSameTime(t *testing.T) {
	ctx := context.Background()
	a, b, aclk, bclk := pair(t)
	for _, n := range []string{"first", "second", "third"} {
		aclk.tick(time.Second)
		if _, err := a.AddNote(ctx, "T1.node-a", n); err != nil {
			t.Fatal(err)
		}
	}
	pull(t, b, a, 100)
	aclk.tick(time.Minute)
	if _, err := a.RemoveNote(ctx, "T1.node-a", 2); err != nil { // "second"
		t.Fatal(err)
	}
	bclk.tick(time.Minute)
	if _, err := b.AddNote(ctx, "T1.node-a", "from b"); err != nil {
		t.Fatal(err)
	}
	meet(t, a, b)
	want := "first,third,from b"
	if got := strings.Join(shownNotes(t, a, "T1.node-a"), ","); got != want {
		t.Fatalf("notes = %q, want %q", got, want)
	}
	// the same text added again later is a new note and is shown
	aclk.tick(time.Minute)
	a.AddNote(ctx, "T1.node-a", "second")
	meet(t, a, b)
	if got := strings.Join(shownNotes(t, b, "T1.node-a"), ","); got != want+",second" {
		t.Fatalf("notes = %q", got)
	}
}

func TestBothReplicasRemovingTheSameNoteIsFine(t *testing.T) {
	ctx := context.Background()
	a, b, aclk, _ := pair(t)
	aclk.tick(time.Second)
	a.AddNote(ctx, "T1.node-a", "only")
	pull(t, b, a, 100)
	a.RemoveNote(ctx, "T1.node-a", 1)
	b.RemoveNote(ctx, "T1.node-a", 1)
	meet(t, a, b)
	if n := shownNotes(t, a, "T1.node-a"); len(n) != 0 {
		t.Fatalf("notes = %v, want none", n)
	}
}

func TestIncomingRemovalsThatPointAtNothingAreRefused(t *testing.T) {
	ctx := context.Background()
	b, _ := replicaAt(t, "node-b", epoch)
	mk := func(kind, payload string) []Op {
		return []Op{{NodeID: "node-z", Seq: 1, TS: epoch, Kind: kind, Payload: []byte(payload)}}
	}
	for name, ops := range map[string][]Op{
		"delete of a missing task": mk(KindTaskRemoved, `{"id":"T9","seen":{}}`),
		"note removal, no task":    mk(KindNoteRemoved, `{"id":"T9","node":"n","seq":1}`),
		"bad seen entry":           mk(KindTaskRemoved, `{"id":"T9","seen":{"bad node!":1}}`),
		"negative seen":            mk(KindTaskRemoved, `{"id":"T9","seen":{"n":-1}}`),
		"unknown field":            mk(KindTaskRemoved, `{"id":"T9","seen":{},"x":1}`),
		"note removal seq 0":       mk(KindNoteRemoved, `{"id":"T9","node":"n","seq":0}`),
	} {
		if _, err := b.ApplyRemote(ctx, ops); !errors.Is(err, ErrBadOp) {
			t.Errorf("%s: err = %v, want ErrBadOp", name, err)
		}
	}
	if countOps(t, b) != 0 {
		t.Fatal("a refused op was kept")
	}
	// a note removal whose note exists on the task but under another task id
	a, _, _, _ := pair(t)
	a.AddNote(ctx, "T1.node-a", "x")
	create(t, a, "other")
	op := []Op{{NodeID: "node-z", Seq: 1, TS: epoch.Add(time.Hour), Kind: KindNoteRemoved, Payload: []byte(`{"id":"T2.node-a","node":"node-a","seq":2}`)}}
	if _, err := a.ApplyRemote(ctx, op); !errors.Is(err, ErrBadOp) {
		t.Fatalf("removing task T1's note through task T2: %v, want ErrBadOp", err)
	}
}

func TestTheOldProjectionIsRebuiltOnOpenAndDeletesSurviveARebuild(t *testing.T) {
	ctx := context.Background()
	path := newDB(t)
	clk := &clock{t: epoch}
	s, err := Open(ctx, path, Options{NodeID: "node-a", Now: clk.now})
	if err != nil {
		t.Fatal(err)
	}
	create(t, s, "pump")
	create(t, s, "filter")
	s.AddNote(ctx, "T1.node-a", "a")
	s.AddNote(ctx, "T1.node-a", "b")
	s.RemoveNote(ctx, "T1.node-a", 1)
	s.DeleteTask(ctx, "T2.node-a")
	h := hashOf(t, s)
	// pretend an older program built the view
	if err := setMeta(ctx, s.db, "projection_version", "1"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2 := open(t, path, Options{NodeID: "node-a", Now: clk.now})
	if hashOf(t, s2) != h {
		t.Fatal("the rebuilt view differs")
	}
	if got := ids(t, s2); len(got) != 1 || got[0] != "T1.node-a" || strings.Join(shownNotes(t, s2, "T1.node-a"), ",") != "b" {
		t.Fatalf("after reopening: tasks %v", got)
	}
	mustVerify(t, s2)
}

// aliveOracle decides which tasks exist from the op list alone, without any of
// the store's tables: a task is shown unless every touching op was seen by some
// delete... precisely: shown if no delete exists, or some op that touched it was
// unseen by every delete.
func aliveOracle(ops []Op) map[string]bool {
	type touch struct {
		node string
		seq  int64
	}
	touches := map[string][]touch{}
	seens := map[string][]map[string]int64{}
	for _, op := range ops {
		switch op.Kind {
		case KindTaskCreated:
			var p createdPayload
			mustDecode(op.Payload, &p)
			touches[p.ID] = append(touches[p.ID], touch{op.NodeID, op.Seq})
		case KindTaskUpdated:
			var p updatedPayload
			mustDecode(op.Payload, &p)
			touches[p.ID] = append(touches[p.ID], touch{op.NodeID, op.Seq})
		case KindNoteAdded:
			var p notePayload
			mustDecode(op.Payload, &p)
			touches[p.ID] = append(touches[p.ID], touch{op.NodeID, op.Seq})
		case KindTaskRemoved:
			var p removedPayload
			mustDecode(op.Payload, &p)
			seens[p.ID] = append(seens[p.ID], p.Seen)
		}
	}
	out := map[string]bool{}
	for id, ts := range touches {
		alive := len(seens[id]) == 0
		for _, tc := range ts {
			unseen := true
			for _, sn := range seens[id] {
				if tc.seq <= sn[tc.node] {
					unseen = false
				}
			}
			if unseen {
				alive = true
			}
		}
		out[id] = alive
	}
	return out
}

func mustDecode(raw []byte, v any) {
	if err := decodeStrict(raw, v); err != nil {
		panic(err)
	}
}

func TestRandomReplicasWithDeletesConvergeAndMatchAnIndependentOracle(t *testing.T) {
	ctx := context.Background()
	deletes, revived := 0, 0
	for seed := int64(1); seed <= 25; seed++ {
		rng := rand.New(rand.NewSource(seed))
		var reps []*Store
		var clks []*clock
		for i, n := range []string{"node-a", "node-b", "node-c"} {
			s, c := replicaAt(t, n, epoch.Add(time.Duration(rng.Intn(120)-60)*time.Second))
			_ = i
			reps, clks = append(reps, s), append(clks, c)
		}
		create(t, reps[0], "seed task")
		for _, s := range reps[1:] {
			pull(t, s, reps[0], 100)
		}
		for step := 0; step < 90; step++ {
			i := rng.Intn(3)
			s, c := reps[i], clks[i]
			c.tick(time.Duration(rng.Intn(4)) * time.Second)
			all, _ := s.List(ctx, task.Filter{})
			pickTask := func() (task.Task, bool) {
				if len(all) == 0 {
					return task.Task{}, false
				}
				return all[rng.Intn(len(all))], true
			}
			switch rng.Intn(9) {
			case 0:
				create(t, s, fmt.Sprintf("t-%d-%d", i, step))
			case 1:
				if tk, ok := pickTask(); ok {
					setStatusOf(t, s, tk.ID, statuses[rng.Intn(3)])
				}
			case 2, 3:
				if tk, ok := pickTask(); ok {
					if _, err := s.AddNote(ctx, tk.ID, fmt.Sprintf("note %d", step)); err != nil {
						t.Fatal(err)
					}
				}
			case 4:
				if tk, ok := pickTask(); ok && len(tk.Notes) > 0 {
					if _, err := s.RemoveNote(ctx, tk.ID, 1+rng.Intn(len(tk.Notes))); err != nil {
						t.Fatal(err)
					}
				}
			case 5:
				if tk, ok := pickTask(); ok {
					if err := s.DeleteTask(ctx, tk.ID); err != nil {
						t.Fatal(err)
					}
					deletes++
				}
			default:
				if j := rng.Intn(3); j != i {
					pull(t, s, reps[j], 1+rng.Intn(6))
				}
			}
		}
		for round := 0; round < 2; round++ {
			for i := range reps {
				for j := range reps {
					if i != j {
						pull(t, reps[i], reps[j], 7)
					}
				}
			}
		}
		h := hashOf(t, reps[0])
		all := allOps(t, reps[0])
		oracle := aliveOracle(all)
		notes, err := reps[0].Notes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		orset := crdt.TaskNotes(notes)
		for i, s := range reps {
			if hashOf(t, s) != h {
				t.Fatalf("seed %d: replica %d disagrees", seed, i)
			}
			mustVerify(t, s)
			// the view agrees with the independent oracle about which tasks exist
			var want []string
			for id, alive := range oracle {
				if alive {
					want = append(want, id)
				}
			}
			sort.Strings(want)
			got := ids(t, s)
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("seed %d replica %d: tasks %v, oracle says %v", seed, i, got, want)
			}
			// ...and about which notes each shown task has (the OR-Set computes them from the ops)
			for _, id := range got {
				var texts []string
				for _, n := range orset[id] {
					texts = append(texts, n.Text)
				}
				if strings.Join(shownNotes(t, s, id), "|") != strings.Join(texts, "|") {
					t.Fatalf("seed %d replica %d task %s: notes %v, OR-Set says %v", seed, i, id, shownNotes(t, s, id), texts)
				}
			}
		}
		for _, alive := range oracle {
			if !alive {
				continue
			}
		}
		// count tasks that were deleted at some point and are shown anyway
		for id, alive := range oracle {
			var removed bool
			for _, op := range all {
				if op.Kind == KindTaskRemoved && strings.Contains(string(op.Payload), `"id":"`+id+`"`) {
					removed = true
				}
			}
			if removed && alive {
				revived++
			}
		}
		if _, err := reps[1].Rebuild(ctx); err != nil || hashOf(t, reps[1]) != h {
			t.Fatalf("seed %d: a rebuild changed the state (%v)", seed, err)
		}
	}
	// the test must have exercised what it claims to
	if deletes < 30 || revived < 3 {
		t.Fatalf("only %d deletes and %d tasks that survived a delete: the workload proves too little", deletes, revived)
	}
}
