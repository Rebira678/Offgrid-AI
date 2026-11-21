package sqlstore

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"offgrid/internal/task"
)

// BenchmarkAppendConcurrent is BenchmarkAppend with several writers at once: the case group
// commit exists for. Each writer is a goroutine calling Create in a loop; ns/op is wall time per
// committed op across all writers, so "appends per second" is 1e9 / ns/op. Every Create
// returns only after its commit is durable (synchronous=FULL).
func BenchmarkAppendConcurrent(b *testing.B) {
	for _, writers := range []int{1, 4, 16, 64} {
		b.Run(fmt.Sprintf("writers=%d", writers), func(b *testing.B) {
			s, err := Open(context.Background(), newBenchDB(b), Options{NodeID: "bench"})
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			ctx := context.Background()
			per := (b.N + writers - 1) / writers
			var wg sync.WaitGroup
			errs := make(chan error, writers)
			b.ReportAllocs()
			b.ResetTimer()
			for w := 0; w < writers; w++ {
				wg.Add(1)
				go func(w int) {
					defer wg.Done()
					for i := 0; i < per; i++ {
						if _, _, err := s.Create(ctx, task.NewTask{Title: fmt.Sprintf("w%d task %d", w, i), Priority: task.PriorityLow}); err != nil {
							errs <- err
							return
						}
					}
				}(w)
			}
			wg.Wait()
			b.StopTimer()
			close(errs)
			for err := range errs {
				b.Fatal(err)
			}
			b.ReportMetric(float64(per*writers)/b.Elapsed().Seconds(), "appends/s")
		})
	}
}

// BenchmarkListUnderWrites is how long a read takes while 16 writers are busy: the number the
// reader pool (store.go, openReaders) exists to keep small. ns/op is one List of 500 tasks.
func BenchmarkListUnderWrites(b *testing.B) {
	s, err := Open(context.Background(), newBenchDB(b), Options{NodeID: "bench"})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	for i := 0; i < 500; i++ {
		if _, _, err := s.Create(ctx, task.NewTask{Title: fmt.Sprintf("seed %d", i), Priority: task.PriorityLow}); err != nil {
			b.Fatal(err)
		}
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				s.Create(ctx, task.NewTask{Title: fmt.Sprintf("w%d-%d", w, i), Priority: task.PriorityLow})
			}
		}(w)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.List(ctx, task.Filter{}); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	close(stop)
	wg.Wait()
}
