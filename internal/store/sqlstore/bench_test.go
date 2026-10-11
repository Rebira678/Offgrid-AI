package sqlstore

import (
	"context"
	"fmt"
	"testing"
	"time"

	"offgrid/internal/task"
)

// fill appends n ops: tasks, notes and status changes in a realistic mix.
func fill(b *testing.B, s *Store, n int) {
	b.Helper()
	ctx := context.Background()
	done := task.StatusDone
	made := 0
	for ops := 0; ops < n; {
		t, created, err := s.Create(ctx, task.NewTask{Title: fmt.Sprintf("task %d", made), Site: fmt.Sprint(made % 9), Priority: task.PriorityMedium})
		if err != nil {
			b.Fatal(err)
		}
		made++
		if created {
			ops++
		}
		for k := 0; k < 3 && ops < n; k++ {
			if _, err := s.AddNote(ctx, t.ID, fmt.Sprintf("note %d", k)); err != nil {
				b.Fatal(err)
			}
			ops++
		}
		if ops < n {
			if _, changed, err := s.Update(ctx, t.ID, task.Changes{Status: &done}); err != nil {
				b.Fatal(err)
			} else if changed {
				ops++
			}
		}
	}
}

// BenchmarkAppend measures committed writes per second, each one an op plus its
// effect on the view in one durable transaction (synchronous=FULL).
func BenchmarkAppend(b *testing.B) {
	clk := &clock{t: epoch}
	s, err := Open(context.Background(), newBenchDB(b), Options{NodeID: "bench", Now: clk.now})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		clk.tick(time.Second)
		if _, _, err := s.Create(ctx, task.NewTask{Title: fmt.Sprintf("task %d", i), Priority: task.PriorityLow}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRebuild measures recomputing the whole view from a log of 1,000,
// 10,000 and 50,000 ops.
func BenchmarkRebuild(b *testing.B) {
	for _, n := range []int{1_000, 10_000, 50_000} {
		b.Run(fmt.Sprintf("%d-ops", n), func(b *testing.B) {
			clk := &clock{t: epoch}
			s, err := Open(context.Background(), newBenchDB(b), Options{NodeID: "bench", Now: clk.now})
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			fill(b, s, n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := s.Rebuild(context.Background()); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func newBenchDB(b *testing.B) string { return b.TempDir() + "/bench.db" }
