package growth

import (
	"testing"
	"time"
)

func FuzzProjectAndCheck(f *testing.F) {
	// Seed corpus with interesting values
	f.Add(int64(100), int64(1024), int64(2048), int64(2), int64(5), int64(1000), int64(5000), true)
	f.Add(int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), false)
	f.Add(int64(-10), int64(-500), int64(-100), int64(-1), int64(-1), int64(-1), int64(-1), true)
	f.Add(int64(1000000), int64(1<<30), int64(1<<32), int64(30), int64(100), int64(1<<30), int64(1<<32), false)

	f.Fuzz(func(t *testing.T, ops, opBytes, dbBytes, blobs, blobBytes, maxOps, maxDB int64, calendar bool) {
		t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		s := Sample{
			At:         t0.Add(10 * 24 * time.Hour),
			First:      t0,
			Last:       t0.Add(10 * 24 * time.Hour),
			Ops:        ops,
			OpBytes:    opBytes,
			DBBytes:    dbBytes,
			Blobs:      blobs,
			BlobBytes:  blobBytes,
			ActiveDays: 5,
		}

		// Project must never panic
		p := Project(s, calendar)
		_ = p.TotalDB
		_ = p.TotalBlobs

		limits := Limits{
			MaxOps: maxOps,
			MaxDB:  maxDB,
		}

		hist := []Sample{
			{
				At:        t0.Add(2 * 24 * time.Hour),
				DBBytes:   dbBytes / 2,
				BlobBytes: blobBytes / 2,
			},
		}

		// Check must never panic
		alarms := Check(s, hist, limits)
		for _, a := range alarms {
			_ = a.String()
		}
	})
}
