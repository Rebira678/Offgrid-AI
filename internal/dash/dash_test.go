package dash

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"offgrid/internal/jobqueue"
	"offgrid/internal/obs"
)

var now = time.Date(2026, 10, 17, 14, 2, 11, 0, time.UTC) // a Saturday

func at(s string) time.Time {
	t, err := time.Parse("15:04:05", s)
	if err != nil {
		panic(err)
	}
	return time.Date(2026, 10, 17, t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
}

// drainSnap is a live drain process that has been offline for a minute.
func drainSnap() obs.Snapshot {
	return obs.Snapshot{
		Version: obs.SnapshotVersion, Proc: "drain", At: now, Interval: time.Second,
		Net: "OFFLINE", NetSince: now.Add(-83 * time.Second),
		Probes:    struct{ OK, Fail int }{61, 3},
		LastProbe: &obs.Probe{At: now, OK: false, Latency: 1500 * time.Millisecond, Err: "context deadline exceeded"},
		ProbeMS:   []int64{40, 42, 41, -1, -1, -1},
		Flips:     []obs.Flip{{At: at("14:00:48"), From: "ONLINE", To: "DEGRADED", Reason: "2 slow probes"}, {At: at("14:00:50"), From: "DEGRADED", To: "OFFLINE", Reason: "3 probes failed in a row"}},
		Jobs:      map[string]obs.JobCounts{"deep_think": {Claimed: 5, Done: 3, Retried: 1, Released: 1}, "interpret": {Claimed: 2, Done: 2}},
		JobLatency: map[string]obs.Latency{
			"deep_think": {N: 3, P50: 1200 * time.Millisecond, P95: 3400 * time.Millisecond, Max: 3400 * time.Millisecond},
		},
		Feed: []obs.FeedLine{
			{At: at("14:02:09"), Kind: "warn", Text: "#6 deep_think given back: offline"},
			{At: at("14:02:03"), Kind: "ok", Text: "#4 interpret done"},
		},
	}
}

func agentSnap() obs.Snapshot {
	return obs.Snapshot{
		Version: obs.SnapshotVersion, Proc: "agent", At: now, Interval: time.Second, Net: "OFFLINE", NetSince: now.Add(-10 * time.Minute),
		Tiers: map[string]int{"rules": 14, "model": 3, "deferred": 5, "status": 1},
		TierLat: map[string]obs.Latency{
			"rules":    {N: 14, P50: 300 * time.Microsecond, P95: 800 * time.Microsecond},
			"model":    {N: 3, P50: 2100 * time.Millisecond, P95: 2600 * time.Millisecond},
			"deferred": {N: 5, P50: time.Millisecond, P95: 2 * time.Millisecond},
		},
		Feed: []obs.FeedLine{{At: at("14:02:10"), Kind: "warn", Text: "deferred: summarize this week"}},
	}
}

func baseView() View {
	return View{Now: now, Snaps: []obs.Snapshot{agentSnap(), drainSnap()}, Queue: &jobqueue.Stats{Queued: 4, Running: 1, Done: 5, Dead: 0, NextReady: now.Add(7 * time.Second)}}
}

// golden is the whole frame for baseView at width 80, checked by eye once.
const golden = `OFFGRID  Sat 17 Oct 14:02:11                                    ● agent  ● drain
┌─ CONNECTION ─────────────────────────────────────────────────────────────────┐
│ ● OFFLINE  for 1m23s                                                         │
│ last probe failed in 1.5s   (61 ok, 3 failed so far)  context deadline exce… │
│ probes ▇█▇✗✗✗                                                                │
│ 14:00:50  DEGRADED → OFFLINE  3 probes failed in a row                       │
│ 14:00:48  ONLINE → DEGRADED  2 slow probes                                   │
└──────────────────────────────────────────────────────────────────────────────┘
┌─ QUEUE ──────────────────────────────────────────────────────────────────────┐
│ queued 4   running 1   done 5   dead 0                                       │
│ [█████████████████████████████████████████████░░░░░░░░░░░░░░░░░░░░░░░░░░░░░] │
│ next job ready in 7.0s                                                       │
│ this worker  started  done retry  dead  back latency p50 / p95               │
│ deep_think         5     3     1     0     1 1.2s / 3.4s                     │
│ interpret          2     2     0     0     0 -                               │
└──────────────────────────────────────────────────────────────────────────────┘
┌─ TIERS  (who handled each message: latest agent run) ────────────────────────┐
│ rules       14  ███████████████████████                p50 300µs  p95 800µs  │
│ model        3  ████                                   p50 2.1s  p95 2.6s    │
│ deferred     5  ████████                               p50 1ms  p95 2ms      │
│ status       1  █                                                            │
└──────────────────────────────────────────────────────────────────────────────┘
┌─ RECENT ─────────────────────────────────────────────────────────────────────┐
│ 14:02:10  deferred: summarize this week                                      │
│ 14:02:09  #6 deep_think given back: offline                                  │
│ 14:02:03  #4 interpret done                                                  │
└──────────────────────────────────────────────────────────────────────────────┘`

func lines(s string) []string { return strings.Split(s, "\n") }

func TestRenderEveryLineFitsTheWidthWithAndWithoutColour(t *testing.T) {
	long := baseView()
	s := drainSnap()
	s.Feed = []obs.FeedLine{{At: now, Kind: "bad", Text: strings.Repeat("very long event text ", 20)}}
	s.LastProbe.Err = strings.Repeat("e", 300)
	s.Flips[0].Reason = strings.Repeat("r", 300)
	long.Snaps = []obs.Snapshot{agentSnap(), s}
	long.Problems = []string{strings.Repeat("p", 300)}
	for _, w := range []int{64, 70, 80, 100, 132} {
		for _, color := range []bool{false, true} {
			out := Render(long, Options{Width: w, Color: color})
			for i, l := range lines(out) {
				if n := visLen(l); n > w {
					t.Errorf("width %d color %v line %d is %d wide: %q", w, color, i, n, l)
				}
			}
			if !color && strings.Contains(out, "\x1b") {
				t.Errorf("width %d: escape codes without colour", w)
			}
			if color && !strings.Contains(out, "\x1b[") {
				t.Errorf("width %d: no colour asked for none given", w)
			}
		}
	}
}

func TestRenderBoxesAreAligned(t *testing.T) {
	out := Render(baseView(), Options{Width: 80})
	for _, l := range lines(out)[1:] {
		if utf8.RuneCountInString(l) != 80 {
			t.Errorf("%d wide: %q", utf8.RuneCountInString(l), l)
		}
	}
}

func TestRenderTooNarrowWidthIsRaisedToTheMinimum(t *testing.T) {
	out := Render(baseView(), Options{Width: 10})
	if w := utf8.RuneCountInString(lines(out)[1]); w != 64 {
		t.Errorf("width %d", w)
	}
}

func TestRenderShowsTheWholeSystemOnOneScreen(t *testing.T) {
	out := Render(baseView(), Options{})
	for _, want := range []string{
		"OFFGRID", "Sat 17 Oct 14:02:11", "● agent", "● drain",
		"OFFLINE", "for 1m23s", "last probe failed in 1.5s", "61 ok, 3 failed so far", "DEGRADED → OFFLINE", "3 probes failed in a row",
		"queued 4", "running 1", "done 5", "dead 0", "next job ready in 7.0s",
		"deep_think", "1.2s / 3.4s",
		"rules", "model", "deferred", "status", "p50 300µs",
		"RECENT", "#6 deep_think given back: offline", "deferred: summarize this week",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("screen is missing %q:\n%s", want, out)
		}
	}
	if n := len(lines(out)); n > 40 {
		t.Errorf("%d lines: it must fit one terminal", n)
	}
}

func TestRenderGoldenFrame(t *testing.T) {
	got := Render(baseView(), Options{Width: 80})
	if got != golden {
		t.Errorf("the frame changed. If that is intended, review it and update golden:\n%s", got)
	}
}

func TestRenderStaleProcessIsNeverShownAsLive(t *testing.T) {
	s := drainSnap()
	s.Stopped = true
	s.At = now.Add(-5 * time.Second)
	v := View{Now: now, Snaps: []obs.Snapshot{s}}
	out := Render(v, Options{})
	if !strings.Contains(out, "last known: drain stopped 5.0s ago") {
		t.Errorf("a stopped process's state must be labelled:\n%s", out)
	}
	if strings.Contains(out, "for 1m23s") {
		t.Errorf("a duration 'for' implies it is still true:\n%s", out)
	}
	if !strings.Contains(out, "○ drain stopped") {
		t.Errorf("header:\n%s", out)
	}
	// silent (crashed), not stopped
	s.Stopped = false
	s.At = now.Add(-30 * time.Second)
	out = Render(View{Now: now, Snaps: []obs.Snapshot{s}}, Options{})
	if !strings.Contains(out, "last known: drain went silent 30.0s ago") || !strings.Contains(out, "○ drain silent 30.0s") {
		t.Errorf("a process that stopped writing without saying so must be called silent:\n%s", out)
	}
}

func TestRenderEmptyAndErrorStates(t *testing.T) {
	out := Render(View{Now: now}, Options{})
	for _, want := range []string{"no process is publishing", "no connectivity reading yet", "queue not read", "no messages routed yet", "nothing has happened yet"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	out = Render(View{Now: now, QueueErr: "no queue yet at /x/queue.db", Problems: []string{"bad.json: not a snapshot"}}, Options{})
	if !strings.Contains(out, "queue unreadable: no queue yet at /x/queue.db") || !strings.Contains(out, "! bad.json: not a snapshot") {
		t.Errorf("\n%s", out)
	}
}

func TestRenderConnectionStatesAndColours(t *testing.T) {
	for state, code := range map[string]string{"ONLINE": "\x1b[32m", "DEGRADED": "\x1b[33m", "OFFLINE": "\x1b[31m"} {
		s := drainSnap()
		s.Net = state
		out := Render(View{Now: now, Snaps: []obs.Snapshot{s}}, Options{Color: true})
		if !strings.Contains(out, code+"● "+state) {
			t.Errorf("%s must be drawn in %q", state, code)
		}
	}
	s := drainSnap()
	s.Net = "UNKNOWN"
	if out := Render(View{Now: now, Snaps: []obs.Snapshot{s}}, Options{}); !strings.Contains(out, "● UNKNOWN") {
		t.Error("unknown")
	}
}

func TestRenderNotProbedWhenStateIsForced(t *testing.T) {
	s := agentSnap()
	out := Render(View{Now: now, Snaps: []obs.Snapshot{s}}, Options{})
	if !strings.Contains(out, "not probed") {
		t.Errorf("\n%s", out)
	}
}

func TestRenderQueueNextReadyWording(t *testing.T) {
	v := baseView()
	v.Queue = &jobqueue.Stats{Queued: 2, NextReady: now.Add(-time.Second)}
	if out := Render(v, Options{}); !strings.Contains(out, "next job is ready now") {
		t.Error("ready now")
	}
	v.Queue = &jobqueue.Stats{Done: 3}
	if out := Render(v, Options{}); !strings.Contains(out, "nothing waiting") {
		t.Error("nothing waiting")
	}
	v.Queue = &jobqueue.Stats{Running: 1, Done: 3}
	if out := Render(v, Options{}); strings.Contains(out, "nothing waiting") {
		t.Error("a running job means something is happening")
	}
	v.Queue = &jobqueue.Stats{}
	if out := Render(v, Options{}); strings.Contains(out, "[") && strings.Contains(out, "█]") {
		t.Error("an empty queue has no bar")
	}
}

func TestBarFillsExactlyAndMatchesProportions(t *testing.T) {
	p := palette{}
	cases := []jobqueue.Stats{
		{Queued: 1, Running: 1, Done: 1, Dead: 1},
		{Queued: 7},
		{Done: 3, Dead: 1},
		{Running: 5, Queued: 1},
		{Dead: 9, Done: 1, Queued: 1},
		{Done: 1, Queued: 2, Running: 3, Dead: 5},
	}
	for _, s := range cases {
		b := bar(s, s.Total(), 52, p)
		if n := utf8.RuneCountInString(b); n != 52 {
			t.Errorf("%+v: bar is %d wide, want 50 cells plus brackets: %q", s, n, b)
		}
	}
	b := bar(jobqueue.Stats{Done: 3, Queued: 1}, 4, 42, p)
	if got := strings.Count(b, "█"); got != 30 {
		t.Errorf("done share: %d of 40 cells, want 30: %q", got, b)
	}
	if got := strings.Count(b, "░"); got != 10 {
		t.Errorf("queued share: %d of 40, want 10", got)
	}
}

func TestSpark(t *testing.T) {
	p := palette{}
	if got := spark([]int64{0, 50, 100}, 10, p); got != "▁▄█" {
		t.Errorf("%q", got)
	}
	if got := spark([]int64{10, -1, 10}, 10, p); got != "█✗█" {
		t.Errorf("a failed probe must show as ✗: %q", got)
	}
	if got := spark([]int64{1, 2, 3, 4, 5, 6}, 3, p); got != "▅▆█" {
		t.Errorf("only the newest values (4,5,6) fit, scaled among themselves: %q", got)
	}
	if got := spark([]int64{0, 0}, 5, p); got != "▁▁" {
		t.Errorf("all zero: %q", got)
	}
	if spark(nil, 5, p) != "" || spark([]int64{1}, 0, p) != "" {
		t.Error("nothing to draw")
	}
}

func TestDur(t *testing.T) {
	cases := map[time.Duration]string{
		0: "0µs", 300 * time.Microsecond: "300µs", time.Millisecond: "1ms", 999 * time.Millisecond: "999ms",
		time.Second: "1.0s", 1500 * time.Millisecond: "1.5s", 59 * time.Second: "59.0s",
		time.Minute: "1m00s", 83 * time.Second: "1m23s", 59*time.Minute + 59*time.Second: "59m59s",
		time.Hour: "1h00m", 2*time.Hour + 5*time.Minute: "2h05m",
	}
	for d, want := range cases {
		if got := dur(d); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
}

func TestMergeTrustsTheFreshestLiveProcessForConnectivity(t *testing.T) {
	stale := drainSnap()
	stale.Net, stale.Stopped = "ONLINE", true
	stale.At = now.Add(-time.Minute)
	live := agentSnap() // OFFLINE, fresh
	m := merge([]obs.Snapshot{stale, live}, now)
	if m.net != "OFFLINE" || m.netFrom != "agent" || m.netStale {
		t.Errorf("a live reading must beat an older stopped one: %+v", m)
	}
	// newer stale beats older stale, when nothing is alive
	a, b := drainSnap(), agentSnap()
	a.Stopped, b.Stopped = true, true
	a.Net, b.Net = "ONLINE", "OFFLINE"
	a.At, b.At = now.Add(-10*time.Second), now.Add(-20*time.Second)
	m = merge([]obs.Snapshot{b, a}, now)
	if m.net != "ONLINE" || !m.netStale || !m.netStop {
		t.Errorf("%+v", m)
	}
	// two live ones: the later write wins
	x, y := drainSnap(), agentSnap()
	x.LastProbe = nil
	x.Net, y.Net = "ONLINE", "DEGRADED"
	x.At, y.At = now.Add(-300*time.Millisecond), now
	if m = merge([]obs.Snapshot{x, y}, now); m.net != "DEGRADED" {
		t.Errorf("%+v", m)
	}
	// order of the input must not matter
	if m2 := merge([]obs.Snapshot{y, x}, now); m2.net != m.net {
		t.Errorf("merge depends on input order: %s vs %s", m2.net, m.net)
	}
}

func TestMergePrefersARealProbeOverAFixedState(t *testing.T) {
	probing := drainSnap() // OFFLINE, has a probe
	probing.Net = "ONLINE"
	forced := agentSnap() // OFFLINE by a flag, no probe, written later
	forced.At = now.Add(100 * time.Millisecond)
	for _, order := range [][]obs.Snapshot{{probing, forced}, {forced, probing}} {
		if m := merge(order, now.Add(100*time.Millisecond)); m.net != "ONLINE" || m.netFrom != "drain" {
			t.Errorf("a pretend state (-net offline) must not hide what the probes see: %s from %s", m.net, m.netFrom)
		}
	}
}

func TestMergeSumsCountersAndSortsTheFeedNewestFirst(t *testing.T) {
	a, b := drainSnap(), drainSnap()
	b.Proc = "drain2"
	m := merge([]obs.Snapshot{a, b, agentSnap()}, now)
	if c := m.jobs["deep_think"]; c.Claimed != 10 || c.Done != 6 || c.Retried != 2 || c.Released != 2 {
		t.Errorf("%+v", c)
	}
	if m.tiers["rules"] != 14 {
		t.Errorf("%v", m.tiers)
	}
	for i := 1; i < len(m.feed); i++ {
		if m.feed[i].At.After(m.feed[i-1].At) {
			t.Errorf("feed not newest first at %d", i)
		}
	}
	if m.feed[0].Text != "deferred: summarize this week" {
		t.Errorf("%+v", m.feed[0])
	}
}

func TestTierLinesOrderAndOmitEmptyTiers(t *testing.T) {
	m := merge([]obs.Snapshot{agentSnap()}, now)
	got := tierLines(m, palette{}, 76)
	if len(got) != 4 {
		t.Fatalf("%v", got)
	}
	for i, want := range []string{"rules", "model", "deferred", "status"} {
		if !strings.HasPrefix(got[i], want) {
			t.Errorf("line %d: %q, want %s first", i, got[i], want)
		}
	}
	// a tier with a tiny share still shows at least one cell
	s := agentSnap()
	s.Tiers = map[string]int{"rules": 1000, "failed": 1}
	l := tierLines(merge([]obs.Snapshot{s}, now), palette{}, 76)
	if !strings.Contains(l[1], "█") {
		t.Errorf("a failed message must not vanish from the bar: %q", l[1])
	}
}

func TestRecentShowsAtMostEightNewestWithColours(t *testing.T) {
	s := drainSnap()
	s.Feed = nil
	for i := 0; i < 12; i++ {
		s.Feed = append(s.Feed, obs.FeedLine{At: now.Add(time.Duration(i) * time.Second), Kind: []string{"ok", "warn", "bad", "info"}[i%4], Text: "e"})
	}
	m := merge([]obs.Snapshot{s}, now)
	got := recentLines(m, palette{true})
	if len(got) != 8 {
		t.Fatalf("%d", len(got))
	}
	if !strings.HasPrefix(ansi.ReplaceAllString(got[0], ""), "14:02:22") {
		t.Errorf("newest first: %q", got[0])
	}
	for _, c := range []string{"\x1b[32m", "\x1b[33m", "\x1b[31m"} {
		if !strings.Contains(strings.Join(got, ""), c) {
			t.Errorf("colour %q missing", c)
		}
	}
}

func TestClipNeverLeavesAHalfEscape(t *testing.T) {
	s := "\x1b[31m" + strings.Repeat("x", 50) + "\x1b[0m"
	c := clip(s, 10)
	if strings.Contains(c, "\x1b") || utf8.RuneCountInString(c) != 10 || !strings.HasSuffix(c, "…") {
		t.Errorf("%q", c)
	}
	if clip("short", 10) != "short" {
		t.Error("short text must pass through")
	}
}

// ---- Live ----------------------------------------------------------------

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func runLive(t *testing.T, tty bool, frames int) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var buf syncBuf
	n := 0
	next := func() (View, Options) {
		n++
		if n == frames {
			cancel()
		}
		return baseView(), Options{}
	}
	done := make(chan struct{})
	go func() { Live(ctx, &buf, tty, 5*time.Millisecond, next); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Live did not stop when its context ended")
	}
	return buf.String()
}

func TestLiveOnATerminalTakesOverAndAlwaysRestores(t *testing.T) {
	out := runLive(t, true, 3)
	if !strings.HasPrefix(out, altScreenOn+hideCursor) {
		t.Errorf("start: %q", out[:20])
	}
	if !strings.HasSuffix(out, showCursor+altScreenOff) {
		t.Errorf("the terminal must be given back: %q", out[len(out)-20:])
	}
	if n := strings.Count(out, home); n < 3 {
		t.Errorf("repainted %d times", n)
	}
	if !strings.Contains(out, clearToEOL+"\n") || !strings.Contains(out, clearBelow) {
		t.Error("leftovers of a longer previous frame must be wiped")
	}
	if !strings.Contains(out, clearToEOL+"\n\x1b[2m(Ctrl-C to quit)") {
		t.Error("the last line of a frame must be cleared to its end too")
	}
	if !strings.Contains(out, "Ctrl-C to quit") {
		t.Error("tell the person how to leave")
	}
}

func TestLiveOnAPipeIsPlainFramesWithNoEscapes(t *testing.T) {
	out := runLive(t, false, 2)
	if strings.Contains(out, "\x1b") {
		t.Errorf("escape codes in piped output: %q", out)
	}
	if n := strings.Count(out, "OFFGRID  "); n < 2 {
		t.Errorf("%d frames", n)
	}
	if !strings.Contains(out, "┘\n\nOFFGRID") {
		t.Errorf("frames must be separated by a blank line: %q", out)
	}
}

func TestLiveDefaultsItsInterval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var buf syncBuf
	go func() {
		for buf.String() == "" {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	Live(ctx, &buf, false, 0, func() (View, Options) { return baseView(), Options{} })
	if !strings.Contains(buf.String(), "OFFGRID") {
		t.Error("the first frame is painted at once, without waiting an interval")
	}
}
