package jobqueue

import (
	"math"
	"testing"
	"time"
)

func TestCeilingDoublesThenCaps(t *testing.T) {
	b := Backoff{Base: 2 * time.Second, Max: 60 * time.Second}
	want := []time.Duration{2, 4, 8, 16, 32, 60, 60, 60}
	for i, w := range want {
		if got := b.Ceiling(i + 1); got != w*time.Second {
			t.Errorf("attempt %d: ceiling %v, want %v", i+1, got, w*time.Second)
		}
	}
}

func TestCeilingNeverOverflows(t *testing.T) {
	b := Backoff{Base: time.Second, Max: 5 * time.Minute}
	for _, attempt := range []int{40, 63, 64, 65, 1000, math.MaxInt32} {
		if got := b.Ceiling(attempt); got != 5*time.Minute {
			t.Errorf("attempt %d: %v, want the cap", attempt, got)
		}
	}
	// A huge base and cap must not overflow either.
	huge := Backoff{Base: time.Duration(math.MaxInt64 / 3), Max: time.Duration(math.MaxInt64 - 1)}
	if got := huge.Ceiling(10); got <= 0 || got > huge.Max {
		t.Errorf("huge ceiling = %v", got)
	}
}

func TestCeilingTreatsBadAttemptAsFirst(t *testing.T) {
	b := Backoff{Base: 3 * time.Second, Max: time.Minute}
	for _, a := range []int{0, -1, -100} {
		if got := b.Ceiling(a); got != 3*time.Second {
			t.Errorf("attempt %d: %v", a, got)
		}
	}
}

func TestNoJitterIsExact(t *testing.T) {
	b := Backoff{Base: time.Second, Max: time.Minute, Jitter: NoJitter}
	for _, r := range []float64{0, 0.5, 0.999} {
		if got := b.Delay(3, r); got != 4*time.Second {
			t.Errorf("r=%v: %v, want 4s regardless of randomness", r, got)
		}
	}
}

func TestEqualJitterRange(t *testing.T) {
	b := Backoff{Base: 10 * time.Second, Max: time.Hour, Jitter: EqualJitter}
	if got := b.Delay(1, 0); got != 5*time.Second {
		t.Errorf("r=0: %v, want half the ceiling (5s)", got)
	}
	if got := b.Delay(1, 0.5); got != 7500*time.Millisecond {
		t.Errorf("r=0.5: %v, want 7.5s", got)
	}
	if got := b.Delay(1, 0.9999999); got < 9990*time.Millisecond || got > 10*time.Second {
		t.Errorf("r~1: %v, want just under 10s", got)
	}
}

func TestFullJitterRange(t *testing.T) {
	b := Backoff{Base: 10 * time.Second, Max: time.Hour, Jitter: FullJitter}
	if got := b.Delay(1, 0); got != 0 {
		t.Errorf("r=0: %v, want 0", got)
	}
	if got := b.Delay(1, 0.25); got != 2500*time.Millisecond {
		t.Errorf("r=0.25: %v, want 2.5s", got)
	}
}

func TestDelayClampsBadRandomness(t *testing.T) {
	b := Backoff{Base: 10 * time.Second, Max: time.Hour, Jitter: FullJitter}
	if got := b.Delay(1, -5); got != 0 {
		t.Errorf("r<0: %v", got)
	}
	if got := b.Delay(1, 7); got > 10*time.Second {
		t.Errorf("r>=1 must not exceed the ceiling: %v", got)
	}
}

// The properties that matter, over every mode and many attempts.
func TestDelayAlwaysWithinBounds(t *testing.T) {
	for _, j := range []Jitter{NoJitter, EqualJitter, FullJitter} {
		b := Backoff{Base: 500 * time.Millisecond, Max: 30 * time.Second, Jitter: j}
		for attempt := 1; attempt <= 80; attempt++ {
			c := b.Ceiling(attempt)
			for r := 0.0; r < 1; r += 0.05 {
				d := b.Delay(attempt, r)
				if d < 0 || d > c {
					t.Fatalf("%v attempt %d r=%.2f: delay %v outside [0, %v]", j, attempt, r, d, c)
				}
				if j == EqualJitter && d < c/2 {
					t.Fatalf("equal jitter attempt %d r=%.2f: %v below half the ceiling %v", attempt, r, d, c)
				}
				if j == NoJitter && d != c {
					t.Fatalf("no jitter must equal the ceiling")
				}
			}
			if attempt > 1 && b.Ceiling(attempt) < b.Ceiling(attempt-1) {
				t.Fatalf("ceiling decreased at attempt %d", attempt)
			}
		}
	}
}

func TestBackoffValidate(t *testing.T) {
	bad := []Backoff{{}, {Base: -1, Max: time.Second}, {Base: time.Second, Max: time.Millisecond}, {Base: time.Second, Max: time.Minute, Jitter: 9}}
	for _, b := range bad {
		if b.Validate() == nil {
			t.Errorf("%+v should be invalid", b)
		}
	}
	if err := DefaultBackoff().Validate(); err != nil {
		t.Error(err)
	}
}

func TestParseJitter(t *testing.T) {
	for s, want := range map[string]Jitter{"none": NoJitter, "equal": EqualJitter, "full": FullJitter} {
		if got, err := ParseJitter(s); err != nil || got != want || got.String() != s {
			t.Errorf("%s: %v %v", s, got, err)
		}
	}
	if _, err := ParseJitter("wild"); err == nil {
		t.Error("unknown jitter must be refused")
	}
}

// The reason jitter exists, as a test: a thousand clients that all failed at
// the same moment. Without jitter they all retry at the same instant; with it
// they spread out.
func TestJitterSpreadsAHerd(t *testing.T) {
	const clients = 1000
	spread := func(j Jitter) (distinct int, maxPerSecond int) {
		b := Backoff{Base: 4 * time.Second, Max: time.Minute, Jitter: j}
		seen := map[time.Duration]bool{}
		perSecond := map[int]int{}
		for i := 0; i < clients; i++ {
			d := b.Delay(3, (float64(i)+0.5)/clients) // evenly spread randomness: deterministic
			seen[d] = true
			perSecond[int(d/time.Second)]++
		}
		for _, n := range perSecond {
			if n > maxPerSecond {
				maxPerSecond = n
			}
		}
		return len(seen), maxPerSecond
	}
	dn, pn := spread(NoJitter)
	de, pe := spread(EqualJitter)
	df, pf := spread(FullJitter)
	if dn != 1 || pn != clients {
		t.Errorf("no jitter: %d distinct delays, %d in the busiest second; want 1 and %d (a retry storm)", dn, pn, clients)
	}
	if de < 500 || pe > clients/4 {
		t.Errorf("equal jitter: %d distinct, busiest second %d", de, pe)
	}
	if df < 500 || pf > clients/10 {
		t.Errorf("full jitter: %d distinct, busiest second %d", df, pf)
	}
	if pf > pe {
		t.Errorf("full jitter (%d) should spread at least as much as equal (%d)", pf, pe)
	}
}

// Ceiling is exported, so it must be safe even for a Backoff nobody validated.
func TestCeilingNeverExceedsMaxEvenIfBaseDoes(t *testing.T) {
	b := Backoff{Base: 10 * time.Second, Max: 5 * time.Second}
	if got := b.Ceiling(1); got != 5*time.Second {
		t.Errorf("Ceiling(1) = %v, want the max (5s)", got)
	}
}
