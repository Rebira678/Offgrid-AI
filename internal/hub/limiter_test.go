package hub

import (
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func TestTheLimiterSpendsItsBurstThenRefillsAtItsRate(t *testing.T) {
	c := &clock{time.Unix(1_000_000, 0)}
	l := NewLimiter(2, 3, c.now) // 2 per second, burst 3
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow(); !ok {
			t.Fatalf("request %d of the burst refused", i+1)
		}
	}
	ok, retry := l.Allow()
	if ok || retry != 500*time.Millisecond {
		t.Fatalf("4th request: ok=%v retry=%v, want refused with 500ms to the next token", ok, retry)
	}
	c.t = c.t.Add(500 * time.Millisecond)
	if ok, _ := l.Allow(); !ok {
		t.Fatal("a token should have refilled after 500ms")
	}
	if ok, _ := l.Allow(); ok {
		t.Fatal("only one token had refilled")
	}
	// a long quiet time refills to the burst and no further
	c.t = c.t.Add(time.Hour)
	n := 0
	for ok, _ := l.Allow(); ok; ok, _ = l.Allow() {
		n++
	}
	if n != 3 {
		t.Fatalf("after an hour the bucket held %d tokens, want the burst of 3", n)
	}
}

func TestTheLimiterIgnoresAClockThatStepsBack(t *testing.T) {
	c := &clock{time.Unix(1_000_000, 0)}
	l := NewLimiter(1, 2, c.now) // 1 per second, burst 2
	l.Allow()                    // one token left
	c.t = c.t.Add(-time.Hour)
	// a step back neither refills the bucket nor takes tokens away
	if ok, _ := l.Allow(); !ok {
		t.Fatal("the token that was left was lost when the clock stepped back")
	}
	if ok, _ := l.Allow(); ok {
		t.Fatal("a clock that went backwards refilled the bucket")
	}
	c.t = c.t.Add(time.Hour + 2*time.Second) // forward again, past where it was
	if ok, _ := l.Allow(); !ok {
		t.Fatal("no refill after time moved on")
	}
}

func TestTheLimiterTreatsNonsenseSettingsSafely(t *testing.T) {
	c := &clock{time.Unix(1, 0)}
	l := NewLimiter(-5, 0, c.now)
	if ok, _ := l.Allow(); !ok {
		t.Fatal("the first request is allowed")
	}
	if ok, retry := l.Allow(); ok || retry != time.Second {
		t.Fatalf("second: ok=%v retry=%v, want refused for 1s", ok, retry)
	}
}
