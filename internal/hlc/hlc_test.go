package hlc

import (
	"errors"
	"math"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is a physical clock the test moves by hand, in nanoseconds.
type fakeClock struct {
	mu sync.Mutex
	ns int64
}

func (f *fakeClock) Now() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return time.Unix(0, f.ns) }
func (f *fakeClock) Set(ns int64)   { f.mu.Lock(); f.ns = ns; f.mu.Unlock() }
func (f *fakeClock) Add(d time.Duration) {
	f.mu.Lock()
	f.ns += int64(d)
	f.mu.Unlock()
}

func ts(w int64, c uint32) Timestamp { return Timestamp{Wall: w, Counter: c} }

func newClock(pt int64) (*Clock, *fakeClock) {
	f := &fakeClock{ns: pt}
	return New(Config{Now: f.Now, MaxDrift: NoDriftLimit}), f
}

// ---- the timestamp type ----------------------------------------------------

func TestCompareIsLexicographicAndATotalOrder(t *testing.T) {
	cases := []struct {
		a, b Timestamp
		want int
	}{
		{ts(1, 0), ts(2, 0), -1},
		{ts(2, 0), ts(1, 0), 1},
		{ts(5, 1), ts(5, 2), -1},
		{ts(5, 2), ts(5, 1), 1},
		{ts(5, 3), ts(5, 3), 0},
		{ts(5, 99), ts(6, 0), -1}, // wall decides before the counter is looked at
		{ts(6, 0), ts(5, math.MaxUint32), 1},
		{Timestamp{}, ts(0, 1), -1},
	}
	for _, c := range cases {
		if got := c.a.Compare(c.b); got != c.want {
			t.Errorf("%v vs %v = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	// sorting by Compare agrees with sorting by the (wall, counter) pair
	rng := rand.New(rand.NewSource(1))
	var xs []Timestamp
	for i := 0; i < 500; i++ {
		xs = append(xs, ts(int64(rng.Intn(20)), uint32(rng.Intn(5))))
	}
	sort.Slice(xs, func(i, j int) bool { return xs[i].Compare(xs[j]) < 0 })
	for i := 1; i < len(xs); i++ {
		a, b := xs[i-1], xs[i]
		if a.Wall > b.Wall || (a.Wall == b.Wall && a.Counter > b.Counter) {
			t.Fatalf("not sorted at %d: %v then %v", i, a, b)
		}
		if a.Compare(b) > 0 || b.Compare(a) < 0 {
			t.Fatalf("not antisymmetric: %v %v", a, b)
		}
	}
	if !ts(1, 0).Before(ts(1, 1)) || ts(1, 1).Before(ts(1, 1)) || !ts(2, 0).After(ts(1, 9)) || ts(1, 0).After(ts(1, 0)) {
		t.Error("Before/After must be strict")
	}
	if Max(ts(1, 5), ts(1, 6)) != ts(1, 6) || Max(ts(3, 0), ts(2, 9)) != ts(3, 0) || Max(ts(4, 4), ts(4, 4)) != ts(4, 4) {
		t.Error("Max")
	}
}

func TestZeroAndTime(t *testing.T) {
	if !(Timestamp{}).IsZero() || ts(0, 1).IsZero() || ts(1, 0).IsZero() {
		t.Error("only the all-zero timestamp is zero")
	}
	got := ts(1_700_000_000_123_456_789, 4).Time()
	if got.Location() != time.UTC || got.UnixNano() != 1_700_000_000_123_456_789 {
		t.Errorf("Time() = %v", got)
	}
}

func TestStringAndParseRoundTrip(t *testing.T) {
	if s := ts(1_700_000_000_123_456_789, 7).String(); s != "2023-11-14T22:13:20.123456789Z#7" {
		t.Errorf("String = %q", s)
	}
	rng := rand.New(rand.NewSource(2))
	for i := 0; i < 2000; i++ {
		in := ts(rng.Int63n(4_000_000_000_000_000_000), rng.Uint32())
		out, err := Parse(in.String())
		if err != nil || out != in {
			t.Fatalf("%v -> %q -> %v, %v", in, in.String(), out, err)
		}
	}
	z, err := Parse(Timestamp{}.String())
	if err != nil || !z.IsZero() {
		t.Errorf("zero round trip: %v %v", z, err)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, s := range []string{
		"", "2026-10-18T11:03:00Z", "2026-10-18T11:03:00Z#", "2026-10-18T11:03:00Z#x", "2026-10-18T11:03:00Z#-1",
		"2026-10-18T11:03:00Z#4294967296", "yesterday#1", "1969-12-31T23:59:59Z#0", "#1",
	} {
		if v, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) = %v, want an error", s, v)
		}
	}
	if v, err := Parse("2026-10-18T11:03:00Z#4294967295"); err != nil || v.Counter != math.MaxUint32 {
		t.Errorf("the largest counter must parse: %v %v", v, err)
	}
}

// ---- Now: local events ----------------------------------------------------------

func TestNowFollowsPhysicalTimeAndResetsTheCounter(t *testing.T) {
	c, f := newClock(1000)
	if got := c.Now(); got != ts(1000, 0) {
		t.Errorf("first = %v", got)
	}
	f.Set(1001)
	if got := c.Now(); got != ts(1001, 0) {
		t.Errorf("physical time moved on: the counter must reset, got %v", got)
	}
}

func TestNowIsStrictlyIncreasingWhileThePhysicalClockStandsStill(t *testing.T) {
	c, _ := newClock(5000)
	prev := Timestamp{}
	for i := 0; i < 1000; i++ {
		got := c.Now()
		if !got.After(prev) {
			t.Fatalf("event %d: %v is not after %v", i, got, prev)
		}
		if got.Wall != 5000 || got.Counter != uint32(i) {
			t.Fatalf("event %d: %v, want (5000,%d)", i, got, i)
		}
		prev = got
	}
}

// The case the whole design exists for: the clock goes backwards.
func TestNowNeverGoesBackwardsWhenThePhysicalClockDoes(t *testing.T) {
	c, f := newClock(1000)
	var got []Timestamp
	for _, pt := range []int64{1000, 900, 900, 500, 1000, 1100} {
		f.Set(pt)
		got = append(got, c.Now())
	}
	want := []Timestamp{ts(1000, 0), ts(1000, 1), ts(1000, 2), ts(1000, 3), ts(1000, 4), ts(1100, 0)}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d: %v, want %v", i, got[i], want[i])
		}
	}
	if !c.Last().Equal(got[len(got)-1]) {
		t.Error("Last must be the last issued")
	}
}

func (t Timestamp) Equal(u Timestamp) bool { return t == u }

func TestNowWithTheClockSetBeforeTheEpoch(t *testing.T) {
	for _, ns := range []int64{-5_000_000_000, -1} {
		c, _ := newClock(ns)
		a, b := c.Now(), c.Now()
		if a.Wall != 0 || !b.After(a) {
			t.Errorf("clock at %d ns must act as the epoch and still advance: %v %v", ns, a, b)
		}
	}
}

func TestCounterExhaustionMovesTheWallInsteadOfWrapping(t *testing.T) {
	c, _ := newClock(50)
	c.Restore(ts(100, math.MaxUint32))
	got := c.Now()
	if got != ts(101, 0) || !got.After(ts(100, math.MaxUint32)) {
		t.Errorf("after the last counter value: %v", got)
	}
	// and through Update
	c2, _ := newClock(50)
	c2.Restore(ts(100, math.MaxUint32))
	got2, err := c2.Update(ts(100, math.MaxUint32))
	if err != nil || got2 != ts(101, 0) {
		t.Errorf("Update at the counter limit: %v %v", got2, err)
	}
}

func TestNowIsUniqueAndOrderedUnderConcurrency(t *testing.T) {
	c, _ := newClock(7777) // frozen: every event shares a wall, so the counter does all the work
	const g, n = 8, 1500
	out := make([][]Timestamp, g)
	var wg sync.WaitGroup
	for i := 0; i < g; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < n; j++ {
				out[i] = append(out[i], c.Now())
			}
		}(i)
	}
	wg.Wait() // run with -race
	seen := map[Timestamp]bool{}
	for i := range out {
		for j, s := range out[i] {
			if seen[s] {
				t.Fatalf("duplicate timestamp %v", s)
			}
			seen[s] = true
			if j > 0 && !s.After(out[i][j-1]) {
				t.Fatalf("goroutine %d went backwards at %d", i, j)
			}
		}
	}
	if len(seen) != g*n {
		t.Errorf("%d unique, want %d", len(seen), g*n)
	}
}

// ---- Update: receiving a message --------------------------------------------------

func TestUpdateFollowsThePaperCaseByCase(t *testing.T) {
	cases := []struct {
		name   string
		local  Timestamp
		remote Timestamp
		pt     int64
		want   Timestamp
	}{
		{"all three walls equal", ts(100, 5), ts(100, 9), 100, ts(100, 10)},
		{"local and remote walls equal, physical behind", ts(100, 5), ts(100, 9), 50, ts(100, 10)},
		{"local ahead of remote and of physical", ts(100, 5), ts(90, 99), 50, ts(100, 6)},
		{"local equals physical, remote behind", ts(100, 5), ts(90, 0), 100, ts(100, 6)},
		{"remote ahead of local and physical", ts(90, 5), ts(100, 7), 50, ts(100, 8)},
		{"remote equals physical, local behind", ts(90, 5), ts(100, 2), 100, ts(100, 3)},
		{"physical ahead of both", ts(90, 5), ts(80, 7), 120, ts(120, 0)},
		{"physical ahead, remote counter huge", ts(90, 5), ts(80, 99999), 120, ts(120, 0)},
		{"local counter larger when walls tie", ts(100, 20), ts(100, 3), 60, ts(100, 21)},
		{"fresh clock hears a remote", Timestamp{}, ts(100, 4), 10, ts(100, 5)},
	}
	for _, c := range cases {
		clk, f := newClock(c.pt)
		clk.Restore(c.local)
		f.Set(c.pt)
		got, err := clk.Update(c.remote)
		if err != nil || got != c.want {
			t.Errorf("%s: got %v (%v), want %v", c.name, got, err, c.want)
			continue
		}
		if !got.After(c.local) || !got.After(c.remote) {
			t.Errorf("%s: the result %v must be after local %v and remote %v", c.name, got, c.local, c.remote)
		}
		if clk.Last() != got {
			t.Errorf("%s: the clock must hold what it issued", c.name)
		}
	}
}

func TestEventAfterAReceiveIsAfterTheSenderOfTheMessage(t *testing.T) {
	a, fa := newClock(1000)
	b, fb := newClock(10) // b's clock is far behind
	_, _ = fa, fb
	sent := a.Now()
	if _, err := b.Update(sent); err != nil {
		t.Fatal(err)
	}
	if mine := b.Now(); !mine.After(sent) {
		t.Errorf("b made an edit after seeing a's, but stamped %v <= %v: a slow clock would lose the later edit", mine, sent)
	}
}

// ---- skew --------------------------------------------------------------------------

func TestSlowClockLosesWithWallTimeAndWinsWithHLC(t *testing.T) {
	const min = int64(time.Minute)
	a, _ := newClock(100 * min)
	b, _ := newClock(90 * min) // 10 minutes slow
	first := a.Now()           // a edits at real time 100
	if _, err := b.Update(first); err != nil {
		t.Fatal(err) // the edit syncs to b
	}
	second := b.Now() // b edits later, by its slow clock still showing 90
	if !(second.After(first)) {
		t.Fatalf("HLC: the later edit must have the later timestamp: %v vs %v", second, first)
	}
	// the same two events stamped by the raw physical clocks:
	naiveFirst, naiveSecond := ts(100*min, 0), ts(90*min, 0)
	if !naiveSecond.Before(naiveFirst) {
		t.Fatal("the scenario is meant to show wall time getting it wrong")
	}
}

func TestFastRemoteWithinTheLimitIsAdoptedAndKeptAhead(t *testing.T) {
	f := &fakeClock{ns: 1000}
	c := New(Config{Now: f.Now, MaxDrift: 500})
	got, err := c.Update(ts(1400, 3)) // 400 ahead: inside the limit
	if err != nil || got != ts(1400, 4) {
		t.Fatalf("%v %v", got, err)
	}
	f.Set(1100) // our own clock is still far behind
	if next := c.Now(); next != ts(1400, 5) {
		t.Errorf("the clock must stay with the larger wall: %v", next)
	}
	if got := c.Ahead(); got != 300 {
		t.Errorf("Ahead = %v, want 300ns", got)
	}
}

func TestRemoteTooFarAheadIsRefusedAndChangesNothing(t *testing.T) {
	f := &fakeClock{ns: int64(time.Hour)}
	c := New(Config{Now: f.Now, MaxDrift: time.Minute})
	before := c.Now()
	lim := ts(f.ns+int64(time.Minute), 0)
	if _, err := c.Update(lim); err != nil {
		t.Errorf("exactly at the limit is allowed: %v", err)
	}
	c2 := New(Config{Now: f.Now, MaxDrift: time.Minute})
	c2.Restore(before)
	far := ts(f.ns+int64(time.Minute)+1, 0)
	got, err := c2.Update(far)
	if !errors.Is(err, ErrDrift) {
		t.Fatalf("one nanosecond past the limit must be refused: %v %v", got, err)
	}
	var de *DriftError
	if !errors.As(err, &de) || de.Remote != far || de.Ahead != time.Minute+1 || de.Limit != time.Minute {
		t.Errorf("%+v", de)
	}
	if !strings.Contains(err.Error(), "ahead of the local clock") {
		t.Errorf("message: %v", err)
	}
	if c2.Last() != before {
		t.Errorf("a refused timestamp must not move the clock: %v -> %v", before, c2.Last())
	}
	// the node still works and is not poisoned
	if next := c2.Now(); next.Wall > f.ns+1 {
		t.Errorf("poisoned: %v", next)
	}
}

func TestDriftLimitDefaultsAndCanBeDisabled(t *testing.T) {
	f := &fakeClock{ns: 0}
	def := New(Config{Now: f.Now})
	if _, err := def.Update(ts(int64(DefaultMaxDrift), 0)); err != nil {
		t.Errorf("an hour ahead is the default limit: %v", err)
	}
	if _, err := New(Config{Now: f.Now}).Update(ts(int64(DefaultMaxDrift)+1, 0)); !errors.Is(err, ErrDrift) {
		t.Errorf("past the default limit: %v", err)
	}
	year := ts(int64(365*24*time.Hour), 0)
	if _, err := New(Config{Now: f.Now, MaxDrift: NoDriftLimit}).Update(year); err != nil {
		t.Errorf("NoDriftLimit: %v", err)
	}
}

func TestRemoteBehindNeverMovesTheClockBack(t *testing.T) {
	c, _ := newClock(1000)
	c.Now()
	before := c.Last()
	got, err := c.Update(ts(10, 0)) // a very old timestamp
	if err != nil || !got.After(before) {
		t.Errorf("%v %v", got, err)
	}
}

// ---- restart ----------------------------------------------------------------------------

func TestRestoreSurvivesARestartWithAClockThatIsBehind(t *testing.T) {
	c, f := newClock(2000)
	last := c.Now()
	f.Set(2001)
	last = c.Now()

	// the process restarts and the battery-dead clock now says 1500
	c2, _ := newClock(1500)
	c2.Restore(last)
	got := c2.Now()
	if !got.After(last) {
		t.Errorf("after a restart the timestamp %v must be after %v", got, last)
	}
	// without Restore, the restarted clock would have gone backwards
	c3, _ := newClock(1500)
	if bad := c3.Now(); bad.After(last) {
		t.Fatal("the scenario must show the hazard")
	}
}

func TestRestoreNeverLowersTheClock(t *testing.T) {
	c, _ := newClock(100)
	c.Restore(ts(500, 2))
	c.Restore(ts(300, 9))
	c.Restore(Timestamp{})
	if c.Last() != ts(500, 2) {
		t.Errorf("%v", c.Last())
	}
	if c.Now() != ts(500, 3) {
		t.Error("Now after Restore")
	}
}

func TestAheadIsZeroWhenThePhysicalClockHasCaughtUp(t *testing.T) {
	c, f := newClock(100)
	c.Restore(ts(160, 0))
	if got := c.Ahead(); got != 60 {
		t.Errorf("%v", got)
	}
	f.Set(160)
	if c.Ahead() != 0 {
		t.Error("equal is not ahead")
	}
	f.Set(900)
	if c.Ahead() != 0 {
		t.Error("behind is not ahead")
	}
}

// ---- many nodes, bad clocks ------------------------------------------------------------

// simulate runs n nodes whose physical clocks are each wrong by a different
// amount that also changes (including jumping backwards), exchanging messages
// at random. It returns what an observer would count as ordering mistakes made
// by the HLC and by raw physical time, judged against the causal order.
func simulate(seed int64, nodes, steps int) (hlcBad, naiveBad, messages int, worstAhead time.Duration, t *testing.T) {
	rng := rand.New(rand.NewSource(seed))
	const base = int64(1_800_000_000) * int64(time.Second)
	real := base
	offsets := make([]int64, nodes)
	maxOffset := int64(0)
	var clocks []*Clock
	var phys []*fakeClock
	for i := range offsets {
		offsets[i] = int64(rng.Intn(10_000)-5_000) * int64(time.Millisecond) // +-5 s
		maxOffset = max(maxOffset, offsets[i])
		f := &fakeClock{ns: real + offsets[i]}
		phys = append(phys, f)
		clocks = append(clocks, New(Config{Now: f.Now, MaxDrift: NoDriftLimit}))
	}
	prev := make([]Timestamp, nodes)
	prevNaive := make([]int64, nodes)
	lastRecv := make([]Timestamp, nodes)
	record := func(i int, s Timestamp) {
		if !s.After(prev[i]) {
			hlcBad++
		}
		prev[i] = s
	}
	for step := 0; step < steps; step++ {
		real += int64(rng.Intn(3)) * int64(time.Millisecond) // often 0: events share a millisecond
		for i := range clocks {
			if rng.Intn(50) == 0 { // the node's clock is stepped, forwards or backwards
				offsets[i] += int64(rng.Intn(4_000)-2_000) * int64(time.Millisecond)
				maxOffset = max(maxOffset, offsets[i])
			}
			phys[i].Set(real + offsets[i])
		}
		i := rng.Intn(nodes)
		if rng.Intn(3) == 0 { // a local event
			s := clocks[i].Now()
			record(i, s)
			if s.Wall > real+maxOffset {
				hlcBad++ // ran ahead of the fastest clock in the system
			}
			d := clocks[i].Ahead()
			if d > worstAhead {
				worstAhead = d
			}
			if d > time.Duration(maxOffset-offsets[i]) {
				hlcBad++ // ahead of physical time by more than the skew to the fastest clock
			}
			continue
		}
		j := (i + 1 + rng.Intn(nodes-1)) % nodes
		sent := clocks[i].Now()
		record(i, sent)
		naiveSent := real + offsets[i]
		recv, err := clocks[j].Update(sent)
		if err != nil {
			hlcBad++
			continue
		}
		messages++
		record(j, recv)
		lastRecv[j] = recv
		if !recv.After(sent) {
			hlcBad++ // causality: the receive must be after the send
		}
		// what raw physical time would have said about the same pair
		naiveRecv := real + offsets[j]
		if naiveRecv <= naiveSent {
			naiveBad++
		}
		_ = prevNaive
		// the very next local event on j must also be after the message
		if next := clocks[j].Now(); !next.After(sent) {
			hlcBad++
		} else {
			record(j, next)
		}
	}
	return hlcBad, naiveBad, messages, worstAhead, t
}

func TestManyNodesWithBadClocksNeverBreakCausalityOrMonotonicity(t *testing.T) {
	totalMessages, totalNaive := 0, 0
	for seed := int64(1); seed <= 25; seed++ {
		hlcBad, naiveBad, msgs, ahead, _ := simulate(seed, 2+int(seed%4), 4000)
		if hlcBad != 0 {
			t.Fatalf("seed %d: %d violations (monotonic, causal, or ahead of the fastest clock)", seed, hlcBad)
		}
		if seed == 1 {
			t.Logf("seed 1: the HLC ran at most %v ahead of its own physical clock (bounded by the skew to the fastest clock, checked on every event)", ahead)
		}
		totalMessages += msgs
		totalNaive += naiveBad
	}
	if totalNaive == 0 {
		t.Fatal("the simulation never made raw physical time wrong, so it proves nothing about the HLC")
	}
	t.Logf("%d messages: HLC ordering mistakes 0; raw physical clocks misordered %d of them", totalMessages, totalNaive)
}
