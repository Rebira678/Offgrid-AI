package soak

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSlopePerHourMathematicalEdges(t *testing.T) {
	at := func(min int, kb int64) Sample {
		return Sample{At: t0.Add(time.Duration(min) * time.Minute), RSSKB: kb}
	}

	// 1. Negative slope (memory shrinking due to GC)
	// Drops 600 KB over 1 hour -> slope -600 KB/h
	negSamples := []Sample{
		at(0, 50000),
		at(20, 49800),
		at(40, 49600),
		at(60, 49400),
	}
	slopeNeg := slopePerHour(negSamples)
	if slopeNeg > -590 || slopeNeg < -610 {
		t.Errorf("expected slope around -600 KB/h, got %f", slopeNeg)
	}

	// 2. Identical timestamps (zero time span)
	sameTime := []Sample{
		at(10, 1000),
		at(10, 2000),
	}
	if s := slopePerHour(sameTime); s != 0 {
		t.Errorf("expected 0 slope for identical timestamps, got %f", s)
	}

	// 3. Exactly horizontal line
	flatSamples := []Sample{
		at(0, 10000),
		at(30, 10000),
		at(60, 10000),
	}
	if s := slopePerHour(flatSamples); s != 0 {
		t.Errorf("expected 0 slope for perfectly flat samples, got %f", s)
	}
}

func TestSummarizeEdgePeakAndOutdatedOrder(t *testing.T) {
	// Reverse order samples: Summarize must sort them by time before analyzing
	sRev := []Sample{
		{At: t0.Add(20 * time.Minute), Role: "run", Goroutines: 10, RSSKB: 30000},
		{At: t0.Add(10 * time.Minute), Role: "run", Goroutines: 10, RSSKB: 29000},
		{At: t0, Role: "run", Goroutines: 10, RSSKB: 28000},
	}
	var b strings.Builder
	for _, s := range sRev {
		j, _ := json.Marshal(s)
		b.Write(j)
		b.WriteByte('\n')
	}

	// With insufficient samples (< MinSamples), returns TOO SHORT
	lim := DefaultLimits
	lim.MinSamples = 2
	lim.MinDuration = 5 * time.Minute
	r, err := Summarize(strings.NewReader(b.String()), lim)
	if err != nil {
		t.Fatal(err)
	}
	if r.Duration < 19*time.Minute {
		t.Errorf("expected span ~20 minutes after sorting, got %v", r.Duration)
	}
}

func TestSummarizeMaxRSSPeakDuringWarmUpFailsBudget(t *testing.T) {
	// 30 samples spanning 15 minutes
	// Samples 0-2 (warm-up) spike to 80,000 KB
	// Remaining samples stay at 30,000 KB
	// With budget 50,000 KB, peak warm-up MUST trigger budget failure
	var b strings.Builder
	for i := 0; i < 30; i++ {
		rss := int64(30000)
		if i == 1 {
			rss = 80000
		}
		s := Sample{
			At:         t0.Add(time.Duration(i) * 30 * time.Second),
			Role:       "run",
			Goroutines: 10,
			RSSKB:      rss,
		}
		j, _ := json.Marshal(s)
		b.Write(j)
		b.WriteByte('\n')
	}

	lim := DefaultLimits
	lim.MinSamples = 20
	lim.MinDuration = 10 * time.Minute
	lim.MaxRSSKB = 50000
	r, err := Summarize(strings.NewReader(b.String()), lim)
	if err != nil {
		t.Fatal(err)
	}
	if r.Verdict != "FAIL" {
		t.Fatalf("expected FAIL for peak RSS budget violation during warm-up, got %s", r.Verdict)
	}
	found := false
	for _, p := range r.Problems {
		if strings.Contains(p, "peaked at 80000 KB, over the budget of 50000 KB") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("budget failure message not found in problems: %v", r.Problems)
	}
}

func TestSummarizeExtremeLinesAndLargeScanners(t *testing.T) {
	// Sample with extraneous forward-compatible JSON keys and nulls
	line := `{"at":"2026-11-12T09:00:00Z","role":"run","goroutines":15,"rss_kb":45000,"extra_future_field":"unknown_meta","nested":{"id":123}}` + "\n"
	r, err := Summarize(strings.NewReader(line), Limits{MinSamples: 1, MinDuration: 0})
	if err != nil {
		t.Fatalf("Summarize failed on valid forward-compatible JSON: %v", err)
	}
	if r.GoroutinesStart != 15 || r.RSSStartKB != 45000 {
		t.Fatalf("parsed values mismatch: goroutines=%d, rss=%d", r.GoroutinesStart, r.RSSStartKB)
	}
}

func TestSummarizeWarmUpBoundaries(t *testing.T) {
	// WarmUp = 0.0 (no warm-up skipping)
	var b strings.Builder
	for i := 0; i < 20; i++ {
		s := Sample{
			At:         t0.Add(time.Duration(i) * time.Minute),
			Role:       "run",
			Goroutines: 10,
			RSSKB:      30000,
		}
		j, _ := json.Marshal(s)
		b.Write(j)
		b.WriteByte('\n')
	}

	lim := DefaultLimits
	lim.WarmUp = 0.0
	lim.MinSamples = 10
	lim.MinDuration = 10 * time.Minute
	r, err := Summarize(strings.NewReader(b.String()), lim)
	if err != nil {
		t.Fatal(err)
	}
	if r.Verdict != "PASS" {
		t.Fatalf("expected PASS with 0.0 warm-up, got %s: %v", r.Verdict, r.Problems)
	}
}

func BenchmarkSlopePerHour(b *testing.B) {
	samples := make([]Sample, 1000)
	for i := 0; i < 1000; i++ {
		samples[i] = Sample{
			At:    t0.Add(time.Duration(i) * 10 * time.Second),
			RSSKB: int64(30000 + (i%50)*10),
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = slopePerHour(samples)
	}
}
