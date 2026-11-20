package sqlstore

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"offgrid/internal/task"
)

// Fixtures. A benchmark function is called again and again (b.N = 1, then larger, and once
// per -count), and each call used to rebuild its whole log with durable writes: 100,000 fsyncs
// for CatchUp10 on a slow disk, minutes per call, so the suite could not be run ten times.
// fixtureDB builds a seeded database once per test process and hands out private copies.
var fixtures struct {
	sync.Mutex
	dir   string
	built map[string]string
}

func fixtureDB(b *testing.B, key string, opts Options, build func(b *testing.B, s *Store)) string {
	b.Helper()
	fixtures.Lock()
	defer fixtures.Unlock()
	if fixtures.dir == "" {
		dir, err := os.MkdirTemp("", "offgrid-bench-")
		if err != nil {
			b.Fatal(err)
		}
		fixtures.dir, fixtures.built = dir, map[string]string{}
	}
	src, ok := fixtures.built[key]
	if !ok {
		src = filepath.Join(fixtures.dir, key+".db")
		s, err := Open(context.Background(), src, opts)
		if err != nil {
			b.Fatal(err)
		}
		build(b, s)
		if err := s.Close(); err != nil {
			b.Fatal(err)
		}
		fixtures.built[key] = src
	}
	dst := filepath.Join(b.TempDir(), "bench.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := copyBenchFile(src+suffix, dst+suffix); err != nil && !(suffix != "" && os.IsNotExist(err)) {
			b.Fatal(err)
		}
	}
	return dst
}

func removeFixtures() {
	if fixtures.dir != "" {
		os.RemoveAll(fixtures.dir)
	}
}

func copyBenchFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

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
			s := openFilled(b, n)
			defer s.Close()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := s.Rebuild(context.Background()); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkFields measures reading the entire op log into per-field CRDT registers
// from logs of 1,000 and 10,000 ops.
func BenchmarkFields(b *testing.B) {
	for _, n := range []int{1_000, 10_000} {
		b.Run(fmt.Sprintf("%d-ops", n), func(b *testing.B) {
			s := openFilled(b, n)
			defer s.Close()
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := s.Fields(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// openFilled opens a private copy of a database whose log holds n ops from fill.
func openFilled(b *testing.B, n int) *Store {
	b.Helper()
	opts := Options{NodeID: "bench", Now: (&clock{t: epoch}).now}
	path := fixtureDB(b, fmt.Sprintf("filled-%d", n), opts, func(b *testing.B, s *Store) { fill(b, s, n) })
	s, err := Open(context.Background(), path, opts)
	if err != nil {
		b.Fatal(err)
	}
	return s
}

func newBenchDB(b *testing.B) string { return b.TempDir() + "/bench.db" }

// BenchmarkVerifyChain: the cost of recomputing every hash in a 10,000-op log.
func BenchmarkVerifyChain10k(b *testing.B) {
	ctx := context.Background()
	s := openFilled(b, 10000)
	defer s.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.VerifyChain(ctx); err != nil {
			b.Fatal(err)
		}
	}
}
