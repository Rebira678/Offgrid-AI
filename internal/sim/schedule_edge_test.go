package sim

import (
	"reflect"
	"testing"
)

// TestScheduleGenerationDistributionWeights verifies that actions are distributed
// across all 12 kinds roughly according to their relative weights over a large sample.
func TestScheduleGenerationDistributionWeights(t *testing.T) {
	cfg := Config{Replicas: 4, Steps: 1000}
	acts := Generate(777, cfg)
	if len(acts) != 1000 {
		t.Fatalf("expected 1000 actions, got %d", len(acts))
	}

	counts := map[Kind]int{}
	for _, a := range acts {
		counts[a.Kind]++
	}

	// Verify all 12 action kinds occurred
	for _, w := range weights {
		if counts[w.k] == 0 {
			t.Errorf("action %s never generated in 1000 steps", w.k)
		}
	}

	// Sync should be the most frequent action by weight (weight=30)
	if counts[Sync] < counts[Create] || counts[Sync] < counts[Crash] {
		t.Errorf("Sync count (%d) was not dominant as expected by weight", counts[Sync])
	}
}

// TestScheduleReplicaIndexConstraints ensures every action respects replica bounds
// and pair actions never target the same replica.
func TestScheduleReplicaIndexConstraints(t *testing.T) {
	for numReplicas := 2; numReplicas <= 8; numReplicas++ {
		cfg := Config{Replicas: numReplicas, Steps: 100}
		acts := Generate(int64(numReplicas*100), cfg)

		for i, a := range acts {
			if a.A < 0 || a.A >= numReplicas {
				t.Fatalf("numReplicas=%d action %d: A=%d out of bounds [0, %d)", numReplicas, i, a.A, numReplicas)
			}
			if a.Kind == Partition || a.Kind == Heal || a.Kind == Sync {
				if a.B < 0 || a.B >= numReplicas {
					t.Fatalf("numReplicas=%d action %d: B=%d out of bounds [0, %d)", numReplicas, i, a.B, numReplicas)
				}
				if a.A == a.B {
					t.Fatalf("numReplicas=%d action %d: self-pair A == B (%d == %d)", numReplicas, i, a.A, a.B)
				}
			}
			if a.Kind == Wait && a.A != 0 {
				t.Fatalf("Wait action has non-zero A (%d)", a.A)
			}
		}
	}
}

// TestNegativeAndZeroSeedsGenerateDeterministically tests that negative or zero seeds
// work without crashing and generate identical schedules when repeated.
func TestNegativeAndZeroSeedsGenerateDeterministically(t *testing.T) {
	cfg := Config{Replicas: 3, Steps: 30}
	for _, seed := range []int64{0, -1, -999, -123456789} {
		a := Generate(seed, cfg)
		b := Generate(seed, cfg)
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("seed %d did not generate identically on repeated runs", seed)
		}
	}
}
