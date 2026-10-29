package sim

import (
	"reflect"
	"testing"
)

// TestShrinkAlreadyMinimalSchedule verifies that if a failing schedule cannot be shrunk
// any further, Shrink returns it intact without endless looping or corruption.
func TestShrinkAlreadyMinimalSchedule(t *testing.T) {
	minimalActs := []Action{
		{Kind: Create, A: 0, N: 1},
		{Kind: Crash, A: 0, N: 2},
	}
	failingResult := Result{
		Seed:    99,
		Actions: minimalActs,
		Failure: &Failure{Class: "exact-pair", Msg: "requires both create and crash"},
	}

	testRun := func(a []Action) Result {
		r := Result{Seed: 99, Actions: a}
		hasCreate, hasCrash := false, false
		for _, x := range a {
			if x.Kind == Create {
				hasCreate = true
			}
			if x.Kind == Crash {
				hasCrash = true
			}
		}
		if hasCreate && hasCrash {
			r.Failure = &Failure{Class: "exact-pair", Msg: "both present"}
		}
		return r
	}

	shrunk := shrink(failingResult, testRun)
	if len(shrunk.Actions) != 2 || !reflect.DeepEqual(shrunk.Actions, minimalActs) {
		t.Fatalf("shrink changed an already minimal schedule: got %+v, want %+v", shrunk.Actions, minimalActs)
	}
}

// TestShrinkIdempotency verifies that shrinking an already shrunk result produces
// an identical schedule.
func TestShrinkIdempotency(t *testing.T) {
	acts := Generate(12, Config{Steps: 40})
	testRun := func(a []Action) Result {
		r := Result{Seed: 12, Actions: a}
		syncCount := 0
		for _, x := range a {
			if x.Kind == Sync {
				syncCount++
			}
		}
		if syncCount >= 3 {
			r.Failure = &Failure{Class: "sync-threshold", Msg: "too many syncs"}
		}
		return r
	}

	start := testRun(acts)
	if start.Failure == nil {
		t.Fatal("setup: initial run did not fail")
	}

	firstShrunk := shrink(start, testRun)
	secondShrunk := shrink(firstShrunk, testRun)

	if !reflect.DeepEqual(firstShrunk.Actions, secondShrunk.Actions) {
		t.Fatalf("shrink is not idempotent:\n1st: %+v\n2nd: %+v", firstShrunk.Actions, secondShrunk.Actions)
	}
}
