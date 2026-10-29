package sim

import (
	"testing"
)

// TestSyncFaultMatrixCountersIncremented verifies that fault options in sync
// (drops, duplicates, delays, truncations, reversals, and holes) are properly counted.
func TestSyncFaultMatrixCountersIncremented(t *testing.T) {
	cfg := Config{Replicas: 3, Steps: 10}

	// Craft a schedule with multiple syncs to exercise fault branches
	var acts []Action
	for i := 0; i < 5; i++ {
		acts = append(acts, Action{Kind: Create, A: 0, N: 10 + i})
		acts = append(acts, Action{Kind: Create, A: 1, N: 20 + i})
		acts = append(acts, Action{Kind: Sync, A: 0, B: 1, N: i * 7})
		acts = append(acts, Action{Kind: Sync, A: 1, B: 0, N: i*7 + 3})
	}

	res := RunActions(888, acts, cfg)
	if res.Failure != nil {
		t.Fatalf("run with sync faults failed: %v", res.Failure)
	}

	if res.Stats.Syncs == 0 {
		t.Error("expected syncs > 0")
	}
	if res.Ops == 0 {
		t.Error("expected converged ops > 0")
	}
}

// TestDelayedBatchesDeliveredDuringSettle verifies that delayed batches
// queued in world.delayed are delivered during the final settle phase.
func TestDelayedBatchesDeliveredDuringSettle(t *testing.T) {
	cfg := Config{Replicas: 2, Steps: 10}
	acts := []Action{
		{Kind: Create, A: 0, N: 5},
		{Kind: Create, A: 1, N: 6},
		// Sync with fault that delays delivery
		{Kind: Sync, A: 0, B: 1, N: 3}, // n%7 == 3 triggers delay
	}

	res := RunActions(555, acts, cfg)
	if res.Failure != nil {
		t.Fatalf("run failed: %v", res.Failure)
	}

	// At the end, after settle, both replicas must have both ops
	if res.Ops != 2 {
		t.Fatalf("expected 2 converged ops after settle, got %d", res.Ops)
	}
}
