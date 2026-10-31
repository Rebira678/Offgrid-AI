//go:build ignore

package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"offgrid/internal/jobqueue"
	"offgrid/internal/store/sqlstore"
)

func main() {
	qpath := "./queue.db"
	if len(os.Args) > 1 {
		qpath = os.Args[1]
	}
	ctx := context.Background()

	db, err := sqlstore.OpenDB(qpath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	q, err := jobqueue.New(ctx, db, jobqueue.Config{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// Clean out existing jobs
	_, _ = db.ExecContext(ctx, "DELETE FROM jobs")

	// Insert and complete 18 jobs
	for i := 1; i <= 18; i++ {
		key := fmt.Sprintf("sync-job-%d", i)
		_, _, err := q.Enqueue(ctx, key, "sync_outbox", []byte(fmt.Sprintf(`{"batch":%d}`, i)), jobqueue.EnqueueOptions{})
		if err != nil {
			continue
		}
		job, ok, err := q.Claim(ctx, time.Minute)
		if err != nil || !ok {
			continue
		}
		_ = q.Complete(ctx, job.ID, job.Attempts)
	}

	// Insert 2 active queued jobs
	_, _, _ = q.Enqueue(ctx, "active-mesh-sync", "sync_outbox", []byte(`{"peer":"field-tablet"}`), jobqueue.EnqueueOptions{})
	_, _, _ = q.Enqueue(ctx, "active-deep-think", "deep_think", []byte(`{"target":"solar-yield"}`), jobqueue.EnqueueOptions{})

	stats, _ := q.Stats(ctx)
	fmt.Printf("Queue stats: %d done, %d queued, %d running\n", stats.Done, stats.Queued, stats.Running)
}
