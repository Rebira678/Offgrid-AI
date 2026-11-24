package sqlstore

import (
	"context"
	"errors"
	"time"

	"offgrid/internal/growth"
)

// Growth measures this replica for the growth alarm (Day 49): what the log and the blob folder hold now,
// and how much calendar the log covers. Times come from the ops' own stamps, so a database copied to a
// new machine reports the pilot's real history, not the copy date.
func (s *Store) Growth(ctx context.Context, now time.Time) (growth.Sample, error) {
	g := growth.Sample{At: now}
	var first, last int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(length(payload)), 0), COALESCE(MIN(ts), 0), COALESCE(MAX(ts), 0) FROM ops`).
		Scan(&g.Ops, &g.OpBytes, &first, &last)
	if err != nil {
		return g, err
	}
	if g.Ops > 0 {
		g.First, g.Last = time.Unix(0, first).UTC(), time.Unix(0, last).UTC()
		// ts is nanoseconds; a day is 86400e9 of them. Days are UTC days: close enough for a rate.
		if err = s.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT ts / 86400000000000) FROM ops`).Scan(&g.ActiveDays); err != nil {
			return g, err
		}
	}
	var pages, size int64
	if err = s.db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		return g, err
	}
	if err = s.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&size); err != nil {
		return g, err
	}
	g.DBBytes = pages * size
	bs, err := s.blobStore()
	if err != nil {
		if errors.Is(err, ErrNoBlobStore) {
			return g, nil
		}
		return g, err
	}
	err = bs.Walk(func(_ string, n int64, _ time.Time) error {
		g.Blobs++
		g.BlobBytes += n
		return nil
	})
	return g, err
}
