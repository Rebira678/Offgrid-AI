// Package hlc is a hybrid logical clock, after Kulkarni, Demirbas, Madappa,
// Avva and Leone, "Logical Physical Clocks and Consistent Snapshots in
// Globally Distributed Databases" (2014).
//
// The problem: replicas that edit offline must agree on "which edit was
// later". A wall clock cannot say that. Phones are minutes wrong, a clock can
// be set backwards, and two devices can disagree by more than the gap between
// two edits, so "later by the clock" sometimes means "earlier in reality" and
// a newer edit silently loses. A pure logical (Lamport) clock orders causally
// but is not tied to real time, so a person cannot read it.
//
// A hybrid logical clock keeps both. A timestamp is a pair (Wall, Counter):
//
//	Wall     the largest physical time this node has seen, from its own clock
//	         or from any timestamp it received (nanoseconds since the Unix epoch)
//	Counter  breaks ties between events that share a Wall
//
// Timestamps compare lexicographically. The guarantees, each tested:
//
//   - Monotonic: every timestamp a clock issues is greater than the last,
//     even if the physical clock stops, jumps back, or is reset.
//   - Causal: if event A could have influenced event B (B's node had received
//     A's timestamp), then ts(A) < ts(B).
//   - Close to real time: Wall never runs ahead of the largest physical clock
//     in the system, so it stays within the skew between the clocks, and the
//     Counter stays small.
//
// What it does not do: it cannot tell you the true order of two events that
// are concurrent (neither could have influenced the other). It picks one,
// deterministically, and that is all a last-writer-wins merge needs.
package hlc

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Timestamp is a hybrid logical time. The zero value means "never".
type Timestamp struct {
	Wall    int64  // nanoseconds since the Unix epoch, never negative
	Counter uint32 // orders events with the same Wall
}

// IsZero reports whether t is the zero timestamp.
func (t Timestamp) IsZero() bool { return t == Timestamp{} }

// Compare returns -1, 0 or +1 as t is before, equal to or after u.
func (t Timestamp) Compare(u Timestamp) int {
	switch {
	case t.Wall < u.Wall:
		return -1
	case t.Wall > u.Wall:
		return 1
	case t.Counter < u.Counter:
		return -1
	case t.Counter > u.Counter:
		return 1
	}
	return 0
}

// Before reports whether t is strictly before u.
func (t Timestamp) Before(u Timestamp) bool { return t.Compare(u) < 0 }

// After reports whether t is strictly after u.
func (t Timestamp) After(u Timestamp) bool { return t.Compare(u) > 0 }

// Max returns the later of t and u.
func Max(t, u Timestamp) Timestamp {
	if u.After(t) {
		return u
	}
	return t
}

// Time is the wall part as a time.Time (UTC). It is what a person should see:
// the moment the node believed it was, or the latest moment it had heard of.
func (t Timestamp) Time() time.Time { return time.Unix(0, t.Wall).UTC() }

// String is RFC 3339 with nanoseconds, then '#' and the counter:
// 2026-10-18T11:03:00.123456789Z#7. Parse reads it back.
func (t Timestamp) String() string {
	return t.Time().Format(time.RFC3339Nano) + "#" + strconv.FormatUint(uint64(t.Counter), 10)
}

// Parse reads the form String produces.
func Parse(s string) (Timestamp, error) {
	i := strings.LastIndexByte(s, '#')
	if i < 0 {
		return Timestamp{}, fmt.Errorf("hlc: %q has no '#counter'", s)
	}
	at, err := time.Parse(time.RFC3339Nano, s[:i])
	if err != nil {
		return Timestamp{}, fmt.Errorf("hlc: %q: bad time: %w", s, err)
	}
	c, err := strconv.ParseUint(s[i+1:], 10, 32)
	if err != nil {
		return Timestamp{}, fmt.Errorf("hlc: %q: bad counter", s)
	}
	if at.Before(time.Unix(0, 0)) {
		return Timestamp{}, fmt.Errorf("hlc: %q is before 1970", s)
	}
	return Timestamp{Wall: at.UnixNano(), Counter: uint32(c)}, nil
}

// ErrDrift means a received timestamp was too far ahead of this node's own
// physical clock to be folded in. Accepting it would drag this node's clock
// (and so every timestamp it issues afterwards) into the future.
var ErrDrift = errors.New("hlc: remote timestamp is too far ahead of the local clock")

// DriftError says how far ahead the rejected timestamp was.
type DriftError struct {
	Remote Timestamp
	Ahead  time.Duration // remote wall minus local physical time
	Limit  time.Duration
}

func (e *DriftError) Error() string {
	return fmt.Sprintf("hlc: remote timestamp %s is %s ahead of the local clock (limit %s)", e.Remote, e.Ahead.Round(time.Millisecond), e.Limit)
}

func (e *DriftError) Is(target error) bool { return target == ErrDrift }

// DefaultMaxDrift is how far ahead of the local clock a received timestamp may
// be before Update refuses to fold it in.
const DefaultMaxDrift = time.Hour

// NoDriftLimit, as Config.MaxDrift, accepts any timestamp.
const NoDriftLimit time.Duration = -1

// Config configures a Clock.
type Config struct {
	// Now is the physical clock. nil means time.Now. Tests inject a fake.
	Now func() time.Time
	// MaxDrift bounds how far ahead of Now a received timestamp may be. Zero
	// means DefaultMaxDrift; NoDriftLimit disables the check.
	MaxDrift time.Duration
}

// Clock issues timestamps. It is safe for concurrent use.
type Clock struct {
	now      func() time.Time
	maxDrift time.Duration

	mu   sync.Mutex
	last Timestamp
}

// New makes a Clock whose first timestamp is based on the physical clock alone.
// If the clock is backed by stored data, call Restore with the newest
// timestamp already issued, so a restart can never go backwards.
func New(cfg Config) *Clock {
	c := &Clock{now: cfg.Now, maxDrift: cfg.MaxDrift}
	if c.now == nil {
		c.now = time.Now
	}
	if c.maxDrift == 0 {
		c.maxDrift = DefaultMaxDrift
	}
	return c
}

func (c *Clock) physical() int64 {
	ns := c.now().UnixNano()
	if ns < 0 { // a clock set before 1970 is a broken clock; treat it as the epoch
		return 0
	}
	return ns
}

// tick returns the timestamp after t with the counter advanced. If the counter
// is exhausted the wall moves one nanosecond instead, which keeps the order.
func tick(t Timestamp) Timestamp {
	if t.Counter == ^uint32(0) {
		return Timestamp{Wall: t.Wall + 1}
	}
	return Timestamp{Wall: t.Wall, Counter: t.Counter + 1}
}

// Now timestamps a local event or a message about to be sent.
//
//	l' = max(l, pt);  c = c+1 if l' == l, else 0
func (c *Clock) Now() Timestamp {
	pt := c.physical()
	c.mu.Lock()
	defer c.mu.Unlock()
	if pt > c.last.Wall {
		c.last = Timestamp{Wall: pt}
	} else {
		c.last = tick(c.last)
	}
	return c.last
}

// Update folds in a timestamp received from another node and timestamps the
// receive event. The result is after both the local last timestamp and remote,
// which is what makes the clock causal.
//
//	l' = max(l, l_m, pt)
//	c  = max(c, c_m)+1  if l' == l == l_m
//	     c+1            if l' == l
//	     c_m+1          if l' == l_m
//	     0              otherwise
//
// If remote is further ahead of the local physical clock than MaxDrift, Update
// returns a *DriftError (it matches ErrDrift) and changes nothing. The caller
// still has the remote data; what it must not do is let one wrong clock pull
// every later timestamp into the future. A refused timestamp has a real
// consequence: events this node makes afterwards may be ordered before it,
// and a last-writer-wins merge will then prefer the remote value.
func (c *Clock) Update(remote Timestamp) (Timestamp, error) {
	pt := c.physical()
	if c.maxDrift > 0 && remote.Wall > pt && time.Duration(remote.Wall-pt) > c.maxDrift {
		return Timestamp{}, &DriftError{Remote: remote, Ahead: time.Duration(remote.Wall - pt), Limit: c.maxDrift}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	l, lm := c.last, remote
	var out Timestamp
	switch wall := max(l.Wall, lm.Wall, pt); {
	case wall == l.Wall && wall == lm.Wall:
		out = tick(Timestamp{Wall: wall, Counter: max(l.Counter, lm.Counter)})
	case wall == l.Wall:
		out = tick(l)
	case wall == lm.Wall:
		out = tick(lm)
	default:
		out = Timestamp{Wall: wall}
	}
	c.last = out
	return out, nil
}

// Last is the most recent timestamp issued, or restored.
func (c *Clock) Last() Timestamp {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

// Restore raises the clock to at least t. It never lowers it. Call it with the
// newest timestamp already stored, when a node starts up: if the physical
// clock is behind what was already issued (a dead battery, a reset, a step
// back by NTP), the next timestamp is still after all of them.
func (c *Clock) Restore(t Timestamp) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last = Max(c.last, t)
}

// Ahead is how far the last timestamp's wall is beyond the physical clock now.
// It is zero when the physical clock is at or past it. A large value means the
// physical clock was once far ahead (or is now far behind) and the node is
// issuing timestamps from the future: worth telling a person.
func (c *Clock) Ahead() time.Duration {
	pt := c.physical()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last.Wall <= pt {
		return 0
	}
	return time.Duration(c.last.Wall - pt)
}
