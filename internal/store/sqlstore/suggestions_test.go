package sqlstore

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"offgrid/internal/task"
)

func prio(p task.Priority) *task.Priority { return &p }
func stat(s task.Status) *task.Status     { return &s }

func prioOf(t *testing.T, s *Store, id string) task.Priority {
	t.Helper()
	tk, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return tk.Priority
}

func pending(t *testing.T, s *Store) []task.Suggestion {
	t.Helper()
	l, err := s.Suggestions(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestASuggestionChangesNothingInTheTask(t *testing.T) {
	ctx := context.Background()
	a, aclk := replicaAt(t, "node-a", epoch)
	create(t, a, "pump")
	aclk.tick(time.Minute)
	before, _ := a.Get(ctx, "T1.node-a")
	opsBefore, _ := a.Ops(ctx, 0, 100)
	sg, created, err := a.Suggest(ctx, task.Draft{Task: "T1.node-a", Priority: prio(task.PriorityHigh), Note: " check the seal ", By: "qwen2.5:7b", Key: "job-1"})
	if err != nil || !created {
		t.Fatalf("%v %v", err, created)
	}
	after, _ := a.Get(ctx, "T1.node-a")
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("the task changed:\nbefore %+v\nafter  %+v", before, after)
	}
	if ops, _ := a.Ops(ctx, 0, 100); len(ops) != len(opsBefore)+1 {
		t.Fatalf("ops %d -> %d", len(opsBefore), len(ops))
	}
	if sg.ID != "node-a.2" || sg.State != task.SuggestionPending || sg.Stale || sg.Note != "check the seal" ||
		sg.BasePriority != before.Priority || sg.BaseStatus != before.Status || *sg.Priority != task.PriorityHigh || sg.By != "qwen2.5:7b" {
		t.Fatalf("%+v", sg)
	}
	mustVerify(t, a)
}

func TestSuggestingTwiceWithTheSameKeyKeepsOne(t *testing.T) {
	ctx := context.Background()
	a, _ := replicaAt(t, "node-a", epoch)
	create(t, a, "pump")
	d := task.Draft{Task: "T1.node-a", Priority: prio(task.PriorityHigh), By: "m", Key: "k"}
	first, c1, _ := a.Suggest(ctx, d)
	again, c2, err := a.Suggest(ctx, d)
	if err != nil || !c1 || c2 || again.ID != first.ID || len(pending(t, a)) != 1 {
		t.Fatalf("%v created=%v/%v ids %s %s", err, c1, c2, first.ID, again.ID)
	}
	// no key: every call is its own suggestion
	d.Key = ""
	a.Suggest(ctx, d)
	a.Suggest(ctx, d)
	if len(pending(t, a)) != 3 {
		t.Fatalf("%d pending", len(pending(t, a)))
	}
}

func TestSuggestRefusesNothingAndUnknownTasks(t *testing.T) {
	ctx := context.Background()
	a, _ := replicaAt(t, "node-a", epoch)
	create(t, a, "pump")
	if _, _, err := a.Suggest(ctx, task.Draft{Task: "T1.node-a", Note: "   "}); !errors.Is(err, task.ErrNothingToSuggest) {
		t.Fatalf("%v", err)
	}
	if _, _, err := a.Suggest(ctx, task.Draft{Task: "T9", Priority: prio(task.PriorityLow)}); !errors.Is(err, task.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if _, _, err := a.Suggest(ctx, task.Draft{Task: "T1.node-a", Priority: prio("urgent")}); err == nil {
		t.Fatal("an invalid priority was recorded")
	}
	if len(pending(t, a)) != 0 {
		t.Fatal("a refused suggestion left a row")
	}
}

func TestAcceptingAFreshSuggestionAppliesItOnceAndDismissingChangesNothing(t *testing.T) {
	ctx := context.Background()
	a, aclk := replicaAt(t, "node-a", epoch)
	create(t, a, "pump")
	sg, _, _ := a.Suggest(ctx, task.Draft{Task: "T1.node-a", Status: stat(task.StatusDone), Priority: prio(task.PriorityHigh), Note: "seal replaced", By: "m"})
	aclk.tick(time.Minute)
	tk, err := a.Resolve(ctx, sg.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if tk.Status != task.StatusDone || tk.Priority != task.PriorityHigh || len(tk.Notes) != 1 || tk.Notes[0].Text != "seal replaced" || tk.DoneAt.IsZero() {
		t.Fatalf("%+v", tk)
	}
	if _, err := a.Resolve(ctx, sg.ID, true); !errors.Is(err, task.ErrSuggestionResolved) {
		t.Fatalf("a second accept: %v", err)
	}
	if _, err := a.Resolve(ctx, sg.ID, false); !errors.Is(err, task.ErrSuggestionResolved) {
		t.Fatalf("dismissing an accepted one: %v", err)
	}
	all, _ := a.Suggestions(ctx, true)
	if len(all) != 1 || all[0].State != task.SuggestionAccepted || len(pending(t, a)) != 0 {
		t.Fatalf("%+v", all)
	}

	create(t, a, "valve")
	sg2, _, _ := a.Suggest(ctx, task.Draft{Task: "T2.node-a", Priority: prio(task.PriorityLow), By: "m"})
	h := hashOf(t, a)
	before, _ := a.Get(ctx, "T2.node-a")
	tk, err = a.Resolve(ctx, sg2.ID, false)
	if err != nil || fmt.Sprint(tk) != fmt.Sprint(before) {
		t.Fatalf("dismiss changed the task: %v\n%+v\n%+v", err, before, tk)
	}
	if hashOf(t, a) == h {
		t.Fatal("dismissing is a fact and must show in the state hash")
	}
	mustVerify(t, a)
}

func TestResolveRefusesBadIDs(t *testing.T) {
	ctx := context.Background()
	a, _ := replicaAt(t, "node-a", epoch)
	create(t, a, "pump")
	for _, id := range []string{"", "node-a", "node-a.", ".3", "node-a.x", "node-a.0", "node-a.99", "bad node.2"} {
		if _, err := a.Resolve(ctx, id, true); !errors.Is(err, task.ErrSuggestionNotFound) {
			t.Errorf("%q: %v", id, err)
		}
	}
	if n, s, err := splitSuggestionID("n.1.7"); err != nil || n != "n.1" || s != 7 {
		t.Errorf("dotted node: %q %d %v", n, s, err)
	}
}

// The roadmap's test: a human edit made offline survives an AI job that finishes later.
func TestAHumanEditMadeOfflineSurvivesAnAIJobFinishingLater(t *testing.T) {
	ctx := context.Background()
	a, b, aclk, bclk := pair(t) // a: the person's phone. b: the laptop whose AI job runs.
	aclk.tick(time.Minute)
	setPriority(t, a, "T1.node-a", task.PriorityHigh) // the person, offline
	bclk.tick(5 * time.Minute)
	sg, _, err := b.Suggest(ctx, task.Draft{Task: "T1.node-a", Priority: prio(task.PriorityLow), By: "qwen2.5:7b"}) // finishes later, knows nothing
	if err != nil {
		t.Fatal(err)
	}
	meet(t, a, b)
	for _, s := range []*Store{a, b} {
		if got := prioOf(t, s, "T1.node-a"); got != task.PriorityHigh {
			t.Fatalf("%s: the AI overwrote a human: priority is %s", s.node, got)
		}
		l := pending(t, s)
		if len(l) != 1 || !l[0].Stale {
			t.Fatalf("%s: the suggestion should be pending and stale: %+v", s.node, l)
		}
		if _, err := s.Resolve(ctx, sg.ID, true); !errors.Is(err, task.ErrStale) {
			t.Fatalf("%s: accepting a stale suggestion: %v", s.node, err)
		}
		if got := prioOf(t, s, "T1.node-a"); got != task.PriorityHigh {
			t.Fatalf("%s: a refused accept still changed the task", s.node)
		}
	}
	// the person dismisses it on one device; both agree
	if _, err := a.Resolve(ctx, sg.ID, false); err != nil {
		t.Fatal(err)
	}
	meet(t, a, b)
	if len(pending(t, b)) != 0 || prioOf(t, b, "T1.node-a") != task.PriorityHigh {
		t.Fatal("after dismissing, devices disagree")
	}
}

func TestASuggestionMadeBeforeAHumanEditIsStaleToo(t *testing.T) {
	ctx := context.Background()
	a, aclk := replicaAt(t, "node-a", epoch)
	create(t, a, "pump")
	sg, _, _ := a.Suggest(ctx, task.Draft{Task: "T1.node-a", Priority: prio(task.PriorityLow), By: "m"})
	aclk.tick(time.Minute)
	setPriority(t, a, "T1.node-a", task.PriorityHigh)
	if _, err := a.Resolve(ctx, sg.ID, true); !errors.Is(err, task.ErrStale) {
		t.Fatalf("%v", err)
	}
	// a field the suggestion does not touch does not make it stale
	sg2, _, _ := a.Suggest(ctx, task.Draft{Task: "T1.node-a", Note: "just a note", By: "m"})
	aclk.tick(time.Minute)
	setPriority(t, a, "T1.node-a", task.PriorityLow)
	if l := pending(t, a); len(l) != 2 || l[0].Stale == false || l[1].Stale {
		t.Fatalf("%+v", l)
	}
	if _, err := a.Resolve(ctx, sg2.ID, true); err != nil {
		t.Fatalf("a note suggestion should still apply: %v", err)
	}
}

func TestAcceptingOnOneDeviceReachesTheOther(t *testing.T) {
	ctx := context.Background()
	a, b, aclk, _ := pair(t)
	sg, _, _ := b.Suggest(ctx, task.Draft{Task: "T1.node-a", Status: stat(task.StatusInProgress), Note: "started", By: "m"})
	meet(t, a, b)
	aclk.tick(time.Minute)
	if _, err := a.Resolve(ctx, sg.ID, true); err != nil {
		t.Fatal(err)
	}
	meet(t, a, b)
	tk, _ := b.Get(ctx, "T1.node-a")
	if tk.Status != task.StatusInProgress || len(tk.Notes) != 1 {
		t.Fatalf("%+v", tk)
	}
	all, _ := b.Suggestions(ctx, true)
	if len(all) != 1 || all[0].State != task.SuggestionAccepted {
		t.Fatalf("%+v", all)
	}
}

func TestTwoDevicesResolvingTheSameSuggestionDifferentlyAgree(t *testing.T) {
	ctx := context.Background()
	for _, acceptFirst := range []bool{true, false} {
		a, b, aclk, bclk := pair(t)
		sg, _, _ := a.Suggest(ctx, task.Draft{Task: "T1.node-a", Priority: prio(task.PriorityHigh), By: "m"})
		pull(t, b, a, 100)
		first, second := a, b
		fc, sc := aclk, bclk
		if !acceptFirst {
			first, second, fc, sc = b, a, bclk, aclk
		}
		fc.tick(time.Minute)
		if _, err := first.Resolve(ctx, sg.ID, true); err != nil {
			t.Fatal(err)
		}
		sc.tick(2 * time.Minute)
		if _, err := second.Resolve(ctx, sg.ID, false); err != nil {
			t.Fatal(err)
		}
		meet(t, a, b)
		for _, s := range []*Store{a, b} {
			all, _ := s.Suggestions(ctx, true)
			if len(all) != 1 || all[0].State != task.SuggestionAccepted || prioOf(t, s, "T1.node-a") != task.PriorityHigh {
				t.Fatalf("acceptFirst=%v %s: the earlier resolution must stand: %+v", acceptFirst, s.node, all)
			}
		}
	}
}

func TestSuggestionsSurviveARebuildAndAnOutOfOrderArrival(t *testing.T) {
	ctx := context.Background()
	a, b, aclk, bclk := pair(t)
	aclk.tick(time.Minute)
	sg, _, _ := a.Suggest(ctx, task.Draft{Task: "T1.node-a", Priority: prio(task.PriorityHigh), By: "m"})
	bclk.tick(time.Second) // b's edit sorts BEFORE a's suggestion
	setPriority(t, b, "T1.node-a", task.PriorityLow)
	pull(t, a, b, 100) // an older op arrives: forces a refold
	h := hashOf(t, a)
	if _, err := a.Rebuild(ctx); err != nil || hashOf(t, a) != h {
		t.Fatalf("rebuild changed the state: %v", err)
	}
	meet(t, a, b)
	if l := pending(t, b); len(l) != 1 || l[0].ID != sg.ID {
		t.Fatalf("%+v", l)
	}
}

func TestAnAcceptedSuggestionOnADeletedTaskIsRefused(t *testing.T) {
	ctx := context.Background()
	a, _ := replicaAt(t, "node-a", epoch)
	create(t, a, "pump")
	sg, _, _ := a.Suggest(ctx, task.Draft{Task: "T1.node-a", Priority: prio(task.PriorityHigh), By: "m"})
	if err := a.DeleteTask(ctx, "T1.node-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Resolve(ctx, sg.ID, true); !errors.Is(err, task.ErrNotFound) {
		t.Fatalf("%v", err)
	}
}

func TestRandomHumansAndAIsAlwaysConvergeAndAIsNeverChangeATaskDirectly(t *testing.T) {
	ctx := context.Background()
	for seed := int64(1); seed <= 12; seed++ {
		rng := rand.New(rand.NewSource(seed))
		var reps []*Store
		var clks []*clock
		for _, n := range []string{"node-a", "node-b", "node-c"} {
			s, c := replicaAt(t, n, epoch.Add(time.Duration(rng.Intn(120)-60)*time.Second))
			reps, clks = append(reps, s), append(clks, c)
		}
		create(t, reps[0], "seed task")
		for _, s := range reps[1:] {
			pull(t, s, reps[0], 100)
		}
		humanPriority := map[string]bool{} // priorities a human wrote, per task
		_ = humanPriority
		for step := 0; step < 70; step++ {
			i := rng.Intn(3)
			s, c := reps[i], clks[i]
			c.tick(time.Duration(rng.Intn(5)) * time.Second)
			all, _ := s.List(ctx, task.Filter{})
			tk := all[rng.Intn(len(all))]
			ps := []task.Priority{task.PriorityLow, task.PriorityMedium, task.PriorityHigh}
			switch rng.Intn(7) {
			case 0, 1:
				setPriority(t, s, tk.ID, ps[rng.Intn(3)])
			case 2, 3: // an AI proposes: the task must be exactly as it was
				before, _ := s.Get(ctx, tk.ID)
				if _, _, err := s.Suggest(ctx, task.Draft{Task: tk.ID, Priority: prio(ps[rng.Intn(3)]), By: "ai"}); err != nil {
					t.Fatal(err)
				}
				if after, _ := s.Get(ctx, tk.ID); fmt.Sprint(before) != fmt.Sprint(after) {
					t.Fatalf("seed %d: a suggestion changed a task", seed)
				}
			case 4: // a person resolves one
				if l := pending(t, s); len(l) > 0 {
					sg := l[rng.Intn(len(l))]
					_, err := s.Resolve(ctx, sg.ID, rng.Intn(2) == 0)
					if err != nil && !errors.Is(err, task.ErrStale) {
						t.Fatalf("seed %d: %v", seed, err)
					}
				}
			default:
				if j := rng.Intn(3); j != i {
					pull(t, s, reps[j], 1+rng.Intn(5))
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
		for i, s := range reps {
			if hashOf(t, s) != h {
				t.Fatalf("seed %d: replica %d disagrees", seed, i)
			}
			mustVerify(t, s)
		}
		if _, err := reps[2].Rebuild(ctx); err != nil || hashOf(t, reps[2]) != h {
			t.Fatalf("seed %d: rebuild changed the state", seed)
		}
	}
}

func TestAcceptingASuggestionCountsAsAnEditSoAConcurrentDeleteDoesNotKillTheTask(t *testing.T) {
	ctx := context.Background()
	a, b, aclk, bclk := pair(t)
	aclk.tick(time.Minute)
	sg, _, _ := a.Suggest(ctx, task.Draft{Task: "T1.node-a", Priority: prio(task.PriorityHigh), By: "m"})
	pull(t, b, a, 100)
	bclk.tick(5 * time.Minute)
	if _, err := b.Resolve(ctx, sg.ID, true); err != nil { // a person accepts on b ...
		t.Fatal(err)
	}
	aclk.tick(6 * time.Minute)
	if err := a.DeleteTask(ctx, "T1.node-a"); err != nil { // ... while another person deletes on a, not having seen that
		t.Fatal(err)
	}
	meet(t, a, b)
	for _, s := range []*Store{a, b} {
		if got := prioOf(t, s, "T1.node-a"); got != task.PriorityHigh {
			t.Fatalf("%s: an accepted edit the deleter had not seen must keep the task alive, got %q", s.node, got)
		}
	}
}

func TestMalformedIncomingSuggestionOpsAreRefusedAndKeepNothing(t *testing.T) {
	ctx := context.Background()
	for name, op := range map[string]Op{
		"suggests nothing":       {NodeID: "node-z", Seq: 2, Kind: KindSuggestionMade, Payload: []byte(`{"task":"T1.node-a","base_status":"open","base_priority":"medium","by":"m"}`)},
		"a blank note":           {NodeID: "node-z", Seq: 2, Kind: KindSuggestionMade, Payload: []byte(`{"task":"T1.node-a","note":"  ","base_status":"open","base_priority":"medium","by":"m"}`)},
		"bad priority":           {NodeID: "node-z", Seq: 2, Kind: KindSuggestionMade, Payload: []byte(`{"task":"T1.node-a","priority":"urgent","base_status":"open","base_priority":"medium","by":"m"}`)},
		"bad base":               {NodeID: "node-z", Seq: 2, Kind: KindSuggestionMade, Payload: []byte(`{"task":"T1.node-a","priority":"low","base_status":"??","base_priority":"medium","by":"m"}`)},
		"unknown task":           {NodeID: "node-z", Seq: 2, Kind: KindSuggestionMade, Payload: []byte(`{"task":"T404","priority":"low","base_status":"open","base_priority":"medium","by":"m"}`)},
		"unknown field":          {NodeID: "node-z", Seq: 2, Kind: KindSuggestionMade, Payload: []byte(`{"task":"T1.node-a","priority":"low","base_status":"open","base_priority":"medium","by":"m","apply":true}`)},
		"resolves a ghost":       {NodeID: "node-z", Seq: 2, Kind: KindSuggestionResolved, Payload: []byte(`{"node":"node-q","seq":9,"resolution":"accepted"}`)},
		"unknown resolution":     {NodeID: "node-z", Seq: 2, Kind: KindSuggestionResolved, Payload: []byte(`{"node":"node-q","seq":9,"resolution":"maybe"}`)},
		"resolution without seq": {NodeID: "node-z", Seq: 2, Kind: KindSuggestionResolved, Payload: []byte(`{"node":"node-q","seq":0,"resolution":"accepted"}`)},
	} {
		a, _ := replicaAt(t, "node-a", epoch)
		create(t, a, "pump") // T1
		src, _ := replicaAt(t, "node-z", epoch)
		pull(t, src, a, 100)
		create(t, src, "other") // gives node-z a seq 1 op so seq 2 is contiguous
		ops, _, _ := src.OpsSince(ctx, Summary{}, 10)
		if _, err := a.ApplyRemote(ctx, ops); err != nil {
			t.Fatal(err)
		}
		op.TS = epoch.Add(time.Hour)
		n := countOps(t, a)
		if _, err := a.ApplyRemote(ctx, []Op{op}); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if countOps(t, a) != n || len(pending(t, a)) != 0 {
			t.Errorf("%s: a refused op left something behind", name)
		}
	}
}
