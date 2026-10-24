package sqlstore

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"offgrid/internal/task"
)

// TestSyncStoreRebuildOnOutOfOrderHLCArrival tests the senior-level invariant that
// if an op arrives with a timestamp earlier than ops already folded into the view,
// the store transparently drops and refolds the projection in canonical HLC order,
// reports Rebuilt: true, and produces an identical state hash to a fresh rebuild.
func TestSyncStoreRebuildOnOutOfOrderHLCArrival(t *testing.T) {
	ctx := context.Background()
	s, clk := replicaAt(t, "node-lidia", epoch)

	// Step 1: Lidia creates 3 tasks at t=10m, t=20m, t=30m
	clk.tick(10 * time.Minute)
	t1 := create(t, s, "Initial Task 1")
	clk.tick(10 * time.Minute)
	t2 := create(t, s, "Initial Task 2")
	clk.tick(10 * time.Minute)
	t3 := create(t, s, "Initial Task 3")

	summaryBefore, err := s.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summaryBefore["node-lidia"] != 3 {
		t.Fatalf("expected 3 ops, got %v", summaryBefore)
	}

	// Step 2: An out-of-order op arrives from Beka stamped at t=15m (between t1 and t2)
	bekaStamp := epoch.Add(15 * time.Minute)
	remoteOp := Op{
		NodeID:  "node-beka",
		Seq:     1,
		TS:      bekaStamp,
		Counter: 0,
		Kind:    KindTaskCreated,
		Payload: json.RawMessage(`{"id":"T1.node-beka","title":"Beka Concurrent Task","site":"East","priority":"high"}`),
	}

	res, err := s.ApplyRemote(ctx, []Op{remoteOp})
	if err != nil {
		t.Fatalf("ApplyRemote failed: %v", err)
	}

	// Invariant: Because bekaStamp (t=15m) sorts before t2 (t=20m) and t3 (t=30m),
	// the store must trigger a view rebuild!
	if !res.Rebuilt {
		t.Fatal("expected res.Rebuilt == true when inserting an out-of-order HLC op")
	}
	if res.Applied != 1 {
		t.Fatalf("expected 1 applied op, got %d", res.Applied)
	}

	// Verify that the view contains all 4 tasks
	tasks, err := s.List(ctx, task.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 4 {
		t.Fatalf("expected 4 tasks in rebuilt view, got %d", len(tasks))
	}

	// Verify projection integrity via Verify()
	mustVerify(t, s)

	// Ensure all task IDs exist and match
	for _, id := range []string{t1.ID, t2.ID, t3.ID, "T1.node-beka"} {
		if _, err := s.Get(ctx, id); err != nil {
			t.Fatalf("missing task %s after rebuild: %v", id, err)
		}
	}
}

// TestSyncStoreInterleavedDuplicatesAndAtomicRollback verifies that ApplyRemote
// handles arbitrary mixtures of duplicate ops and new ops, and that any validation
// error rolls back the entire batch atomically without partial mutation.
func TestSyncStoreInterleavedDuplicatesAndAtomicRollback(t *testing.T) {
	ctx := context.Background()
	s, _ := replicaAt(t, "node-local", epoch)

	// Seed with 5 ops from node-remote
	var seedOps []Op
	for seq := int64(1); seq <= 5; seq++ {
		seedOps = append(seedOps, Op{
			NodeID:  "node-remote",
			Seq:     seq,
			TS:      epoch.Add(time.Duration(seq) * time.Minute),
			Counter: 0,
			Kind:    KindTaskCreated,
			Payload: json.RawMessage(fmt.Sprintf(`{"id":"TR%d","title":"Task %d","site":"","priority":"low"}`, seq, seq)),
		})
	}

	res1, err := s.ApplyRemote(ctx, seedOps)
	if err != nil {
		t.Fatalf("seed ApplyRemote failed: %v", err)
	}
	if res1.Applied != 5 || res1.Duplicates != 0 {
		t.Fatalf("unexpected seed result: %+v", res1)
	}

	stateHashBefore := hashOf(t, s)

	// Case A: Batch with interleaved duplicates (seq 3, 4, 5) and new ops (seq 6, 7)
	interleaved := []Op{
		seedOps[2], // seq 3 (duplicate)
		seedOps[3], // seq 4 (duplicate)
		{
			NodeID:  "node-remote",
			Seq:     6,
			TS:      epoch.Add(6 * time.Minute),
			Counter: 0,
			Kind:    KindTaskCreated,
			Payload: json.RawMessage(`{"id":"TR6","title":"Task 6","site":"","priority":"low"}`),
		},
		seedOps[4], // seq 5 (duplicate)
		{
			NodeID:  "node-remote",
			Seq:     7,
			TS:      epoch.Add(7 * time.Minute),
			Counter: 0,
			Kind:    KindTaskCreated,
			Payload: json.RawMessage(`{"id":"TR7","title":"Task 7","site":"","priority":"low"}`),
		},
	}

	res2, err := s.ApplyRemote(ctx, interleaved)
	if err != nil {
		t.Fatalf("interleaved ApplyRemote failed: %v", err)
	}
	if res2.Applied != 2 || res2.Duplicates != 3 {
		t.Fatalf("expected 2 applied and 3 duplicates, got: %+v", res2)
	}

	// Case B: Batch with duplicate, new op, followed by an INVALID corrupt op.
	// Atomic guarantee: The entire batch must fail and nothing from the batch may be saved.
	stateHashBeforeInvalid := hashOf(t, s)
	invalidBatch := []Op{
		interleaved[2], // seq 6 (duplicate now)
		{
			NodeID:  "node-remote",
			Seq:     8,
			TS:      epoch.Add(8 * time.Minute),
			Counter: 0,
			Kind:    KindTaskCreated,
			Payload: json.RawMessage(`{"id":"TR8","title":"Task 8","site":"","priority":"low"}`),
		},
		{
			NodeID:  "node-remote",
			Seq:     9,
			TS:      epoch.Add(9 * time.Minute),
			Counter: 0,
			Kind:    "unknown.exploding.kind", // INVALID KIND
			Payload: json.RawMessage(`{}`),
		},
	}

	_, err = s.ApplyRemote(ctx, invalidBatch)
	if err == nil {
		t.Fatal("expected error on invalid op kind, got nil")
	}

	// Verify atomicity: state hash must be completely unchanged
	stateHashAfterInvalid := hashOf(t, s)
	if stateHashBeforeInvalid != stateHashAfterInvalid {
		t.Fatalf("atomicity violated! Hash before=%s, after failed batch=%s", stateHashBeforeInvalid, stateHashAfterInvalid)
	}

	// Verify that seq 8 was NOT stored
	summary, _ := s.Summary(ctx)
	if summary["node-remote"] != 7 {
		t.Fatalf("expected node-remote summary to remain at 7, got %d", summary["node-remote"])
	}

	_ = stateHashBefore
}

// TestSyncStoreMultiNodeSummaryAndOpsSince verifies summary tracking and monotonic
// delta extraction across multiple independent replicas.
func TestSyncStoreMultiNodeSummaryAndOpsSince(t *testing.T) {
	ctx := context.Background()
	s, _ := replicaAt(t, "node-local", epoch)

	// Ingest ops from 4 distinct nodes
	nodes := []string{"node-alpha", "node-bravo", "node-charlie", "node-delta"}
	for _, n := range nodes {
		var ops []Op
		for seq := int64(1); seq <= 3; seq++ {
			ops = append(ops, Op{
				NodeID:  n,
				Seq:     seq,
				TS:      epoch.Add(time.Duration(seq) * time.Minute),
				Counter: 0,
				Kind:    KindTaskCreated,
				Payload: json.RawMessage(fmt.Sprintf(`{"id":"T_%s_%d","title":"Task","site":"","priority":"low"}`, n, seq)),
			})
		}
		if _, err := s.ApplyRemote(ctx, ops); err != nil {
			t.Fatalf("apply for %s failed: %v", n, err)
		}
	}

	summary, err := s.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if summary[n] != 3 {
			t.Fatalf("summary[%s] = %d, want 3", n, summary[n])
		}
	}

	// Ask for ops since partial summary
	partialHave := Summary{
		"node-alpha":   2, // lacks seq 3
		"node-bravo":   3, // has all
		"node-charlie": 1, // lacks seq 2, 3
		"node-delta":   0, // lacks seq 1, 2, 3
	}
	missing, more, err := s.OpsSince(ctx, partialHave, 100)
	if err != nil {
		t.Fatal(err)
	}
	if more {
		t.Fatal("expected more == false")
	}
	// Expected missing: 1 (alpha) + 0 (bravo) + 2 (charlie) + 3 (delta) = 6 ops
	if len(missing) != 6 {
		t.Fatalf("expected 6 missing ops, got %d", len(missing))
	}

	// Verify that missing ops are strictly sorted in canonical HLC fold order
	for i := 1; i < len(missing); i++ {
		prev := keyOf(missing[i-1])
		curr := keyOf(missing[i])
		if !prev.less(curr) {
			t.Fatalf("ops returned out of canonical fold order at index %d: prev=%+v, curr=%+v", i, prev, curr)
		}
	}
}

func replicaBench(b *testing.B, node string, start time.Time) (*Store, *clock) {
	b.Helper()
	clk := &clock{t: start}
	dir := b.TempDir()
	s, err := Open(context.Background(), fmt.Sprintf("%s/%s.db", dir, node), Options{NodeID: node, Now: clk.now})
	if err != nil {
		b.Fatal(err)
	}
	return s, clk
}

// Benchmarks for SQLStore Sync operations

func BenchmarkApplyRemoteBatch500(b *testing.B) {
	ctx := context.Background()
	b.ReportAllocs()

	// Pre-generate 500 valid ops
	var batch []Op
	for seq := int64(1); seq <= 500; seq++ {
		batch = append(batch, Op{
			NodeID:  "node-remote-bench",
			Seq:     seq,
			TS:      epoch.Add(time.Duration(seq) * time.Second),
			Counter: 0,
			Kind:    KindTaskCreated,
			Payload: json.RawMessage(fmt.Sprintf(`{"id":"TB%d","title":"Bench Task %d","site":"","priority":"low"}`, seq, seq)),
		})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		s, _ := replicaBench(b, fmt.Sprintf("node-local-bench-%d", i), epoch)
		b.StartTimer()

		res, err := s.ApplyRemote(ctx, batch)
		if err != nil {
			b.Fatal(err)
		}
		if res.Applied != 500 {
			b.Fatalf("expected 500 applied, got %d", res.Applied)
		}
		s.Close()
	}
}

func BenchmarkOpsSinceMultiNode(b *testing.B) {
	ctx := context.Background()
	s, _ := replicaBench(b, "node-local", epoch)
	defer s.Close()

	// Populate with 200 ops across 4 nodes
	nodes := []string{"node-1", "node-2", "node-3", "node-4"}
	for _, n := range nodes {
		var ops []Op
		for seq := int64(1); seq <= 50; seq++ {
			ops = append(ops, Op{
				NodeID:  n,
				Seq:     seq,
				TS:      epoch.Add(time.Duration(seq) * time.Second),
				Counter: 0,
				Kind:    KindTaskCreated,
				Payload: json.RawMessage(fmt.Sprintf(`{"id":"T_%s_%d","title":"Task","site":"","priority":"low"}`, n, seq)),
			})
		}
		if _, err := s.ApplyRemote(ctx, ops); err != nil {
			b.Fatal(err)
		}
	}

	have := Summary{"node-1": 25, "node-2": 25, "node-3": 25, "node-4": 25}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		ops, _, err := s.OpsSince(ctx, have, 100)
		if err != nil {
			b.Fatal(err)
		}
		if len(ops) != 100 {
			b.Fatalf("expected 100 ops, got %d", len(ops))
		}
	}
}
