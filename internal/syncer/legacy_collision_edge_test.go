package syncer

import (
	"context"
	"strings"
	"testing"
	"time"

	"offgrid/internal/store/sqlstore"
)

// TestLegacyPlainIDSyncRefusalAndNonDestructiveAbort tests that if two pre-Day 24
// replicas both created legacy unadorned IDs ("T1") before meeting, sync cleanly refuses
// to corrupt either database, returns a descriptive error, and leaves both databases
// fully consistent and verifiable.
func TestLegacyPlainIDSyncRefusalAndNonDestructiveAbort(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{t: t0}

	storeA := newStore(t, "legacy-a", clk)
	storeB := newStore(t, "legacy-b", clk)

	// Simulate legacy pre-Day 24 ops where each replica recorded a plain "T1"
	opA := []sqlstore.Op{
		{
			NodeID:  "legacy-a",
			Seq:     1,
			TS:      t0,
			Kind:    sqlstore.KindTaskCreated,
			Payload: []byte(`{"id":"T1","title":"Legacy Task A","site":"1","priority":"high"}`),
		},
	}
	opB := []sqlstore.Op{
		{
			NodeID:  "legacy-b",
			Seq:     1,
			TS:      t0.Add(time.Second),
			Kind:    sqlstore.KindTaskCreated,
			Payload: []byte(`{"id":"T1","title":"Legacy Task B","site":"2","priority":"low"}`),
		},
	}

	if _, err := storeA.ApplyRemote(ctx, opA); err != nil {
		t.Fatalf("seeding opA into storeA: %v", err)
	}
	if _, err := storeB.ApplyRemote(ctx, opB); err != nil {
		t.Fatalf("seeding opB into storeB: %v", err)
	}

	// Baseline checks: both stores verify their own initial state
	if _, err := storeA.Verify(ctx); err != nil {
		t.Fatalf("storeA verify baseline: %v", err)
	}
	if _, err := storeB.Verify(ctx); err != nil {
		t.Fatalf("storeB verify baseline: %v", err)
	}

	hashABefore, err := storeA.StateHash(ctx)
	if err != nil {
		t.Fatalf("storeA StateHash: %v", err)
	}

	// Attempt HTTP sync between legacy-a and legacy-b
	client, _ := serve(t, storeB, clk, key)
	_, err = client.Once(ctx, storeA)
	if err == nil {
		t.Fatalf("expected sync error due to colliding legacy T1 IDs, but got nil")
	}

	if !strings.Contains(err.Error(), "created twice") && !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("expected error mentioning task created twice or unique constraint, got: %v", err)
	}

	// Non-destructive abort check: storeA must remain unchanged and verify cleanly
	hashAAfter, err := storeA.StateHash(ctx)
	if err != nil {
		t.Fatalf("storeA StateHash after aborted sync: %v", err)
	}
	if hashAAfter != hashABefore {
		t.Fatalf("storeA state altered despite sync abort: %s vs %s", hashAAfter, hashABefore)
	}
	if _, err := storeA.Verify(ctx); err != nil {
		t.Fatalf("storeA failed verification after aborted sync: %v", err)
	}
}
