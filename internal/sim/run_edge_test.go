package sim

import (
	"strings"
	"testing"
)

// TestRunActionsMinimalSchedules tests running schedules of 1 or 2 actions.
func TestRunActionsMinimalSchedules(t *testing.T) {
	cfg := Config{Replicas: 3, Steps: 2}

	// 1. Single create
	rCreate := RunActions(1, []Action{{Kind: Create, A: 0, N: 10}}, cfg)
	if rCreate.Failure != nil {
		t.Fatalf("single create failed: %v", rCreate.Failure)
	}
	if len(rCreate.Trace) < 2 {
		t.Fatalf("trace too short: %v", rCreate.Trace)
	}

	// 2. Single skew
	rSkew := RunActions(2, []Action{{Kind: Skew, A: 1, N: 50}}, cfg)
	if rSkew.Failure != nil {
		t.Fatalf("single skew failed: %v", rSkew.Failure)
	}

	// 3. Single crash
	rCrash := RunActions(3, []Action{{Kind: Crash, A: 2, N: 10}}, cfg)
	if rCrash.Failure != nil {
		t.Fatalf("single crash failed: %v", rCrash.Failure)
	}

	// 4. Partition and Heal
	rPartHeal := RunActions(4, []Action{
		{Kind: Partition, A: 0, B: 1, N: 0},
		{Kind: Heal, A: 0, B: 1, N: 0},
	}, cfg)
	if rPartHeal.Failure != nil {
		t.Fatalf("partition-heal failed: %v", rPartHeal.Failure)
	}

	// 5. Wait
	rWait := RunActions(5, []Action{{Kind: Wait, N: 100}}, cfg)
	if rWait.Failure != nil {
		t.Fatalf("wait failed: %v", rWait.Failure)
	}
}

// TestRunActionsFullMeshPartitionThenSettle verifies that partitioning all links
// while creating edits on all replicas converges cleanly upon settling.
func TestRunActionsFullMeshPartitionThenSettle(t *testing.T) {
	cfg := Config{Replicas: 3, Steps: 10}
	acts := []Action{
		// Cut all connections
		{Kind: Partition, A: 0, B: 1, N: 0},
		{Kind: Partition, A: 1, B: 2, N: 0},
		{Kind: Partition, A: 0, B: 2, N: 0},
		// Independent edits
		{Kind: Create, A: 0, N: 111},
		{Kind: Create, A: 1, N: 222},
		{Kind: Create, A: 2, N: 333},
		// Sync attempts must be blocked
		{Kind: Sync, A: 0, B: 1, N: 0},
	}

	res := RunActions(42, acts, cfg)
	if res.Failure != nil {
		t.Fatalf("run failed: %v\nTrace:\n%s", res.Failure, strings.Join(res.Trace, "\n"))
	}
	if res.Stats.Partitions != 3 {
		t.Errorf("Stats.Partitions = %d, want 3", res.Stats.Partitions)
	}
	if res.Stats.Blocked < 1 {
		t.Errorf("Stats.Blocked = %d, want >= 1", res.Stats.Blocked)
	}
	if res.Ops != 3 {
		t.Errorf("converged ops = %d, want 3", res.Ops)
	}
}
