package soak

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

// TestEdgeLimitsBoundary verifies exact boundary conditions for MinSamples
// and MinDuration.
func TestEdgeLimitsBoundary(t *testing.T) {
	// 19 samples over 11 minutes -> TOO SHORT (needs 20)
	{
		var b strings.Builder
		for i := 0; i < 19; i++ {
			s := Sample{
				At:         t0.Add(time.Duration(i) * 35 * time.Second),
				Role:       "run",
				Goroutines: 10,
				RSSKB:      25000,
			}
			j, _ := json.Marshal(s)
			b.WriteString(string(j) + "\n")
		}
		r, err := Summarize(strings.NewReader(b.String()), DefaultLimits)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if r.Verdict != "TOO SHORT" {
			t.Errorf("19 samples got %s, want TOO SHORT", r.Verdict)
		}
	}

	// 20 samples over 9m50s -> TOO SHORT (needs 10m)
	{
		var b strings.Builder
		for i := 0; i < 20; i++ {
			s := Sample{
				At:         t0.Add(time.Duration(i) * 29 * time.Second),
				Role:       "run",
				Goroutines: 10,
				RSSKB:      25000,
			}
			j, _ := json.Marshal(s)
			b.WriteString(string(j) + "\n")
		}
		r, err := Summarize(strings.NewReader(b.String()), DefaultLimits)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if r.Verdict != "TOO SHORT" {
			t.Errorf("9m50s duration got %s, want TOO SHORT", r.Verdict)
		}
	}

	// 20 samples over 10m10s -> PASS
	{
		var b strings.Builder
		for i := 0; i < 20; i++ {
			s := Sample{
				At:         t0.Add(time.Duration(i) * 32 * time.Second),
				Role:       "run",
				Goroutines: 10,
				RSSKB:      25000,
			}
			j, _ := json.Marshal(s)
			b.WriteString(string(j) + "\n")
		}
		r, err := Summarize(strings.NewReader(b.String()), DefaultLimits)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if r.Verdict != "PASS" {
			t.Errorf("20 samples / 10m got %s, want PASS", r.Verdict)
		}
	}
}

// TestEdgeGrowthExactThresholds tests exact boundary comparisons for
// MaxGoroutineGrowth (+5) and MaxRSSGrowthPct (+20%).
func TestEdgeGrowthExactThresholds(t *testing.T) {
	// Baseline goroutines 10. End goroutines 15 (+5) -> PASS.
	{
		g := func(i int) int {
			if i < 90 {
				return 10
			}
			return 15 // exactly +5
		}
		r := summarize(t, series(100, g, flat64(30000)))
		if r.Verdict != "PASS" {
			t.Errorf("goroutine growth +5 got %s, want PASS", r.Verdict)
		}
	}

	// Baseline goroutines 10. End goroutines 16 (+6) -> FAIL.
	{
		g := func(i int) int {
			if i < 90 {
				return 10
			}
			return 16 // +6
		}
		r := summarize(t, series(100, g, flat64(30000)))
		if r.Verdict != "FAIL" {
			t.Errorf("goroutine growth +6 got %s, want FAIL", r.Verdict)
		}
	}

	// RSS baseline 100,000 KB. End 120,000 KB (exactly 20%) -> PASS.
	{
		rss := func(i int) int64 {
			if i < 90 {
				return 100000
			}
			return 120000
		}
		r := summarize(t, series(100, flat(10), rss))
		if r.Verdict != "PASS" {
			t.Errorf("RSS growth +20%% got %s, want PASS", r.Verdict)
		}
	}

	// RSS baseline 100,000 KB. End 120,100 KB (> 20%) -> FAIL.
	{
		rss := func(i int) int64 {
			if i < 90 {
				return 100000
			}
			return 120100
		}
		r := summarize(t, series(100, flat(10), rss))
		if r.Verdict != "FAIL" {
			t.Errorf("RSS growth +20.1%% got %s, want FAIL", r.Verdict)
		}
	}
}

// TestEdgeFailureRateBoundaries tests failure threshold (> 50%).
func TestEdgeFailureRateBoundaries(t *testing.T) {
	// 49 failures out of 100 rounds -> PASS
	{
		var b strings.Builder
		for i := 0; i < 100; i++ {
			failures := int64(0)
			if i >= 51 {
				failures = int64(i - 50)
			}
			s := Sample{
				At:         t0.Add(time.Duration(i) * 10 * time.Second),
				Goroutines: 10,
				RSSKB:      30000,
				Rounds:     100,
				Failures:   failures,
			}
			j, _ := json.Marshal(s)
			b.WriteString(string(j) + "\n")
		}
		r := summarize(t, b.String())
		if r.Verdict != "PASS" {
			t.Errorf("49/100 failures got %s, want PASS", r.Verdict)
		}
	}

	// 51 failures out of 100 rounds -> FAIL
	{
		var b strings.Builder
		for i := 0; i < 100; i++ {
			s := Sample{
				At:         t0.Add(time.Duration(i) * 10 * time.Second),
				Goroutines: 10,
				RSSKB:      30000,
				Rounds:     100,
				Failures:   51,
			}
			j, _ := json.Marshal(s)
			b.WriteString(string(j) + "\n")
		}
		r := summarize(t, b.String())
		if r.Verdict != "FAIL" {
			t.Errorf("51/100 failures got %s, want FAIL", r.Verdict)
		}
	}
}

// TestEdgeRunNegativeOrZeroInterval tests error on invalid intervals.
func TestEdgeRunNegativeOrZeroInterval(t *testing.T) {
	ctx := context.Background()
	if err := Run(ctx, io.Discard, "test", 0, nil); err == nil {
		t.Error("expected error on 0 interval")
	}
	if err := Run(ctx, io.Discard, "test", -5*time.Second, nil); err == nil {
		t.Error("expected error on negative interval")
	}
}
