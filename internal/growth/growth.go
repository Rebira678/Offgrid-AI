// Package growth turns what a replica holds into "how big will this be in a year" and raises an alarm
// when the log or the blob folder grows past what the owner said they would accept (Day 49).
//
// It replaces building compaction: the question compaction answers ("is the log too big?") is answered
// here with the pilot's own numbers, and the alarm says when the answer changes.
//
// Everything in this file is a pure function of its inputs so the arithmetic can be tested exactly.
package growth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"
)

const day = 24 * time.Hour

// Sample is one measurement of a replica.
type Sample struct {
	At         time.Time `json:"at"`
	Ops        int64     `json:"ops"`
	OpBytes    int64     `json:"op_bytes"`   // the sum of op payload lengths (the rest of a row is hashes, signature and indexes: see DBBytes)
	DBBytes    int64     `json:"db_bytes"`   // the whole SQLite file as it lies on disk (pages x page size)
	BlobBytes  int64     `json:"blob_bytes"` // files in the blob folder
	Blobs      int64     `json:"blobs"`
	First      time.Time `json:"first"`       // oldest op in the log (zero if empty)
	Last       time.Time `json:"last"`        // newest op in the log
	ActiveDays int64     `json:"active_days"` // distinct calendar days that have at least one op
}

// Rate is growth per day.
type Rate struct {
	OpsPerDay   float64 `json:"ops_per_day"`
	DBPerDay    float64 `json:"db_bytes_per_day"`
	BlobPerDay  float64 `json:"blob_bytes_per_day"`
	TotalPerDay float64 `json:"total_bytes_per_day"`
}

// Projection is what a year of the same use would add, and what the replica would then hold.
type Projection struct {
	Basis      string  `json:"basis"`     // "calendar" or "active days"
	DaysUsed   float64 `json:"days_used"` // the denominator
	Rate       Rate    `json:"rate"`
	YearOps    int64   `json:"year_ops"`
	YearDB     int64   `json:"year_db_bytes"`   // added in a year
	YearBlobs  int64   `json:"year_blob_bytes"` // added in a year
	TotalDB    int64   `json:"total_db_bytes"`  // held after a year
	TotalBlobs int64   `json:"total_blob_bytes"`
	Enough     bool    `json:"enough"` // false when the log spans less than MinSpan: the number is a guess
}

// MinSpan is how much history a projection needs before it is more than a guess.
const MinSpan = 7 * day

// Project extrapolates a year from the log's own history. calendar=false divides by the days that had
// activity, which is the better number for a crew that works weekdays and a worse one for a hobbyist.
func Project(s Sample, calendar bool) Projection {
	p := Projection{Basis: "calendar"}
	var days float64
	if !s.First.IsZero() && s.Last.After(s.First) {
		days = s.Last.Sub(s.First).Hours() / 24
	}
	p.Enough = !s.First.IsZero() && s.Last.Sub(s.First) >= MinSpan
	if !calendar {
		p.Basis = "active days"
		days = float64(s.ActiveDays)
	}
	if days < 1 {
		days = 1 // never divide a burst by a fraction of a day: that would invent a huge rate
	}
	p.DaysUsed = days
	p.Rate = Rate{
		OpsPerDay:  float64(s.Ops) / days,
		DBPerDay:   float64(s.DBBytes) / days,
		BlobPerDay: float64(s.BlobBytes) / days,
	}
	p.Rate.TotalPerDay = p.Rate.DBPerDay + p.Rate.BlobPerDay
	// Active days happen on only some calendar days; a year of them is 365 * active/calendar.
	scale := 365.0
	if !calendar && !s.First.IsZero() {
		span := s.Last.Sub(s.First).Hours() / 24
		if span < float64(s.ActiveDays) {
			span = float64(s.ActiveDays)
		}
		if span >= 1 {
			scale = 365.0 * float64(s.ActiveDays) / span
		}
	}
	p.YearOps = int64(float64(s.Ops) / days * scale)
	p.YearDB = int64(float64(s.DBBytes) / days * scale)
	p.YearBlobs = int64(float64(s.BlobBytes) / days * scale)
	p.TotalDB = s.DBBytes + p.YearDB
	p.TotalBlobs = s.BlobBytes + p.YearBlobs
	return p
}

// Limits are what the owner will accept. Zero means "no limit" for that line.
type Limits struct {
	MaxOps         int64   // ops in the log (ADR 010's trigger for building compaction)
	MaxDB          int64   // the database file
	MaxBlobs       int64   // the blob folder
	MaxDBPerDay    float64 // growth between two samples, bytes per day
	MaxBlobPerDay  float64
	RateWindow     time.Duration // how far back to look for the rate (default 7 days)
	YearDB, YearBl int64         // alarm if the projected size after a year exceeds this
}

// Alarm is one line that went over.
type Alarm struct {
	What   string `json:"what"`
	Value  int64  `json:"value"`
	Limit  int64  `json:"limit"`
	Detail string `json:"detail"`
}

func (a Alarm) String() string { return a.What + ": " + a.Detail }

func mib(n int64) string { return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20)) }

// Check compares now against the limits and, when history is given, against the sample about
// RateWindow ago. History must be oldest first.
func Check(now Sample, hist []Sample, l Limits) []Alarm {
	var out []Alarm
	if l.MaxOps > 0 && now.Ops > l.MaxOps {
		out = append(out, Alarm{"log length", now.Ops, l.MaxOps, fmt.Sprintf("the log holds %d ops, over your limit of %d", now.Ops, l.MaxOps)})
	}
	if l.MaxDB > 0 && now.DBBytes > l.MaxDB {
		out = append(out, Alarm{"database size", now.DBBytes, l.MaxDB, fmt.Sprintf("the database is %s, over your limit of %s", mib(now.DBBytes), mib(l.MaxDB))})
	}
	if l.MaxBlobs > 0 && now.BlobBytes > l.MaxBlobs {
		out = append(out, Alarm{"blob size", now.BlobBytes, l.MaxBlobs, fmt.Sprintf("the blob folder is %s, over your limit of %s", mib(now.BlobBytes), mib(l.MaxBlobs))})
	}
	p := Project(now, true)
	if l.YearDB > 0 && p.Enough && p.TotalDB > l.YearDB {
		out = append(out, Alarm{"projected database", p.TotalDB, l.YearDB, fmt.Sprintf("at this pace the database will be %s in a year, over %s", mib(p.TotalDB), mib(l.YearDB))})
	}
	if l.YearBl > 0 && p.Enough && p.TotalBlobs > l.YearBl {
		out = append(out, Alarm{"projected blobs", p.TotalBlobs, l.YearBl, fmt.Sprintf("at this pace the blob folder will be %s in a year, over %s", mib(p.TotalBlobs), mib(l.YearBl))})
	}
	if base, ok := baseline(now, hist, l.RateWindow); ok {
		d := now.At.Sub(base.At).Hours() / 24
		if l.MaxDBPerDay > 0 {
			r := float64(now.DBBytes-base.DBBytes) / d
			if r > l.MaxDBPerDay {
				out = append(out, Alarm{"database growth", int64(r), int64(l.MaxDBPerDay), fmt.Sprintf("the database grew %s a day over the last %.1f days, over %s a day", mib(int64(r)), d, mib(int64(l.MaxDBPerDay)))})
			}
		}
		if l.MaxBlobPerDay > 0 {
			r := float64(now.BlobBytes-base.BlobBytes) / d
			if r > l.MaxBlobPerDay {
				out = append(out, Alarm{"blob growth", int64(r), int64(l.MaxBlobPerDay), fmt.Sprintf("the blob folder grew %s a day over the last %.1f days, over %s a day", mib(int64(r)), d, mib(int64(l.MaxBlobPerDay)))})
			}
		}
	}
	return out
}

// baseline picks the newest sample that is at least window old, else the oldest sample that is at
// least an hour old (a shorter gap says nothing about a rate per day).
func baseline(now Sample, hist []Sample, window time.Duration) (Sample, bool) {
	if window <= 0 {
		window = 7 * day
	}
	var best Sample
	found := false
	for _, h := range hist {
		age := now.At.Sub(h.At)
		if age < time.Hour {
			continue
		}
		if age >= window {
			best, found = h, true // keep the newest of the old enough ones: hist is oldest first
			continue
		}
		if !found {
			best, found = h, true // nothing old enough yet: the oldest recent one
			break
		}
		break
	}
	return best, found
}

// History is the file of samples, kept next to the database. It is advisory: losing it loses the rate
// alarm, never data.
type History struct{ Path string }

const maxSamples = 800

// Load returns the samples oldest first. A missing file is no history.
func (h History) Load() ([]Sample, error) {
	b, err := os.ReadFile(h.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var v []Sample
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("growth history %s is not valid JSON (delete it to start again): %w", h.Path, err)
	}
	sort.SliceStable(v, func(i, j int) bool { return v[i].At.Before(v[j].At) })
	return v, nil
}

// Add appends s, keeping at most one sample per hour and the newest maxSamples, and writes the file
// atomically with owner-only permissions.
func (h History) Add(s Sample) error {
	v, err := h.Load()
	if err != nil {
		return err
	}
	if n := len(v); n > 0 && s.At.Sub(v[n-1].At) < time.Hour {
		v[n-1] = s
	} else {
		v = append(v, s)
	}
	if len(v) > maxSamples {
		v = v[len(v)-maxSamples:]
	}
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	tmp := h.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, h.Path)
}
