package sqlstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"offgrid/internal/task"
)

func TestSummaryTriggerMultiNodeConcurrentAppends(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/multi_summary.db", Options{NodeID: "node-0"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	const nodes = 8
	const opsPerNode = 25
	var wg sync.WaitGroup
	wg.Add(nodes)

	for i := 0; i < nodes; i++ {
		nodeID := fmt.Sprintf("node-%d", i)
		go func(nid string) {
			defer wg.Done()
			for s := 1; s <= opsPerNode; s++ {
				op := Op{
					TS:      time.Now().UTC(),
					Counter: 0,
					NodeID:  nid,
					Seq:     int64(s),
					Kind:    KindTaskCreated,
					Payload: []byte(fmt.Sprintf(`{"id":"task-%s-%d","title":"t","site":"","priority":"low"}`, nid, s)),
				}
				if _, err := st.ApplyRemote(ctx, []Op{op}); err != nil {
					t.Errorf("ApplyRemote(%s, %d): %v", nid, s, err)
					return
				}
			}
		}(nodeID)
	}
	wg.Wait()

	sum, err := st.Summary(ctx)
	if err != nil {
		t.Fatalf("st.Summary: %v", err)
	}
	logSum, err := summaryFromLog(ctx, st.db)
	if err != nil {
		t.Fatalf("summaryFromLog: %v", err)
	}

	if len(sum) != nodes {
		t.Fatalf("got %d nodes in summary, want %d", len(sum), nodes)
	}
	for n, maxSeq := range logSum {
		if sum[n] != maxSeq {
			t.Errorf("node %s: summary has %d, log has %d", n, sum[n], maxSeq)
		}
	}

	drift, err := st.CheckSummary(ctx)
	if err != nil || len(drift) != 0 {
		t.Fatalf("expected 0 drift, got %v (err=%v)", drift, err)
	}
}

func TestSummaryTriggerOutOfOrderSequences(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/ooo_summary.db", Options{NodeID: "main"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Direct inserts into ops table to test trigger monotonicity:
	// Insert seq 5 first
	_, err = st.db.Exec(`INSERT INTO ops (ts, counter, node_id, seq, kind, payload) VALUES (1000, 0, 'remote-peer', 5, 'task.created', '{}')`)
	if err != nil {
		t.Fatalf("insert seq 5: %v", err)
	}

	sum, _ := st.Summary(ctx)
	if sum["remote-peer"] != 5 {
		t.Fatalf("want max_seq 5, got %d", sum["remote-peer"])
	}

	// Insert lower seq 2 (simulating out-of-order or duplicate op insertion)
	_, err = st.db.Exec(`INSERT INTO ops (ts, counter, node_id, seq, kind, payload) VALUES (1001, 0, 'remote-peer', 2, 'task.created', '{}')`)
	if err != nil {
		t.Fatalf("insert seq 2: %v", err)
	}

	sum, _ = st.Summary(ctx)
	if sum["remote-peer"] != 5 {
		t.Fatalf("max_seq regressed to %d after lower seq 2 inserted", sum["remote-peer"])
	}

	// Insert higher seq 8
	_, err = st.db.Exec(`INSERT INTO ops (ts, counter, node_id, seq, kind, payload) VALUES (1002, 0, 'remote-peer', 8, 'task.created', '{}')`)
	if err != nil {
		t.Fatalf("insert seq 8: %v", err)
	}

	sum, _ = st.Summary(ctx)
	if sum["remote-peer"] != 8 {
		t.Fatalf("want max_seq 8, got %d", sum["remote-peer"])
	}
}

func TestCheckSummaryDetectsAllDriftModesAndRebuildRepairs(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/drift.db", Options{NodeID: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if _, _, err := st.Create(ctx, task.NewTask{Title: "Task 1", Priority: task.PriorityLow}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Create(ctx, task.NewTask{Title: "Task 2", Priority: task.PriorityLow}); err != nil {
		t.Fatal(err)
	}

	// 1. Manually tamper with summary table to simulate desync
	if _, err := st.db.Exec(`UPDATE op_summary SET max_seq = 999 WHERE node_id = 'node-a'`); err != nil {
		t.Fatal(err)
	}
	drift, err := st.CheckSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(drift) != 1 || drift[0].Table != 999 || drift[0].Log != 2 {
		t.Fatalf("unexpected drift: %+v", drift)
	}
	if drift[0].String() != "node-a: summary table says 999, the log says 2" {
		t.Errorf("unexpected drift String(): %s", drift[0].String())
	}

	// Verify fails on drift
	if _, err := st.Verify(ctx); !errors.Is(err, ErrDiverged) {
		t.Fatalf("expected ErrDiverged on tampered summary, got %v", err)
	}

	// Rebuild repairs it
	if _, err := st.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if drift, _ := st.CheckSummary(ctx); len(drift) != 0 {
		t.Fatalf("expected 0 drift after Rebuild, got %+v", drift)
	}

	// 2. Insert phantom node into op_summary not in ops
	if _, err := st.db.Exec(`INSERT INTO op_summary (node_id, max_seq) VALUES ('ghost-node', 5)`); err != nil {
		t.Fatal(err)
	}
	drift, err = st.CheckSummary(ctx)
	if err != nil || len(drift) != 1 || drift[0].Node != "ghost-node" || drift[0].Log != 0 {
		t.Fatalf("expected phantom node drift, got %+v (err=%v)", drift, err)
	}

	// Rebuild removes phantom node
	if _, err := st.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	drift, _ = st.CheckSummary(ctx)
	if len(drift) != 0 {
		t.Fatalf("expected 0 drift after Rebuild, got %+v", drift)
	}
}

func TestSummaryEmptyDatabaseBehavior(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, ":memory:", Options{NodeID: "empty"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	sum, err := st.Summary(ctx)
	if err != nil {
		t.Fatalf("Summary on empty db failed: %v", err)
	}
	if len(sum) != 0 {
		t.Fatalf("expected empty summary, got %v", sum)
	}

	drift, err := st.CheckSummary(ctx)
	if err != nil || len(drift) != 0 {
		t.Fatalf("expected no drift on empty db, got %v (err=%v)", drift, err)
	}
}

func TestSummaryTriggerTransactionRollback(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/rollback.db", Options{NodeID: "rb-node"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Direct insert within transaction
	_, err = tx.ExecContext(ctx, `INSERT INTO ops (ts, counter, node_id, seq, kind, payload) VALUES (100, 0, 'tx-node', 1, 'task.created', '{}')`)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}

	// Before rollback, summary inside tx has it
	txSum, err := scanSummary(ctx, tx, querySummary)
	if err != nil || txSum["tx-node"] != 1 {
		tx.Rollback()
		t.Fatalf("inside tx: expected tx-node seq 1, got %v (err=%v)", txSum, err)
	}

	// Rollback
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	// Outside tx, op_summary must not have tx-node
	postSum, err := st.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := postSum["tx-node"]; exists {
		t.Fatalf("op_summary leaked uncommitted transaction row: %v", postSum)
	}
}

func TestSummaryTableCheckConstraintViolation(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, ":memory:", Options{NodeID: "main"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Direct attempt to insert max_seq <= 0 must fail CHECK constraint
	_, err = st.db.ExecContext(ctx, `INSERT INTO op_summary (node_id, max_seq) VALUES ('bad-node', 0)`)
	if err == nil {
		t.Fatal("expected CHECK constraint failure on max_seq = 0, got nil")
	}

	_, err = st.db.ExecContext(ctx, `INSERT INTO op_summary (node_id, max_seq) VALUES ('bad-node', -1)`)
	if err == nil {
		t.Fatal("expected CHECK constraint failure on max_seq = -1, got nil")
	}
}

func TestSummaryTableHighFanoutReplicas(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/fanout_summary.db", Options{NodeID: "leader"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	const numReplicas = 24
	const opsPerReplica = 10
	var wg sync.WaitGroup
	wg.Add(numReplicas)

	for r := 0; r < numReplicas; r++ {
		replicaNode := fmt.Sprintf("replica-%02d", r)
		go func(node string) {
			defer wg.Done()
			for s := 1; s <= opsPerReplica; s++ {
				op := Op{
					TS:      time.Now().UTC(),
					Counter: 0,
					NodeID:  node,
					Seq:     int64(s),
					Kind:    KindTaskCreated,
					Payload: []byte(fmt.Sprintf(`{"id":"task-%s-%d","title":"test","site":"","priority":"low"}`, node, s)),
				}
				if _, err := st.ApplyRemote(ctx, []Op{op}); err != nil {
					t.Errorf("ApplyRemote failed for %s op %d: %v", node, s, err)
					return
				}
			}
		}(replicaNode)
	}
	wg.Wait()

	sum, err := st.Summary(ctx)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if len(sum) != numReplicas {
		t.Fatalf("expected %d replicas in summary, got %d", numReplicas, len(sum))
	}
	for r := 0; r < numReplicas; r++ {
		replicaNode := fmt.Sprintf("replica-%02d", r)
		if sum[replicaNode] != opsPerReplica {
			t.Errorf("replica %s: expected seq %d, got %d", replicaNode, opsPerReplica, sum[replicaNode])
		}
	}
}

func TestVerifyCatchesDeletedSummaryRowAndRebuildRepairs(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/repair_summary.db", Options{NodeID: "main"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	op := Op{
		TS:      time.Now().UTC(),
		Counter: 0,
		NodeID:  "device-alpha",
		Seq:     1,
		Kind:    KindTaskCreated,
		Payload: []byte(`{"id":"task-alpha-1","title":"test","site":"","priority":"low"}`),
	}
	if _, err := st.ApplyRemote(ctx, []Op{op}); err != nil {
		t.Fatal(err)
	}

	// Delete the row from op_summary directly
	if _, err := st.db.ExecContext(ctx, `DELETE FROM op_summary WHERE node_id = 'device-alpha'`); err != nil {
		t.Fatal(err)
	}

	// CheckSummary must report drift
	drift, err := st.CheckSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(drift) != 1 || drift[0].Node != "device-alpha" || drift[0].Table != 0 || drift[0].Log != 1 {
		t.Fatalf("unexpected drift report: %v", drift)
	}

	// Verify must fail with ErrDiverged
	if _, err := st.Verify(ctx); !errors.Is(err, ErrDiverged) {
		t.Fatalf("expected ErrDiverged on deleted summary row, got %v", err)
	}

	// Rebuild repairs it
	if _, err := st.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	// After Rebuild, drift must be 0 and Verify must pass
	driftAfter, err := st.CheckSummary(ctx)
	if err != nil || len(driftAfter) != 0 {
		t.Fatalf("expected 0 drift after Rebuild, got %v (err=%v)", driftAfter, err)
	}
	if _, err := st.Verify(ctx); err != nil {
		t.Fatalf("Verify failed after Rebuild: %v", err)
	}
}

func TestMigration8SchemaObjects(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, ":memory:", Options{NodeID: "main"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Check op_summary table
	var tableSQL string
	err = st.db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name='op_summary'`).Scan(&tableSQL)
	if err != nil {
		t.Fatalf("op_summary table not found in sqlite_master: %v", err)
	}
	if !strings.Contains(tableSQL, "WITHOUT ROWID") {
		t.Errorf("expected op_summary to be WITHOUT ROWID, got SQL: %s", tableSQL)
	}

	// Check ops_summary_insert trigger
	var triggerSQL string
	err = st.db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='trigger' AND name='ops_summary_insert'`).Scan(&triggerSQL)
	if err != nil {
		t.Fatalf("ops_summary_insert trigger not found: %v", err)
	}
	if !strings.Contains(triggerSQL, "AFTER INSERT ON ops") {
		t.Errorf("expected trigger on ops table, got: %s", triggerSQL)
	}

	// Check ops_newest index
	var indexSQL string
	err = st.db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='index' AND name='ops_newest'`).Scan(&indexSQL)
	if err != nil {
		t.Fatalf("ops_newest index not found: %v", err)
	}
	if !strings.Contains(indexSQL, "ts DESC, counter DESC, node_id DESC, seq DESC") {
		t.Errorf("unexpected ops_newest index definition: %s", indexSQL)
	}
}
