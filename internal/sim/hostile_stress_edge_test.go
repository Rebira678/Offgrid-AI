package sim

import (
	"fmt"
	"math/rand"
	"testing"
)

func BenchmarkMaskDevices(b *testing.B) {
	devA := "dev_0123456789abcdef0123456789abcdef"
	devB := "dev_fedcba9876543210fedcba9876543210"

	sampleTrace := []string{
		fmt.Sprintf("r0 enrolled %s", devA),
		fmt.Sprintf("r1 received op from %s", devB),
		fmt.Sprintf("r0 synced with %s again", devA),
		"unrelated trace line",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res := Result{
			Trace: append([]string(nil), sampleTrace...),
			Failure: &Failure{
				Class: "test",
				Msg:   fmt.Sprintf("failure on %s", devA),
			},
		}
		maskDevices(&res)
	}
}

func BenchmarkSecurePrelude80Steps(b *testing.B) {
	cfg := Config{Replicas: 3, Steps: 80, Secure: true}
	rng := rand.New(rand.NewSource(42))
	base := make([]Action, 80)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		acts := append([]Action(nil), base...)
		securePrelude(rng, acts, cfg)
	}
}

func BenchmarkSecurePrelude200Steps(b *testing.B) {
	cfg := Config{Replicas: 3, Steps: 200, Secure: true}
	rng := rand.New(rand.NewSource(42))
	base := make([]Action, 200)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		acts := append([]Action(nil), base...)
		securePrelude(rng, acts, cfg)
	}
}

// TestHostileScheduleSeedsZeroAndNegative verifies that non-positive seeds
// generate deterministic, valid secure schedules without crashing.
func TestHostileScheduleSeedsZeroAndNegative(t *testing.T) {
	cfg := Config{Secure: true, Steps: 40, Replicas: 3}
	for _, seed := range []int64{0, -1, -42, -9999} {
		acts1 := Generate(seed, cfg)
		acts2 := Generate(seed, cfg)

		if len(acts1) != 40 || len(acts2) != 40 {
			t.Fatalf("seed %d: got length %d / %d, want 40", seed, len(acts1), len(acts2))
		}

		for i := range acts1 {
			if acts1[i] != acts2[i] {
				t.Fatalf("seed %d: action mismatch at %d: %+v vs %+v", seed, i, acts1[i], acts2[i])
			}
		}

		var hasHostile bool
		for _, a := range acts1 {
			if isHostile(a.Kind) {
				hasHostile = true
				break
			}
		}
		if !hasHostile {
			t.Fatalf("seed %d: generated schedule contains no hostile actions", seed)
		}
	}
}

// TestHostileShrinkOnRealHostileFailure verifies that shrinking a failing secure schedule
// produces minimal actions and preserves the hostile actions responsible for the failure.
func TestHostileShrinkOnRealHostileFailure(t *testing.T) {
	cfg := Config{Secure: true, Steps: 50, Replicas: 3}
	acts := Generate(12345, cfg)

	predicate := func(as []Action) Result {
		var hasRevoke, hasRogue bool
		for _, a := range as {
			if a.Kind == Revoke {
				hasRevoke = true
			}
			if a.Kind == Rogue {
				hasRogue = true
			}
		}
		res := Result{Seed: 12345, Actions: as}
		if hasRevoke && hasRogue {
			res.Failure = &Failure{Class: "rogue-after-revoke", Msg: "rogue write executed"}
		}
		return res
	}

	initial := predicate(acts)
	if initial.Failure == nil {
		t.Fatal("initial schedule did not trigger simulated failure")
	}

	shrunk := shrink(initial, predicate)
	if shrunk.Failure == nil {
		t.Fatal("shrunk schedule does not fail")
	}
	if len(shrunk.Actions) != 2 {
		t.Fatalf("shrunk to %d actions, want 2: %v", len(shrunk.Actions), shrunk.Actions)
	}

	var hasRevoke, hasRogue bool
	for _, a := range shrunk.Actions {
		if a.Kind == Revoke {
			hasRevoke = true
		}
		if a.Kind == Rogue {
			hasRogue = true
		}
	}
	if !hasRevoke || !hasRogue {
		t.Fatalf("shrunk actions missing key hostile actions: %v", shrunk.Actions)
	}
}

// FuzzParseActionHostile tests action parsing robustness against arbitrary strings.
func FuzzParseActionHostile(f *testing.F) {
	f.Add("revoke r0 1")
	f.Add("forge r1 42")
	f.Add("rogue r2 100")
	f.Add("replay r0 5")
	f.Add("sync r0 r1")
	f.Add("edit r0 1")
	f.Add("crash r1")
	f.Add("malformed hostile string")
	f.Add("")

	f.Fuzz(func(t *testing.T, s string) {
		a, err := ParseAction(s)
		if err == nil {
			// If it parsed successfully, formatting it and parsing back must succeed
			formatted := a.String()
			reparsed, err2 := ParseAction(formatted)
			if err2 != nil || reparsed != a {
				t.Fatalf("round-trip parse failure for %q: got %+v, %v", formatted, reparsed, err2)
			}
		}
	})
}
