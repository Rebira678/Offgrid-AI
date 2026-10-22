package hlc

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// TestClockHighConcurrencyMonotonicity runs 50 concurrent goroutines stamping 500
// events each (25,000 total). It verifies zero duplicates and strict global monotonicity.
func TestClockHighConcurrencyMonotonicity(t *testing.T) {
	c := New(Config{})
	const numGoroutines = 50
	const opsPerGoroutine = 500
	const total = numGoroutines * opsPerGoroutine

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	results := make([][]Timestamp, numGoroutines)
	for g := 0; g < numGoroutines; g++ {
		g := g
		results[g] = make([]Timestamp, opsPerGoroutine)
		go func() {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				results[g][i] = c.Now()
			}
		}()
	}
	wg.Wait()

	// 1. Per-goroutine strict monotonicity
	for g := 0; g < numGoroutines; g++ {
		for i := 1; i < opsPerGoroutine; i++ {
			if !results[g][i].After(results[g][i-1]) {
				t.Fatalf("goroutine %d issued non-monotonic timestamps: %v then %v",
					g, results[g][i-1], results[g][i])
			}
		}
	}

	// 2. Global uniqueness across all goroutines
	seen := make(map[Timestamp]struct{}, total)
	for g := 0; g < numGoroutines; g++ {
		for i := 0; i < opsPerGoroutine; i++ {
			ts := results[g][i]
			if _, exists := seen[ts]; exists {
				t.Fatalf("duplicate timestamp issued concurrently: %v", ts)
			}
			seen[ts] = struct{}{}
		}
	}
	if len(seen) != total {
		t.Fatalf("expected %d unique timestamps, got %d", total, len(seen))
	}
}

// TestClockDriftRejectionExactBoundary tests the precise nanosecond boundary
// of MaxDrift enforcement in Update.
func TestClockDriftRejectionExactBoundary(t *testing.T) {
	now := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	limit := 5 * time.Minute
	c := New(Config{
		Now:      func() time.Time { return now },
		MaxDrift: limit,
	})

	pt := now.UnixNano()

	// 1. Below limit: pt + limit - 1ns -> must succeed
	tsBelow := Timestamp{Wall: pt + int64(limit) - 1, Counter: 0}
	if _, err := c.Update(tsBelow); err != nil {
		t.Fatalf("expected accept at limit - 1ns, got err: %v", err)
	}

	// 2. Exact limit: pt + limit -> must succeed (condition is > maxDrift)
	tsExact := Timestamp{Wall: pt + int64(limit), Counter: 0}
	if _, err := c.Update(tsExact); err != nil {
		t.Fatalf("expected accept at exact limit, got err: %v", err)
	}

	// 3. Above limit: pt + limit + 1ns -> must be rejected with ErrDrift
	tsAbove := Timestamp{Wall: pt + int64(limit) + 1, Counter: 0}
	_, err := c.Update(tsAbove)
	if err == nil {
		t.Fatalf("expected rejection at limit + 1ns, got nil error")
	}
	if !errors.Is(err, ErrDrift) {
		t.Fatalf("expected ErrDrift, got: %v", err)
	}
	var driftErr *DriftError
	if !errors.As(err, &driftErr) {
		t.Fatalf("expected *DriftError type, got: %T", err)
	}
	if driftErr.Limit != limit {
		t.Fatalf("expected DriftError.Limit = %v, got %v", limit, driftErr.Limit)
	}
}

// TestClockRestoreIdempotenceAndMonotonicity verifies that Restore raises the clock
// to the newest timestamp and ignores older or zero timestamps.
func TestClockRestoreIdempotenceAndMonotonicity(t *testing.T) {
	c := New(Config{})
	initial := c.Now()

	// Restore older timestamp: should have zero effect
	c.Restore(Timestamp{Wall: initial.Wall - 1000, Counter: 0})
	if c.Last() != initial {
		t.Fatalf("Restore lowered clock: initial=%v, now=%v", initial, c.Last())
	}

	// Restore zero timestamp: should have zero effect
	c.Restore(Timestamp{})
	if c.Last() != initial {
		t.Fatalf("Restore with zero changed clock: %v", c.Last())
	}

	// Restore newer timestamp: should advance Last()
	future := Timestamp{Wall: initial.Wall + 50000, Counter: 10}
	c.Restore(future)
	if c.Last() != future {
		t.Fatalf("Restore failed to advance to future stamp: %v", c.Last())
	}

	// Next Now() must be strictly greater than future
	next := c.Now()
	if !next.After(future) {
		t.Fatalf("Next timestamp %v is not after restored future stamp %v", next, future)
	}
}

// TestTimestampRFC3339SubNanosecondParseRoundTrip verifies serialization
// and deserialization round trips without precision loss or parse errors.
func TestTimestampRFC3339SubNanosecondParseRoundTrip(t *testing.T) {
	stamps := []Timestamp{
		{Wall: 0, Counter: 0},
		{Wall: 1, Counter: 1},
		{Wall: 1700000000123456789, Counter: 42},
		{Wall: 1700000000000000001, Counter: 999},
		{Wall: 1700000000999999999, Counter: 4294967295},
	}

	for _, ts := range stamps {
		str := ts.String()
		parsed, err := Parse(str)
		if err != nil {
			t.Fatalf("Parse(%q) failed: %v", str, err)
		}
		if parsed != ts {
			t.Fatalf("round-trip mismatch: got %v (%s), want %v", parsed, str, ts)
		}
	}

	// Invalid strings
	invalids := []string{
		"",
		"not-a-timestamp",
		"2026-10-22T10:00:00Z", // missing #counter
		"2026-10-22T10:00:00Z#-1",
		"2026-10-22T10:00:00Z#99999999999999999999", // counter overflow
		"1969-12-31T23:59:59Z#0",                    // before 1970
	}
	for _, bad := range invalids {
		if _, err := Parse(bad); err == nil {
			t.Errorf("expected Parse(%q) to fail, got nil error", bad)
		}
	}
}
