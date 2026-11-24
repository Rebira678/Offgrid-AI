package growth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

const mb = 1 << 20

func sample(days float64, db, blob int64) Sample {
	return Sample{At: t0.Add(time.Duration(days * 24 * float64(time.Hour))), Ops: int64(days * 100), DBBytes: db, BlobBytes: blob, Blobs: 3,
		First: t0, Last: t0.Add(time.Duration(days * 24 * float64(time.Hour))), ActiveDays: int64(days)}
}

func TestProjectionIsExactArithmetic(t *testing.T) {
	// 30 days, 30 MB database and 60 MB of photos: 1 MB and 2 MB a day, so 365 MB and 730 MB a year.
	s := sample(30, 30*mb, 60*mb)
	p := Project(s, true)
	if p.YearDB != 365*mb || p.YearBlobs != 730*mb {
		t.Fatalf("year db %d blobs %d", p.YearDB, p.YearBlobs)
	}
	if p.TotalDB != 395*mb || p.TotalBlobs != 790*mb {
		t.Fatalf("totals %d %d", p.TotalDB, p.TotalBlobs)
	}
	if p.YearOps != 36500 || !p.Enough {
		t.Fatalf("ops %d enough %v", p.YearOps, p.Enough)
	}
}

func TestActiveDaysBasisScalesByTheShareOfDaysUsed(t *testing.T) {
	// Worked 20 of 40 calendar days. Per active day 1 MB; a year of the same habit is 182.5 active days.
	s := sample(40, 20*mb, 0)
	s.ActiveDays = 20
	p := Project(s, false)
	want := int64(float64(20*mb) / 20 * 365 * 20 / 40)
	if p.YearDB != want {
		t.Fatalf("got %d want %d", p.YearDB, want)
	}
	if c := Project(s, true); c.YearDB != 365*mb/2 {
		t.Fatalf("calendar basis %d", c.YearDB)
	}
}

func TestABurstIsNotInventedIntoAHugeRate(t *testing.T) {
	s := sample(0.01, 1*mb, 0) // a day's worth of writing in a quarter of an hour
	p := Project(s, true)
	if p.DaysUsed != 1 || p.YearDB != 365*mb || p.Enough {
		t.Fatalf("%+v", p)
	}
	if e := Project(Sample{}, true); e.YearDB != 0 || e.Enough {
		t.Fatalf("empty: %+v", e)
	}
}

func TestLogLengthAlarmFiresOnlyOverTheLimit(t *testing.T) {
	s := sample(30, 0, 0) // 3,000 ops
	if a := Check(s, nil, Limits{MaxOps: 3000}); len(a) != 0 {
		t.Fatalf("at the limit is not over it: %v", a)
	}
	a := Check(s, nil, Limits{MaxOps: 2999})
	if len(a) != 1 || a[0].What != "log length" || !strings.Contains(a[0].String(), "3000 ops") {
		t.Fatalf("%v", a)
	}
}

func TestSizeAlarmsFireOnlyOverTheLimit(t *testing.T) {
	s := sample(30, 100*mb, 200*mb)
	if a := Check(s, nil, Limits{MaxDB: 100 * mb, MaxBlobs: 200 * mb}); len(a) != 0 {
		t.Fatalf("at the limit is not over it: %v", a)
	}
	a := Check(s, nil, Limits{MaxDB: 99 * mb, MaxBlobs: 199 * mb})
	if len(a) != 2 || a[0].What != "database size" || a[1].What != "blob size" {
		t.Fatalf("%v", a)
	}
	if !strings.Contains(a[0].String(), "100.0 MiB") || !strings.Contains(a[0].String(), "99.0 MiB") {
		t.Fatal(a[0].String())
	}
	if a := Check(s, nil, Limits{}); len(a) != 0 {
		t.Fatalf("zero means no limit: %v", a)
	}
}

func TestProjectedAlarmNeedsAWeekOfHistory(t *testing.T) {
	young := sample(3, 30*mb, 0) // 10 MB a day: huge, but three days prove nothing
	if a := Check(young, nil, Limits{YearDB: 100 * mb}); len(a) != 0 {
		t.Fatalf("a guess must not page anyone: %v", a)
	}
	old := sample(30, 30*mb, 0)
	if a := Check(old, nil, Limits{YearDB: 100 * mb}); len(a) != 1 || a[0].What != "projected database" {
		t.Fatalf("%v", a)
	}
	if a := Check(old, nil, Limits{YearDB: 395 * mb}); len(a) != 0 {
		t.Fatalf("exactly at the limit: %v", a)
	}
	if a := Check(sample(30, 0, 60*mb), nil, Limits{YearBl: 100 * mb}); len(a) != 1 || a[0].What != "projected blobs" {
		t.Fatalf("%v", a)
	}
}

func TestRateAlarmUsesTheBaselineAboutAWindowAgo(t *testing.T) {
	now := sample(30, 100*mb, 0)
	hist := []Sample{
		{At: now.At.Add(-20 * 24 * time.Hour), DBBytes: 10 * mb},
		{At: now.At.Add(-8 * 24 * time.Hour), DBBytes: 60 * mb}, // newest one at least 7 days old: the baseline
		{At: now.At.Add(-2 * 24 * time.Hour), DBBytes: 90 * mb},
	}
	// (100-60)/8 = 5 MiB a day
	if a := Check(now, hist, Limits{MaxDBPerDay: 4 * mb}); len(a) != 1 || a[0].What != "database growth" || a[0].Value != 5*mb {
		t.Fatalf("%v", a)
	}
	if a := Check(now, hist, Limits{MaxDBPerDay: 5 * mb}); len(a) != 0 {
		t.Fatalf("exactly at the limit: %v", a)
	}
	// a fresh history: the oldest sample that is at least an hour old is used
	short := []Sample{{At: now.At.Add(-2 * 24 * time.Hour), DBBytes: 80 * mb}, {At: now.At.Add(-time.Minute), DBBytes: 99 * mb}}
	if a := Check(now, short, Limits{MaxDBPerDay: 5 * mb}); len(a) != 1 || a[0].Value != 10*mb {
		t.Fatalf("%v", a)
	}
	// a sample from a minute ago says nothing about a daily rate
	if a := Check(now, short[1:], Limits{MaxDBPerDay: 1}); len(a) != 0 {
		t.Fatalf("%v", a)
	}
	blobs := []Sample{{At: now.At.Add(-10 * 24 * time.Hour), BlobBytes: 0}}
	n2 := now
	n2.BlobBytes = 50 * mb // 5 MiB a day
	if a := Check(n2, blobs, Limits{MaxBlobPerDay: 4 * mb}); len(a) != 1 || a[0].What != "blob growth" {
		t.Fatalf("%v", a)
	}
}

func TestHistoryKeepsOneSamplePerHourAndIsPrivate(t *testing.T) {
	h := History{Path: filepath.Join(t.TempDir(), "x.growth.json")}
	if v, err := h.Load(); err != nil || v != nil {
		t.Fatalf("missing file is no history: %v %v", v, err)
	}
	a := Sample{At: t0, DBBytes: 1}
	b := Sample{At: t0.Add(10 * time.Minute), DBBytes: 2}
	c := Sample{At: t0.Add(2 * time.Hour), DBBytes: 3}
	for _, s := range []Sample{a, b, c} {
		if err := h.Add(s); err != nil {
			t.Fatal(err)
		}
	}
	v, _ := h.Load()
	if len(v) != 2 || v[0].DBBytes != 2 || v[1].DBBytes != 3 {
		t.Fatalf("%+v", v)
	}
	if st, _ := os.Stat(h.Path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	if err := os.WriteFile(h.Path, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Load(); err == nil || !strings.Contains(err.Error(), "delete it") {
		t.Fatalf("%v", err)
	}
}

func TestHistoryIsCapped(t *testing.T) {
	h := History{Path: filepath.Join(t.TempDir(), "h.json")}
	for i := 0; i < maxSamples+5; i++ {
		if err := h.Add(Sample{At: t0.Add(time.Duration(i) * 2 * time.Hour), Ops: int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	v, _ := h.Load()
	if len(v) != maxSamples || v[0].Ops != 5 {
		t.Fatalf("%d first %d", len(v), v[0].Ops)
	}
}
