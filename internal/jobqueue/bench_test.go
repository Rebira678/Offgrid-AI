package jobqueue

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"offgrid/internal/store/sqlstore"
)

func BenchmarkEnqueue(b *testing.B) {
	path := filepath.Join(b.TempDir(), "bench-queue.db")
	db, err := sqlstore.OpenDB(path)
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	q, err := New(context.Background(), db, Config{})
	if err != nil {
		b.Fatal(err)
	}

	payload := json.RawMessage(`{"task":"benchmark-item"}`)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("job-%d", i)
		if _, _, err := q.Enqueue(ctx, key, "summarize", payload, EnqueueOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkClaimAndComplete(b *testing.B) {
	path := filepath.Join(b.TempDir(), "bench-claim.db")
	db, err := sqlstore.OpenDB(path)
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	q, err := New(context.Background(), db, Config{})
	if err != nil {
		b.Fatal(err)
	}

	payload := json.RawMessage(`{"task":"benchmark-item"}`)
	ctx := context.Background()

	// Pre-fill N jobs
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("job-%d", i)
		if _, _, err := q.Enqueue(ctx, key, "summarize", payload, EnqueueOptions{}); err != nil {
			b.Fatal(err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		job, ok, err := q.Claim(ctx, time.Minute)
		if err != nil {
			b.Fatal(err)
		}
		if !ok {
			b.Fatalf("expected job available at iteration %d", i)
		}
		if err := q.Complete(ctx, job.ID, job.Attempts); err != nil {
			b.Fatal(err)
		}
	}
}
