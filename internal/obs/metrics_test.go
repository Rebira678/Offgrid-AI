package obs

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"offgrid/internal/agent"
	"offgrid/internal/connectivity"
	"offgrid/internal/drain"
	"offgrid/internal/jobqueue"
	"offgrid/internal/router"
)

func TestSummarizeUsesNearestRank(t *testing.T) {
	var ds []time.Duration
	for i := 100; i >= 1; i-- { // deliberately unsorted
		ds = append(ds, time.Duration(i)*time.Millisecond)
	}
	orig := append([]time.Duration(nil), ds...)
	got := Summarize(ds)
	if got.N != 100 || got.P50 != 50*time.Millisecond || got.P95 != 95*time.Millisecond || got.Max != 100*time.Millisecond {
		t.Errorf("100 values: %+v", got)
	}
	for i := range ds {
		if ds[i] != orig[i] {
			t.Fatal("Summarize reordered its input")
		}
	}
	if one := Summarize([]time.Duration{7}); one.P50 != 7 || one.P95 != 7 || one.Max != 7 || one.N != 1 {
		t.Errorf("one value: %+v", one)
	}
	if two := Summarize([]time.Duration{10, 20}); two.P50 != 10 || two.P95 != 20 {
		t.Errorf("two values: %+v", two)
	}
	if z := Summarize(nil); z != (Latency{}) {
		t.Errorf("none: %+v", z)
	}
}

func TestTierOf(t *testing.T) {
	cases := []struct {
		name string
		r    router.Result
		want string
	}{
		{"rules", router.Result{Route: router.RouteLocal, Outcome: agent.Outcome{Tier: agent.TierRules}}, TierRules},
		{"model", router.Result{Route: router.RouteLocal, Outcome: agent.Outcome{Tier: agent.TierModel}}, TierModel},
		{"unread", router.Result{Route: router.RouteLocal, Outcome: agent.Outcome{Tier: agent.TierNone}}, TierUnread},
		{"cloud", router.Result{Route: router.RouteCloud}, TierCloud},
		{"deferred", router.Result{Route: router.RouteDeferred}, TierDeferred},
		{"status", router.Result{Route: router.RouteStatus}, TierStatus},
		{"failed", router.Result{Route: router.RouteFailed}, TierFailed},
		{"unknown route is not hidden", router.Result{Route: "weird"}, TierFailed},
	}
	for _, c := range cases {
		if got := TierOf(c.r); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestPushKeepsOnlyTheNewest(t *testing.T) {
	var s []int
	for i := 1; i <= 10; i++ {
		s = push(s, i, 4)
	}
	if len(s) != 4 || s[0] != 7 || s[3] != 10 {
		t.Errorf("%v", s)
	}
}

func TestNewMetricsStartsUnknown(t *testing.T) {
	m, _ := newM()
	if s := m.Snapshot("x", 1, t0, time.Second); s.Net != "UNKNOWN" {
		t.Errorf("before any probe the state must be UNKNOWN, got %q", s.Net)
	}
}

func TestDrainCountsPerKindAndMeasuresLatencyFromTheClaim(t *testing.T) {
	m, _ := newM()
	m.Drain(ev(drain.EvClaimed, 1, "interpret", t0))
	m.Drain(ev(drain.EvClaimed, 2, "deep_think", t0))
	m.Drain(ev(drain.EvDone, 1, "interpret", t0.Add(300*time.Millisecond)))
	// job 2 fails once, is claimed again later, and only the LAST claim counts
	m.Drain(drain.Event{Kind: drain.EvRetry, Job: jobqueue.Job{ID: 2, Kind: "deep_think"}, Delay: time.Second, At: t0.Add(time.Second)})
	m.Drain(ev(drain.EvClaimed, 2, "deep_think", t0.Add(5*time.Second)))
	m.Drain(ev(drain.EvDone, 2, "deep_think", t0.Add(5*time.Second+700*time.Millisecond)))
	m.Drain(ev(drain.EvClaimed, 3, "deep_think", t0))
	m.Drain(ev(drain.EvDead, 3, "deep_think", t0))
	m.Drain(ev(drain.EvReleased, 4, "interpret", t0))
	m.Drain(ev(drain.EvLostClaim, 5, "interpret", t0))
	m.Drain(ev(drain.EvGate, 0, "", t0))

	s := m.Snapshot("drain", 1, t0, time.Second)
	if got, want := s.Jobs["interpret"], (JobCounts{Claimed: 1, Done: 1, Released: 1, Lost: 1}); got != want {
		t.Errorf("interpret = %+v, want %+v", got, want)
	}
	if got, want := s.Jobs["deep_think"], (JobCounts{Claimed: 3, Done: 1, Retried: 1, Dead: 1}); got != want {
		t.Errorf("deep_think = %+v, want %+v", got, want)
	}
	if l := s.JobLatency["interpret"]; l.N != 1 || l.P50 != 300*time.Millisecond {
		t.Errorf("interpret latency %+v", l)
	}
	if l := s.JobLatency["deep_think"]; l.N != 1 || l.P50 != 700*time.Millisecond {
		t.Errorf("deep_think latency must be measured from the last claim, got %+v", l)
	}
	if len(m.started) != 0 {
		t.Errorf("claim times leaked: %v", m.started)
	}
}

func TestNetworkGateEventsAreNotJobsAndCreateNoJobKind(t *testing.T) {
	m, _ := newM()
	m.Drain(drain.Event{Kind: drain.EvGate, Note: "network is down", At: t0})
	s := m.Snapshot("d", 1, t0, time.Second)
	if len(s.Jobs) != 0 {
		t.Errorf("a network event must not appear as a job kind: %v", s.Jobs)
	}
	if len(s.Feed) != 1 || s.Feed[0].Text != "network: network is down" || s.Feed[0].Kind != "info" {
		t.Errorf("but it is worth showing in the feed: %+v", s.Feed)
	}
}

func TestDrainFeedKindsAndBound(t *testing.T) {
	m, _ := newM()
	for i := 0; i < 30; i++ {
		m.Drain(ev(drain.EvDone, int64(i), "interpret", t0.Add(time.Duration(i)*time.Second)))
	}
	m.Drain(ev(drain.EvDead, 99, "interpret", t0))
	s := m.Snapshot("d", 1, t0, time.Second)
	if len(s.Feed) != feedWindow {
		t.Fatalf("feed length %d, want %d", len(s.Feed), feedWindow)
	}
	last := s.Feed[len(s.Feed)-1]
	if last.Kind != "bad" || !strings.Contains(last.Text, "DEAD") {
		t.Errorf("a dead job must be flagged bad: %+v", last)
	}
	if s.Feed[0].Kind != "ok" {
		t.Errorf("done must be ok: %+v", s.Feed[0])
	}
}

func TestLatencyWindowIsBounded(t *testing.T) {
	m, _ := newM()
	for i := 0; i < latencyWindow+50; i++ {
		id := int64(i)
		m.Drain(ev(drain.EvClaimed, id, "k", t0))
		m.Drain(ev(drain.EvDone, id, "k", t0.Add(time.Duration(i+1)*time.Millisecond)))
	}
	if n := len(m.jobLat["k"]); n != latencyWindow {
		t.Errorf("window %d, want %d", n, latencyWindow)
	}
	// counters never forget even though the window does
	if c := m.Snapshot("d", 1, t0, time.Second).Jobs["k"]; c.Done != latencyWindow+50 {
		t.Errorf("done = %d", c.Done)
	}
}

func TestProbeCountsAndMarksFailures(t *testing.T) {
	m, c := newM()
	m.Probe(connectivity.Result{OK: true, Latency: 40 * time.Millisecond, Target: "a"}, connectivity.Good, connectivity.Online)
	c.Add(time.Second)
	m.Probe(connectivity.Result{OK: false, Latency: 1500 * time.Millisecond, Err: errors.New("boom")}, connectivity.Fail, connectivity.Online)
	s := m.Snapshot("d", 1, t0, time.Second)
	if s.Probes.OK != 1 || s.Probes.Fail != 1 {
		t.Errorf("probes %+v", s.Probes)
	}
	if len(s.ProbeMS) != 2 || s.ProbeMS[0] != 40 || s.ProbeMS[1] != -1 {
		t.Errorf("probe strip %v: a failed probe must be -1, not its timeout", s.ProbeMS)
	}
	if s.LastProbe == nil || s.LastProbe.OK || s.LastProbe.Err != "boom" || s.LastProbe.Latency != 1500*time.Millisecond {
		t.Errorf("last probe %+v", s.LastProbe)
	}
	if s.Net != "ONLINE" || !s.NetSince.Equal(t0) {
		t.Errorf("net %s since %v", s.Net, s.NetSince)
	}
}

func TestProbeWindowIsBounded(t *testing.T) {
	m, _ := newM()
	for i := 0; i < probeWindow+10; i++ {
		m.Probe(connectivity.Result{OK: true, Latency: time.Millisecond}, connectivity.Good, connectivity.Online)
	}
	if n := len(m.probeLat); n != probeWindow {
		t.Errorf("%d", n)
	}
}

func TestTransitionAddsItsReasonWithoutDuplicatingTheFlip(t *testing.T) {
	m, c := newM()
	// the monitor calls OnProbe first, then publishes the event
	m.Probe(connectivity.Result{OK: true}, connectivity.Good, connectivity.Online)
	c.Add(time.Second)
	m.Transition(connectivity.Event{From: connectivity.Unknown, To: connectivity.Online, Reason: "first probe"})
	s := m.Snapshot("d", 1, t0, time.Second)
	if len(s.Flips) != 1 || s.Flips[0].Reason != "first probe" || s.Flips[0].From != "UNKNOWN" || s.Flips[0].To != "ONLINE" {
		t.Errorf("flips %+v", s.Flips)
	}
	// the other order: the event arrives before any probe reports the state
	m2, _ := newM()
	m2.Transition(connectivity.Event{From: connectivity.Unknown, To: connectivity.Offline, Reason: "3 failed"})
	if f := m2.Snapshot("d", 1, t0, time.Second).Flips; len(f) != 1 || f[0].Reason != "3 failed" {
		t.Errorf("event first: %+v", f)
	}
}

func TestSetNetOnlyRecordsRealChanges(t *testing.T) {
	m, _ := newM()
	m.SetNet(connectivity.Offline, "-net offline")
	m.SetNet(connectivity.Offline, "again")
	s := m.Snapshot("a", 1, t0, time.Second)
	if s.Net != "OFFLINE" || len(s.Flips) != 1 || s.Flips[0].Reason != "-net offline" {
		t.Errorf("%+v", s)
	}
}

func TestFlipHistoryIsBounded(t *testing.T) {
	m, _ := newM()
	for i := 0; i < 20; i++ {
		to := connectivity.Online
		if i%2 == 1 {
			to = connectivity.Offline
		}
		m.SetNet(to, "")
	}
	if n := len(m.Snapshot("a", 1, t0, time.Second).Flips); n != flipWindow {
		t.Errorf("%d flips", n)
	}
}

func TestRouteCountsTiersAndKeepsRecentDecisions(t *testing.T) {
	m, c := newM()
	local := router.Result{Text: "list open tasks", Route: router.RouteLocal, Outcome: agent.Outcome{Tier: agent.TierRules}, Total: 300 * time.Microsecond}
	deferred := router.Result{Text: "summarize\nthis   week", Route: router.RouteDeferred, Reason: "judgement", Total: time.Millisecond}
	failed := router.Result{Text: "x", Route: router.RouteFailed}
	m.Route(local)
	m.Route(local)
	c.Add(time.Second)
	m.Route(deferred)
	m.Route(failed)
	s := m.Snapshot("agent", 1, t0, time.Second)
	if s.Tiers[TierRules] != 2 || s.Tiers[TierDeferred] != 1 || s.Tiers[TierFailed] != 1 {
		t.Errorf("tiers %v", s.Tiers)
	}
	if l := s.TierLat[TierRules]; l.N != 2 || l.P50 != 300*time.Microsecond {
		t.Errorf("rules latency %+v", l)
	}
	d := s.Decisions[2]
	if d.Text != "summarize this week" || d.Tier != TierDeferred || d.Reason != "judgement" {
		t.Errorf("decision %+v: text must be one line", d)
	}
	if got := s.Feed[2].Kind; got != "warn" {
		t.Errorf("deferred feed kind %q", got)
	}
	if got := s.Feed[3].Kind; got != "bad" {
		t.Errorf("failed feed kind %q", got)
	}
	for i := 0; i < 30; i++ {
		m.Route(local)
	}
	if n := len(m.Snapshot("a", 1, t0, time.Second).Decisions); n != decisionLimit {
		t.Errorf("decisions %d", n)
	}
}

func TestQueueGauge(t *testing.T) {
	m, _ := newM()
	if m.Snapshot("a", 1, t0, time.Second).Queue != nil {
		t.Error("no queue reading yet: Queue must be nil, not zeros")
	}
	m.Queue(jobqueue.Stats{Queued: 3, Running: 1, Done: 5, Dead: 2})
	q := m.Snapshot("a", 1, t0, time.Second).Queue
	if q == nil || q.Queued != 3 || q.Dead != 2 {
		t.Errorf("%+v", q)
	}
}

func TestMetricsAreSafeForConcurrentUse(t *testing.T) {
	m := NewMetrics()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				id := int64(g*1000 + i)
				m.Drain(ev(drain.EvClaimed, id, "k", time.Now()))
				m.Drain(ev(drain.EvDone, id, "k", time.Now()))
				m.Probe(connectivity.Result{OK: true}, connectivity.Good, connectivity.Online)
				m.Route(router.Result{Route: router.RouteDeferred})
				_ = m.Snapshot("x", 1, time.Now(), time.Second)
			}
		}(g)
	}
	wg.Wait() // run with -race
	if c := m.Snapshot("x", 1, t0, time.Second).Jobs["k"]; c.Done != 1600 {
		t.Errorf("done = %d", c.Done)
	}
}
