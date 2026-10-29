package sim

import (
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// seedsFor is how many seeds the sweep tests run. The Makefile runs 1000+ with
// the CLI; the unit test keeps `go test ./...` quick.
func seedsFor(t *testing.T, normal, short int) int {
	if v := os.Getenv("SIM_SEEDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			t.Fatalf("SIM_SEEDS=%q", v)
		}
		return n
	}
	if testing.Short() {
		return short
	}
	return normal
}

// The point of the whole package: the same seed is the same run, to the last
// byte of the trace and the last bit of the state hash.
func TestTheSameSeedGivesTheSameRunEveryTime(t *testing.T) {
	for _, seed := range []int64{1, 7, 42, 1234} {
		a, b := Run(seed, Config{}), Run(seed, Config{})
		if a.Failure != nil || b.Failure != nil {
			t.Fatalf("seed %d: %v %v", seed, a.Failure, b.Failure)
		}
		if !reflect.DeepEqual(a.Actions, b.Actions) || !reflect.DeepEqual(a.Trace, b.Trace) || a.Hash != b.Hash || a.Ops != b.Ops || a.Stats != b.Stats {
			t.Fatalf("seed %d gave two different runs:\n%s\n---\n%s", seed, strings.Join(a.Trace, "\n"), strings.Join(b.Trace, "\n"))
		}
	}
}

// Different seeds are different schedules (or the sweep would be one test run
// a thousand times).
func TestDifferentSeedsGiveDifferentSchedulesAndStates(t *testing.T) {
	hashes := map[string]int64{}
	for seed := int64(1); seed <= 40; seed++ {
		r := Run(seed, Config{})
		if r.Failure != nil {
			t.Fatalf("seed %d: %v", seed, r.Failure)
		}
		if prev, dup := hashes[r.Hash]; dup {
			t.Errorf("seeds %d and %d ended in the same state %s", prev, seed, r.Hash[:12])
		}
		hashes[r.Hash] = seed
	}
	if reflect.DeepEqual(Generate(1, Config{}), Generate(2, Config{})) {
		t.Fatal("seeds 1 and 2 gave the same schedule")
	}
}

func TestGenerateIsAPureFunctionOfSeedAndConfigAndRespectsTheShape(t *testing.T) {
	cfg := Config{Replicas: 4, Steps: 50}
	a, b := Generate(9, cfg), Generate(9, cfg)
	if !reflect.DeepEqual(a, b) || len(a) != 50 {
		t.Fatalf("len %d, equal %v", len(a), reflect.DeepEqual(a, b))
	}
	for _, x := range a {
		if x.A < 0 || x.A >= 4 || x.B < 0 || x.B >= 4 {
			t.Fatalf("replica out of range: %v", x)
		}
		if (x.Kind == Sync || x.Kind == Partition || x.Kind == Heal) && x.A == x.B {
			t.Fatalf("%v talks to itself", x)
		}
	}
	if d := Generate(9, Config{}); len(d) != 80 {
		t.Fatalf("default schedule has %d steps, want 80", len(d))
	}
}

// Replaying a schedule from its action list is the same as running its seed:
// that is what makes a printed schedule a bug report.
func TestReplayingTheActionListGivesTheSameRunAsTheSeed(t *testing.T) {
	r := Run(5, Config{})
	again := RunActions(5, r.Actions, Config{})
	if !reflect.DeepEqual(r.Trace, again.Trace) || r.Hash != again.Hash {
		t.Fatal("replay differs from the original run")
	}
}

// A sweep that passes only counts if the faults were really thrown. Over a few
// hundred seeds every kind of fault must have happened, and several different
// failure paths of the store must have been exercised.
func TestTheSweepReallyThrowsEveryKindOfFault(t *testing.T) {
	n := seedsFor(t, 150, 60)
	sum := Many(1, n, 2, Config{})
	if len(sum.Failed) != 0 {
		f := sum.Failed[0]
		t.Fatalf("seed %d: %v\n%s", f.Seed, f.Failure, strings.Join(f.Trace, "\n"))
	}
	s := sum.Total
	for name, v := range map[string]int{
		"edits": s.Edits, "syncs": s.Syncs, "partitions": s.Partitions, "blocked syncs": s.Blocked,
		"clock skews": s.Skews, "crashes": s.Crashes, "dropped replies": s.Dropped,
		"duplicated replies": s.Duplicated, "delayed replies": s.Delayed, "cut replies": s.Truncated,
		"clock-drift refusals": s.DriftRefusals, "refolds of the view": s.Rebuilds,
	} {
		if v == 0 {
			t.Errorf("%d seeds never produced any %s", n, name)
		}
	}
	if sum.Ops == 0 {
		t.Error("no ops were converged")
	}
	t.Logf("%d seeds in %v: %+v", n, sum.Elapsed, s)
}

// Each kind of action appears in a default schedule's worth of seeds.
func TestEveryKindOfActionIsGenerated(t *testing.T) {
	seen := map[Kind]bool{}
	for seed := int64(1); seed <= 30; seed++ {
		for _, a := range Generate(seed, Config{}) {
			seen[a.Kind] = true
		}
	}
	for _, w := range weights {
		if !seen[w.k] {
			t.Errorf("no %s action in 30 schedules", w.k)
		}
	}
}

// Shrink on a made-up failure: the run "fails" when a create on r1 is followed
// later by a crash of r2. The smallest schedule that does is exactly those two
// actions, and Shrink must find it from 80.
func TestShrinkCutsAScheduleDownToTheActionsThatMatter(t *testing.T) {
	acts := Generate(3, Config{})
	fakeRun := func(a []Action) Result {
		r := Result{Seed: 3, Actions: a}
		created := false
		for _, x := range a {
			if x.Kind == Create && x.A == 1 {
				created = true
			}
			if created && x.Kind == Crash && x.A == 2 {
				r.Failure = &Failure{Class: "fake", Msg: "needs both"}
			}
		}
		return r
	}
	// make sure the 80-action schedule fails in the fake sense
	acts = append(acts, Action{Kind: Create, A: 1}, Action{Kind: Crash, A: 2})
	start := fakeRun(acts)
	if start.Failure == nil {
		t.Fatal("setup: the fake failure did not trigger")
	}
	got := shrink(start, fakeRun)
	if len(got.Actions) != 2 || got.Actions[0].Kind != Create || got.Actions[1].Kind != Crash {
		t.Fatalf("shrunk to %d actions: %v", len(got.Actions), got.Actions)
	}
}

func TestShrinkKeepsTheSameKindOfFailureAndNeverReturnsAPassingSchedule(t *testing.T) {
	acts := Generate(11, Config{})
	run := func(a []Action) Result {
		r := Result{Seed: 11, Actions: a}
		n := 0
		for _, x := range a {
			if x.Kind == Sync {
				n++
			}
		}
		switch {
		case n >= 5:
			r.Failure = &Failure{Class: "many-syncs"}
		case n >= 1:
			r.Failure = &Failure{Class: "other"} // a different failure must not count
		}
		return r
	}
	start := run(acts)
	if start.Failure == nil || start.Failure.Class != "many-syncs" {
		t.Fatalf("setup: %+v", start.Failure)
	}
	got := shrink(start, run)
	n := 0
	for _, x := range got.Actions {
		if x.Kind == Sync {
			n++
		}
	}
	if got.Failure == nil || got.Failure.Class != "many-syncs" || n != 5 || len(got.Actions) != 5 {
		t.Fatalf("got %d actions (%d syncs), failure %+v", len(got.Actions), n, got.Failure)
	}
}

func TestShrinkOfAPassingRunReturnsItUnchanged(t *testing.T) {
	r := Run(1, Config{Steps: 10})
	if got := Shrink(r, Config{Steps: 10}); got.Failure != nil || len(got.Actions) != 10 {
		t.Fatalf("%+v", got.Failure)
	}
}

// The files of a run are always removed, unless asked for and the run failed.
func TestARunLeavesNoFilesBehind(t *testing.T) {
	dir := t.TempDir()
	Run(2, Config{Dir: dir})
	left, _ := os.ReadDir(dir)
	if len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
}

func TestEveryReplicaCountFrom2To5Converges(t *testing.T) {
	for n := 2; n <= 5; n++ {
		for seed := int64(1); seed <= 4; seed++ {
			if r := Run(seed, Config{Replicas: n, Steps: 40}); r.Failure != nil {
				t.Fatalf("%d replicas, seed %d: %v", n, seed, r.Failure)
			}
		}
	}
}

func TestSharedStartConverges(t *testing.T) {
	for seed := int64(1); seed <= 20; seed++ {
		if r := Run(seed, Config{Shared: true}); r.Failure != nil {
			t.Fatalf("seed %d: %v\n%s", seed, r.Failure, strings.Join(r.Trace, "\n"))
		}
	}
}

func ExampleRun() {
	r := Run(1, Config{Steps: 3})
	fmt.Println(r.Failure == nil, len(r.Actions))
	// Output: true 3
}

func TestParseActionReadsBackEverythingGenerateMakes(t *testing.T) {
	for seed := int64(1); seed <= 20; seed++ {
		for _, a := range Generate(seed, Config{}) {
			got, err := ParseAction(a.String())
			if err != nil || got != a {
				t.Fatalf("%q -> %+v, %v; want %+v", a.String(), got, err, a)
			}
		}
	}
}

func TestParseActionRefusesGarbage(t *testing.T) {
	for _, bad := range []string{"", "sync", "dance r1 n=3", "create r1", "create r1 n=x", "create r1 n=-4", "create n=3", "sync r1 n=3",
		"sync r1-r1 n=3", "sync r1-rx n=3", "wait r1 n=3", "create r-1 n=3", "create r1 r2 n=3"} {
		if a, err := ParseAction(bad); err == nil {
			t.Errorf("%q was accepted as %+v", bad, a)
		}
	}
}

func TestAnAtomicityFailureIsNamedAsOne(t *testing.T) {
	f := syncFailure("step 3", &notAtomic{"changed"})
	if f.Class != "atomic" || !strings.Contains(f.Msg, "changed") {
		t.Fatalf("%+v", f)
	}
	if g := syncFailure("step 3", fmt.Errorf("boom")); g.Class != "sync" {
		t.Fatalf("%+v", g)
	}
}
