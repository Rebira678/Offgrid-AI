package sqlstore

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"offgrid/internal/task"
)

// planOf asks SQLite how it would run q.
func planOf(t *testing.T, s *Store, q string) []string {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+q)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a, b, c int
		var d string
		if err := rows.Scan(&a, &b, &c, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

var scanOfLog = regexp.MustCompile(`^SCAN (ops|o)\b`)

// TestHotQueriesDoNotScanTheLog is the cause of Day 25's finding, tested directly: the queries a sync
// round runs must not read every op. (The benchmark BenchmarkCatchUp10 shows the effect in time.)
func TestHotQueriesDoNotScanTheLog(t *testing.T) {
	s := openTemp(t, "a")
	for name, q := range map[string]string{"summary": querySummary, "newest op": queryNewest} {
		for _, line := range planOf(t, s, q) {
			// "SCAN ... ops_newest" with LIMIT 1 reads one index entry: that is the point of the index
			if (scanOfLog.MatchString(line) && !strings.Contains(line, "ops_newest")) || strings.Contains(line, "TEMP B-TREE") {
				t.Errorf("%s: SQLite plans %q, which reads or sorts the whole log:\n%s", name, line, strings.Join(planOf(t, s, q), "\n"))
			}
		}
	}
}

func openTemp(t *testing.T, node string) *Store {
	t.Helper()
	s, err := Open(context.Background(), t.TempDir()+"/s.db", Options{NodeID: node})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func groupBySummary(t *testing.T, s *Store) Summary {
	t.Helper()
	got, err := summaryFromLog(context.Background(), s.db)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// The maintained table equals what the log says after local writes, remote ops, duplicates and a refused batch.
func TestSummaryTableFollowsTheLog(t *testing.T) {
	ctx := context.Background()
	a, b := openTemp(t, "a"), openTemp(t, "b")
	for i := 0; i < 12; i++ {
		if _, _, err := a.Create(ctx, task.NewTask{Title: fmt.Sprintf("a%d", i), Priority: task.PriorityLow}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := b.Create(ctx, task.NewTask{Title: fmt.Sprintf("b%d", i), Priority: task.PriorityLow}); err != nil {
			t.Fatal(err)
		}
	}
	for round := 0; round < 2; round++ { // the second round is all duplicates
		have, _ := b.Summary(ctx)
		ops, _, err := a.OpsSince(ctx, have, 1000)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.ApplyRemote(ctx, ops); err != nil {
			t.Fatal(err)
		}
	}
	// a batch that fails halfway must leave the table as the log is: the table moves in the same transaction
	have, _ := a.Summary(ctx)
	ops, _, _ := b.OpsSince(ctx, have, 1000)
	bad := append([]Op(nil), ops...)
	bad[len(bad)-1].Seq += 5 // a gap
	if _, err := a.ApplyRemote(ctx, bad); err == nil {
		t.Fatal("a batch with a gap was accepted")
	}
	for name, s := range map[string]*Store{"a": a, "b": b} {
		got, _ := s.Summary(ctx)
		want := groupBySummary(t, s)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: summary table %v, log says %v", name, got, want)
		}
	}
	if drift, err := b.CheckSummary(ctx); err != nil || len(drift) != 0 {
		t.Fatalf("CheckSummary on a healthy store = %v, %v", drift, err)
	}
}

// A table that disagrees with the log is found by CheckSummary and Verify, and Rebuild repairs it.
func TestSummaryDriftIsFoundAndRepaired(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t, "a")
	for i := 0; i < 5; i++ {
		if _, _, err := s.Create(ctx, task.NewTask{Title: fmt.Sprintf("t%d", i), Priority: task.PriorityLow}); err != nil {
			t.Fatal(err)
		}
	}
	for name, sql := range map[string]string{
		"too low":  `UPDATE op_summary SET max_seq = 2`,
		"too high": `UPDATE op_summary SET max_seq = 99`,
		"missing":  `DELETE FROM op_summary`,
		"extra":    `INSERT INTO op_summary VALUES ('ghost', 4)`,
	} {
		if _, err := s.db.ExecContext(ctx, sql); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		drift, err := s.CheckSummary(ctx)
		if err != nil || len(drift) == 0 {
			t.Fatalf("%s: CheckSummary = %v, %v; want a difference", name, drift, err)
		}
		if _, err := s.Verify(ctx); err == nil {
			t.Fatalf("%s: Verify did not notice the summary table", name)
		}
		if _, err := s.Rebuild(ctx); err != nil {
			t.Fatalf("%s: Rebuild: %v", name, err)
		}
		if drift, err := s.CheckSummary(ctx); err != nil || len(drift) != 0 {
			t.Fatalf("%s: after Rebuild: %v, %v", name, drift, err)
		}
		if _, err := s.Verify(ctx); err != nil {
			t.Fatalf("%s: Verify after Rebuild: %v", name, err)
		}
	}
}

// A database from before migration 8 gets its table filled from the log, and keeps its ops.
func TestMigration8BackfillsTheSummary(t *testing.T) {
	ctx := context.Background()
	raw := rawDB(t)
	if err := migrate(ctx, raw, migrations[:7]); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		if _, err := raw.Exec(`INSERT INTO ops (node_id, seq, ts, counter, kind, payload) VALUES ('x', ?, ?, 0, 'k', '{}')`, i, i); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := raw.Exec(`INSERT INTO ops (node_id, seq, ts, counter, kind, payload) VALUES ('y', 1, 9, 0, 'k', '{}')`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, raw, migrations); err != nil {
		t.Fatal(err)
	}
	got, err := summaryOf(ctx, raw)
	if err != nil || got["x"] != 3 || got["y"] != 1 || len(got) != 2 {
		t.Fatalf("summary after migration = %v, %v", got, err)
	}
	// and it is maintained from then on
	if _, err := raw.Exec(`INSERT INTO ops (node_id, seq, ts, counter, kind, payload) VALUES ('x', 4, 10, 0, 'k', '{}')`); err != nil {
		t.Fatal(err)
	}
	if got, _ := summaryOf(ctx, raw); got["x"] != 4 {
		t.Fatalf("the trigger did not advance the summary: %v", got)
	}
}
