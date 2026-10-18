package obs

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"offgrid/internal/jobqueue"
)

// SnapshotPath is where a process publishes inside the metrics directory.
func SnapshotPath(dir, proc string) string { return filepath.Join(dir, proc+".json") }

// Publish writes m's snapshot to dir/proc.json every interval until ctx ends,
// then once more marked Stopped so a viewer can tell "finished" from
// "crashed". If queue is non-nil it is polled for the queue depth on each
// tick. Errors writing are passed to onErr (may be nil) and do not stop it:
// the dashboard is a convenience and must never take the work down.
func Publish(ctx context.Context, m *Metrics, dir, proc string, interval time.Duration, queue func(context.Context) (jobqueue.Stats, error), onErr func(error)) {
	if interval <= 0 {
		interval = time.Second
	}
	started := m.now()
	write := func(ctx context.Context, stopped bool) {
		if queue != nil {
			if st, err := queue(ctx); err == nil {
				m.Queue(st)
			} else if onErr != nil && ctx.Err() == nil {
				onErr(err)
			}
		}
		s := m.Snapshot(proc, os.Getpid(), started, interval)
		s.Stopped = stopped
		if err := WriteSnapshot(SnapshotPath(dir, proc), s); err != nil && onErr != nil {
			onErr(err)
		}
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	write(ctx, false)
	for {
		select {
		case <-ctx.Done():
			write(context.WithoutCancel(ctx), true)
			return
		case <-t.C:
			write(ctx, false)
		}
	}
}
