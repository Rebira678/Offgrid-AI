package growth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestProjectBoundariesAndAnomalies(t *testing.T) {
	// 1. Exact boundary of MinSpan (7 days)
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	sJustUnder := Sample{
		First: base,
		Last:  base.Add(7*24*time.Hour - time.Nanosecond),
		Ops:   100,
	}
	pJustUnder := Project(sJustUnder, true)
	if pJustUnder.Enough {
		t.Errorf("expected Enough=false for duration just under 7 days, got true")
	}

	sExact := Sample{
		First: base,
		Last:  base.Add(7 * 24 * time.Hour),
		Ops:   100,
	}
	pExact := Project(sExact, true)
	if !pExact.Enough {
		t.Errorf("expected Enough=true for exactly 7 days, got false")
	}

	sJustOver := Sample{
		First: base,
		Last:  base.Add(7*24*time.Hour + time.Nanosecond),
		Ops:   100,
	}
	pJustOver := Project(sJustOver, true)
	if !pJustOver.Enough {
		t.Errorf("expected Enough=true for duration just over 7 days, got false")
	}

	// 2. Clock skew / inverted timestamps (Last is before First)
	sInverted := Sample{
		First:      base.Add(24 * time.Hour),
		Last:       base, // 1 day earlier
		Ops:        50,
		ActiveDays: 1,
	}
	pInverted := Project(sInverted, true)
	if pInverted.Enough {
		t.Errorf("inverted times must not be Enough")
	}
	if pInverted.DaysUsed != 1 {
		t.Errorf("expected daysUsed=1 floor, got %f", pInverted.DaysUsed)
	}
	if pInverted.Rate.OpsPerDay != 50 {
		t.Errorf("expected rate=50 ops/day, got %f", pInverted.Rate.OpsPerDay)
	}

	// 3. ActiveDays basis with 0 active days
	sZeroActive := Sample{
		First:      base,
		Last:       base.Add(10 * 24 * time.Hour),
		Ops:        100,
		ActiveDays: 0,
	}
	pZeroActive := Project(sZeroActive, false)
	if pZeroActive.DaysUsed != 1 {
		t.Errorf("expected daysUsed floor of 1 for 0 active days, got %f", pZeroActive.DaysUsed)
	}

	// 4. ActiveDays exceeds calendar span (e.g. clock jumps or multi-node divergence)
	sActiveOverSpan := Sample{
		First:      base,
		Last:       base.Add(2 * 24 * time.Hour), // 2 calendar days
		Ops:        200,
		DBBytes:    10 * mb,
		ActiveDays: 5, // 5 active days reported
	}
	pActiveOverSpan := Project(sActiveOverSpan, false)
	if pActiveOverSpan.DaysUsed != 5 {
		t.Errorf("expected daysUsed=5, got %f", pActiveOverSpan.DaysUsed)
	}
	// scale should equal 365 when span is capped to active days
	if pActiveOverSpan.YearOps != 200/5*365 {
		t.Errorf("expected yearOps=%d, got %d", 200/5*365, pActiveOverSpan.YearOps)
	}

	// 5. Zero first timestamp with non-zero last
	sZeroFirst := Sample{
		Last: base,
		Ops:  10,
	}
	pZeroFirst := Project(sZeroFirst, true)
	if pZeroFirst.Enough {
		t.Errorf("zero First time cannot be enough")
	}
	if pZeroFirst.DaysUsed != 1 {
		t.Errorf("expected daysUsed=1, got %f", pZeroFirst.DaysUsed)
	}
}

func TestCheckAllSevenAlarmsSimultaneously(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := Sample{
		At:         t0.Add(30 * 24 * time.Hour),
		First:      t0,
		Last:       t0.Add(30 * 24 * time.Hour),
		Ops:        600000,
		DBBytes:    2048 * mb,
		Blobs:      50,
		BlobBytes:  10240 * mb,
		ActiveDays: 30,
	}

	hist := []Sample{
		{
			At:        now.At.Add(-10 * 24 * time.Hour),
			DBBytes:   1000 * mb,
			BlobBytes: 5000 * mb,
		},
	}

	limits := Limits{
		MaxOps:        500000,
		MaxDB:         1024 * mb,
		MaxBlobs:      8192 * mb,
		MaxDBPerDay:   50 * mb,  // grew 1048 MB in 10 days = 104.8 MB/day > 50
		MaxBlobPerDay: 200 * mb, // grew 5240 MB in 10 days = 524 MB/day > 200
		YearDB:        1024 * mb,
		YearBl:        8192 * mb,
		RateWindow:    7 * 24 * time.Hour,
	}

	alarms := Check(now, hist, limits)
	if len(alarms) != 7 {
		t.Fatalf("expected all 7 alarms to trigger, got %d:\n%+v", len(alarms), alarms)
	}

	expectedKinds := []string{
		"log length",
		"database size",
		"blob size",
		"projected database",
		"projected blobs",
		"database growth",
		"blob growth",
	}

	for i, kind := range expectedKinds {
		if alarms[i].What != kind {
			t.Errorf("alarm[%d]: expected %q, got %q", i, kind, alarms[i].What)
		}
		if str := alarms[i].String(); !strings.Contains(str, kind) {
			t.Errorf("alarm string %q should contain %q", str, kind)
		}
	}
}

func TestCheckNegativeAndZeroLimits(t *testing.T) {
	s := sample(30, 50*mb, 50*mb)
	// Negative limits should not trigger alarms (treated as no limit)
	negLimits := Limits{
		MaxOps:        -1,
		MaxDB:         -100,
		MaxBlobs:      -100,
		MaxDBPerDay:   -5,
		MaxBlobPerDay: -5,
		YearDB:        -100,
		YearBl:        -100,
	}
	alarms := Check(s, nil, negLimits)
	if len(alarms) != 0 {
		t.Errorf("negative limits should not trigger alarms, got: %+v", alarms)
	}
}

func TestCheckNegativeGrowthNoAlarms(t *testing.T) {
	// Database shrunk due to vacuum or blobs deleted by GC
	now := sample(30, 50*mb, 20*mb)
	hist := []Sample{
		{
			At:        now.At.Add(-8 * 24 * time.Hour),
			DBBytes:   100 * mb, // was 100 MB, now 50 MB
			BlobBytes: 80 * mb,  // was 80 MB, now 20 MB
		},
	}
	limits := Limits{
		MaxDBPerDay:   5 * mb,
		MaxBlobPerDay: 5 * mb,
	}
	alarms := Check(now, hist, limits)
	if len(alarms) != 0 {
		t.Errorf("negative growth should not trigger rate alarms, got: %+v", alarms)
	}
}

func TestBaselineSelectionEdges(t *testing.T) {
	now := sample(30, 100*mb, 100*mb)

	// 1. History is completely empty
	if _, ok := baseline(now, nil, 7*day); ok {
		t.Error("expected false for nil history")
	}
	if _, ok := baseline(now, []Sample{}, 7*day); ok {
		t.Error("expected false for empty history")
	}

	// 2. All history entries are under 1 hour old
	recentHist := []Sample{
		{At: now.At.Add(-10 * time.Minute)},
		{At: now.At.Add(-45 * time.Minute)},
		{At: now.At.Add(-59 * time.Minute)},
	}
	if _, ok := baseline(now, recentHist, 7*day); ok {
		t.Error("expected false when all entries are < 1h old")
	}

	// 3. Exactly 1 hour old entry
	oneHourHist := []Sample{
		{At: now.At.Add(-1 * time.Hour), DBBytes: 42},
	}
	if base, ok := baseline(now, oneHourHist, 7*day); !ok || base.DBBytes != 42 {
		t.Errorf("expected 1-hour entry to be accepted as fallback baseline, got ok=%v, base=%+v", ok, base)
	}

	// 4. Default window when window <= 0 is 7 days
	multiHist := []Sample{
		{At: now.At.Add(-14 * 24 * time.Hour), DBBytes: 10},
		{At: now.At.Add(-8 * 24 * time.Hour), DBBytes: 20}, // newest >= 7 days
		{At: now.At.Add(-3 * 24 * time.Hour), DBBytes: 30},
	}
	if base, ok := baseline(now, multiHist, 0); !ok || base.DBBytes != 20 {
		t.Errorf("expected 8-day entry (newest >= default 7d), got ok=%v, db=%d", ok, base.DBBytes)
	}
}

func TestHistoryLoadSortsUnorderedFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "unsorted.growth.json")

	t0 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	samples := []Sample{
		{At: t0.Add(5 * time.Hour), Ops: 50},
		{At: t0.Add(1 * time.Hour), Ops: 10},
		{At: t0.Add(3 * time.Hour), Ops: 30},
	}
	raw, err := json.Marshal(samples)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	h := History{Path: p}
	loaded, err := h.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(loaded) != 3 {
		t.Fatalf("expected 3 items, got %d", len(loaded))
	}
	if loaded[0].Ops != 10 || loaded[1].Ops != 30 || loaded[2].Ops != 50 {
		t.Errorf("expected sorted items, got: %+v", loaded)
	}
}

func TestHistoryDebounceExactBoundaries(t *testing.T) {
	dir := t.TempDir()
	h := History{Path: filepath.Join(dir, "debounce.growth.json")}

	base := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)

	// Add first sample
	if err := h.Add(Sample{At: base, Ops: 10}); err != nil {
		t.Fatal(err)
	}

	// Add sample at 59m59s: within 1h of first sample (10:00:00), replaces previous
	if err := h.Add(Sample{At: base.Add(59*time.Minute + 59*time.Second), Ops: 20}); err != nil {
		t.Fatal(err)
	}
	v, _ := h.Load()
	if len(v) != 1 || v[0].Ops != 20 {
		t.Fatalf("expected 1 replaced sample with ops=20, got %+v", v)
	}

	// The newest sample in history now has timestamp 10:59:59.
	// Adding at 11:00:00 is only 1s later, so it would replace again.
	// To test appending a second sample, the new sample must be at least 1h after 10:59:59 (e.g. 12:00:00).
	if err := h.Add(Sample{At: base.Add(2 * time.Hour), Ops: 30}); err != nil {
		t.Fatal(err)
	}
	v, _ = h.Load()
	if len(v) != 2 || v[1].Ops != 30 {
		t.Fatalf("expected 2 samples with newest ops=30, got %+v", v)
	}
}

func TestHistoryConcurrentAdd(t *testing.T) {
	dir := t.TempDir()
	h := History{Path: filepath.Join(dir, "concurrent.growth.json")}

	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	const routines = 10

	for i := 0; i < routines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			// Each routine adds at a different hour to test append logic
			s := Sample{
				At:  base.Add(time.Duration(idx*2) * time.Hour),
				Ops: int64(idx + 1),
			}
			_ = h.Add(s)
		}(i)
	}
	wg.Wait()

	v, err := h.Load()
	if err != nil {
		t.Fatalf("Load after concurrent adds failed: %v", err)
	}
	if len(v) == 0 {
		t.Fatal("expected at least 1 sample to be recorded")
	}
	// Verify timestamps are in non-decreasing order
	for i := 1; i < len(v); i++ {
		if v[i].At.Before(v[i-1].At) {
			t.Errorf("history not sorted after concurrency: %v before %v", v[i].At, v[i-1].At)
		}
	}
}

func TestHistoryEmptyFileReturnsError(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(p, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	h := History{Path: p}
	_, err := h.Load()
	if err == nil {
		t.Error("expected error loading empty JSON file, got nil")
	}
}

func TestGrowthExtremeValuesNoPanic(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Extreme op count and byte counts (near 1 Exabyte or MaxInt64/4)
	huge := Sample{
		First:      base,
		Last:       base.Add(365 * 24 * time.Hour),
		Ops:        100_000_000_000,
		DBBytes:    1 << 50, // 1 Petabyte
		BlobBytes:  1 << 52, // 4 Petabytes
		ActiveDays: 365,
	}

	p := Project(huge, true)
	if p.YearOps <= 0 || p.YearDB <= 0 || p.YearBlobs <= 0 {
		t.Errorf("expected positive projections for large valid values, got: %+v", p)
	}

	pAct := Project(huge, false)
	if pAct.YearOps <= 0 || pAct.YearDB <= 0 {
		t.Errorf("expected positive projections on active days basis, got: %+v", pAct)
	}
}

func TestCheckZeroDurationSafetyAndFutureSamples(t *testing.T) {
	t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	now := sample(30, 100*mb, 100*mb)
	now.At = t0

	// History with future timestamp (corrupted clock or clock jump)
	futureHist := []Sample{
		{At: t0.Add(24 * time.Hour), DBBytes: 200 * mb},   // in future
		{At: t0.Add(10 * time.Minute), DBBytes: 150 * mb}, // in future
	}

	// Should not panic or calculate negative duration
	alarms := Check(now, futureHist, Limits{MaxDBPerDay: 5 * mb})
	if len(alarms) != 0 {
		t.Errorf("future history must not trigger rate alarms, got: %+v", alarms)
	}

	// History sample with identical timestamp to now
	exactHist := []Sample{
		{At: t0, DBBytes: 50 * mb},
	}
	alarms = Check(now, exactHist, Limits{MaxDBPerDay: 5 * mb})
	if len(alarms) != 0 {
		t.Errorf("zero-duration history must not trigger rate alarms, got: %+v", alarms)
	}
}

func TestHistoryLoadCorruptedVariations(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name    string
		content []byte
	}{
		{"null-byte", []byte("[\x00]")},
		{"unclosed-array", []byte("[{\"at\":\"2026-01-01T00:00:00Z\"}")},
		{"scalar-number", []byte("12345")},
		{"json-null", []byte("null")},
		{"array-of-nulls", []byte("[null, null]")},
		{"trailing-garbage", []byte("[]garbage")},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(dir, c.name+".json")
			if err := os.WriteFile(p, c.content, 0o600); err != nil {
				t.Fatal(err)
			}
			h := History{Path: p}
			v, err := h.Load()
			// Either an error or handled gracefully without panic
			if c.name == "json-null" {
				if err != nil || v != nil {
					t.Errorf("null json should yield nil slice, got %v / %v", v, err)
				}
			} else if c.name == "array-of-nulls" {
				if err != nil {
					t.Logf("array of nulls returned error: %v", err)
				}
			} else {
				if err == nil {
					t.Errorf("%s: expected error, got nil", c.name)
				}
			}
		})
	}
}
