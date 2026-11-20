package sqlstore

import (
	"context"
	"fmt"
	"testing"
	"time"

	"offgrid/internal/task"
)

// syncOnce moves everything b lacks from a, in batches.
func syncOnce(tb testing.TB, a, b *Store, batch int) int {
	tb.Helper()
	ctx := context.Background()
	total := 0
	for {
		have, err := b.Summary(ctx)
		if err != nil {
			tb.Fatal(err)
		}
		ops, more, err := a.OpsSince(ctx, have, batch)
		if err != nil {
			tb.Fatal(err)
		}
		if len(ops) > 0 {
			if _, err := b.ApplyRemote(ctx, ops); err != nil {
				tb.Fatal(err)
			}
			total += len(ops)
		}
		if !more {
			return total
		}
	}
}

// BenchmarkCatchUp10: the cost of catching a replica up by 10 new ops when the
// log already holds 10,000 or 100,000. Day 25 found it grew with the log; with the
// maintained summary it must not (see TestCatchUpCostIsFlat).
func BenchmarkCatchUp10(b *testing.B) {
	for _, n := range []int{10_000, 100_000} {
		b.Run(fmt.Sprintf("log-%d", n), func(b *testing.B) {
			a, c := catchUpPairFixture(b, n)
			ctx := context.Background()
			seq := 0
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				for k := 0; k < 10; k++ {
					seq++
					if _, _, err := a.Create(ctx, task.NewTask{Title: fmt.Sprintf("new %d", seq), Priority: task.PriorityLow}); err != nil {
						b.Fatal(err)
					}
				}
				b.StartTimer()
				if got := syncOnce(b, a, c, 500); got != 10 {
					b.Fatalf("moved %d ops, want 10", got)
				}
			}
		})
	}
}

// catchUpPairFixture is catchUpPair for benchmarks: a's log is built once per test process and
// copied (see fixtureDB); only the sync into the empty replica b is repeated.
func catchUpPairFixture(b *testing.B, n int) (a, c *Store) {
	b.Helper()
	ctx := context.Background()
	clk := &clock{t: epoch.Add(time.Duration(n+1000) * time.Millisecond)}
	build := func(b *testing.B, s *Store) {
		for made := 0; made < n; made++ {
			if _, _, err := s.Create(ctx, task.NewTask{Title: fmt.Sprintf("task %d", made), Priority: task.PriorityLow}); err != nil {
				b.Fatal(err)
			}
		}
	}
	// The fixture is written with the same clock rule as the copy, a clock that ticks per op, so the
	// copy's clock can continue from where the log ends.
	fclk := &clock{t: epoch}
	opts := Options{NodeID: "a", Now: func() time.Time { fclk.tick(time.Millisecond); return fclk.t }}
	path := fixtureDB(b, fmt.Sprintf("catchup-%d", n), opts, build)
	a, err := Open(ctx, path, Options{NodeID: "a", Now: clk.now})
	if err != nil {
		b.Fatal(err)
	}
	c, err = Open(ctx, b.TempDir()+"/b.db", Options{NodeID: "b", Now: clk.now})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { a.Close(); c.Close() })
	syncOnce(b, a, c, 1000)
	return a, c
}
