// Package obs is how Offgrid explains itself: structured logs (log/slog),
// counters, and a snapshot of them that other processes can read.
//
// Offgrid is several processes over one pair of files: `agent -route`
// answers and defers messages, `drain` works the queue, and `top` shows both.
// They share no memory, so each process keeps its own Metrics and publishes a
// small JSON snapshot file; `top` reads the files and the queue itself.
// Nothing here talks to the network or to a model.
package obs

import (
	"sort"
	"sync"
	"time"

	"offgrid/internal/agent"
	"offgrid/internal/connectivity"
	"offgrid/internal/drain"
	"offgrid/internal/jobqueue"
	"offgrid/internal/router"
)

// Sizes of the bounded histories. Counters never forget; these windows do.
const (
	latencyWindow = 256 // durations kept per series, for percentiles
	probeWindow   = 40  // probe latencies kept for the sparkline
	feedWindow    = 12  // recent events kept for the screen
	flipWindow    = 6   // recent connectivity changes kept
	decisionLimit = 8   // recent routing decisions kept
)

// Tier names a way a message was handled.
const (
	TierRules    = "rules"    // Tier 0: a rule read it, no model
	TierModel    = "model"    // Tier 1: the local model read it
	TierUnread   = "unread"   // answered locally, but nobody could read it
	TierCloud    = "cloud"    // a cloud model answered
	TierDeferred = "deferred" // saved in the queue for later
	TierStatus   = "status"   // a question about the queue itself
	TierFailed   = "failed"   // could not answer or save
)

// TierOf says which tier handled a routed message.
func TierOf(r router.Result) string {
	switch r.Route {
	case router.RouteLocal:
		switch r.Outcome.Tier {
		case agent.TierRules:
			return TierRules
		case agent.TierModel:
			return TierModel
		}
		return TierUnread
	case router.RouteCloud:
		return TierCloud
	case router.RouteDeferred:
		return TierDeferred
	case router.RouteStatus:
		return TierStatus
	}
	return TierFailed
}

// JobCounts is what the worker did with one kind of job.
type JobCounts struct {
	Claimed, Done, Retried, Dead, Released, Lost int
}

// Probe is the last network check.
type Probe struct {
	At      time.Time     `json:"at"`
	OK      bool          `json:"ok"`
	Latency time.Duration `json:"latency"`
	Target  string        `json:"target,omitempty"`
	Err     string        `json:"err,omitempty"`
}

// Flip is one change of connectivity state.
type Flip struct {
	At     time.Time `json:"at"`
	From   string    `json:"from"`
	To     string    `json:"to"`
	Reason string    `json:"reason,omitempty"`
}

// Decision is one routed message.
type Decision struct {
	At     time.Time     `json:"at"`
	Text   string        `json:"text"`
	Tier   string        `json:"tier"`
	Reason string        `json:"reason,omitempty"`
	Total  time.Duration `json:"total"`
}

// FeedLine is one line of the recent-events list.
type FeedLine struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"` // ok, warn, bad, info
	Text string    `json:"text"`
}

// Latency summarises a window of durations.
type Latency struct {
	N   int           `json:"n"`
	P50 time.Duration `json:"p50"`
	P95 time.Duration `json:"p95"`
	Max time.Duration `json:"max"`
}

// Metrics is one process's counters. It is safe for concurrent use.
type Metrics struct {
	Now func() time.Time // nil means time.Now

	mu sync.Mutex

	net      string
	netSince time.Time
	probes   struct{ ok, fail int }
	last     *Probe
	probeLat []time.Duration
	flips    []Flip

	queue   *jobqueue.Stats
	queueAt time.Time

	jobs     map[string]*JobCounts
	started  map[int64]time.Time // claim time per running job id
	jobLat   map[string][]time.Duration
	tiers    map[string]int
	tierLat  map[string][]time.Duration
	decision []Decision
	feed     []FeedLine
}

// NewMetrics makes an empty Metrics. The connection state starts "unknown":
// nothing has been checked yet, and claiming otherwise would be a guess.
func NewMetrics() *Metrics {
	return &Metrics{
		net:     connectivity.Unknown.String(),
		jobs:    map[string]*JobCounts{},
		started: map[int64]time.Time{},
		jobLat:  map[string][]time.Duration{},
		tiers:   map[string]int{},
		tierLat: map[string][]time.Duration{},
	}
}

func (m *Metrics) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func push[T any](s []T, v T, limit int) []T {
	s = append(s, v)
	if len(s) > limit {
		s = append(s[:0:0], s[len(s)-limit:]...)
	}
	return s
}

// Summarize computes the percentiles of ds (nearest rank). Empty gives zero.
func Summarize(ds []time.Duration) Latency {
	if len(ds) == 0 {
		return Latency{}
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	rank := func(p float64) time.Duration {
		i := int(p*float64(len(s))+0.999999) - 1
		return s[min(max(i, 0), len(s)-1)]
	}
	return Latency{N: len(s), P50: rank(0.50), P95: rank(0.95), Max: s[len(s)-1]}
}

// SetNet records a connectivity state that is not probed (for example
// -net offline). from is a short label of where it came from.
func (m *Metrics) SetNet(s connectivity.State, why string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setNet(s.String(), why)
}

func (m *Metrics) setNet(to, why string) {
	if to == m.net {
		return
	}
	now := m.now()
	m.flips = push(m.flips, Flip{At: now, From: m.net, To: to, Reason: why}, flipWindow)
	m.net, m.netSince = to, now
}

// Probe records one network check. It matches connectivity.Config.OnProbe.
func (m *Metrics) Probe(r connectivity.Result, _ connectivity.Verdict, s connectivity.State) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := Probe{At: m.now(), OK: r.OK, Latency: r.Latency, Target: r.Target}
	if r.Err != nil {
		p.Err = r.Err.Error()
	}
	if r.OK {
		m.probes.ok++
	} else {
		m.probes.fail++
	}
	m.last = &p
	lat := r.Latency
	if !r.OK {
		lat = -1 // a failed probe has no meaningful latency: the sparkline marks it
	}
	m.probeLat = push(m.probeLat, lat, probeWindow)
	if m.netSince.IsZero() {
		m.netSince = p.At
	}
	// The state can change on a probe; the monitor's Event (Transition)
	// carries the reason, so only adopt the state here if no event has yet.
	if s.String() != m.net {
		m.setNet(s.String(), "")
	}
}

// Transition records a connectivity change with its reason.
func (m *Metrics) Transition(e connectivity.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	to := e.To.String()
	if to == m.net { // the probe hook already adopted the state: add the reason
		if n := len(m.flips); n > 0 && m.flips[n-1].To == to && m.flips[n-1].Reason == "" {
			m.flips[n-1].Reason = e.Reason
		}
		return
	}
	m.setNet(to, e.Reason)
}

// Queue records the queue's current depth.
func (m *Metrics) Queue(s jobqueue.Stats) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queue, m.queueAt = &s, m.now()
}

func (m *Metrics) counts(kind string) *JobCounts {
	c := m.jobs[kind]
	if c == nil {
		c = &JobCounts{}
		m.jobs[kind] = c
	}
	return c
}

// Drain records one worker event. It matches drain.Config.OnEvent.
func (m *Metrics) Drain(e drain.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	line := FeedLine{At: e.At, Kind: "info"}
	if e.Kind == drain.EvGate { // about the network, not about any job
		line.Text = "network: " + e.Note
		m.feed = push(m.feed, line, feedWindow)
		return
	}
	c := m.counts(e.Job.Kind)
	tag := "#" + itoa(e.Job.ID) + " " + e.Job.Kind
	switch e.Kind {
	case drain.EvClaimed:
		c.Claimed++
		m.started[e.Job.ID] = e.At
		line.Text = tag + " started (try " + itoa(int64(e.Job.Attempts)) + ")"
	case drain.EvDone:
		c.Done++
		if t0, ok := m.started[e.Job.ID]; ok {
			m.jobLat[e.Job.Kind] = push(m.jobLat[e.Job.Kind], e.At.Sub(t0), latencyWindow)
		}
		delete(m.started, e.Job.ID)
		line.Kind, line.Text = "ok", tag+" done"
	case drain.EvRetry:
		c.Retried++
		delete(m.started, e.Job.ID)
		line.Kind, line.Text = "warn", tag+" failed, retry in "+e.Delay.Round(time.Millisecond).String()
	case drain.EvDead:
		c.Dead++
		delete(m.started, e.Job.ID)
		line.Kind, line.Text = "bad", tag+" DEAD"
	case drain.EvReleased:
		c.Released++
		delete(m.started, e.Job.ID)
		line.Kind, line.Text = "warn", tag+" given back: "+e.Note
	case drain.EvLostClaim:
		c.Lost++
		delete(m.started, e.Job.ID)
		line.Kind, line.Text = "warn", tag+" lost its claim"
	case drain.EvError:
		line.Kind, line.Text = "bad", tag+" queue problem"
	}
	m.feed = push(m.feed, line, feedWindow)
}

// Route records one routed message.
func (m *Metrics) Route(r router.Result) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := TierOf(r)
	m.tiers[t]++
	m.tierLat[t] = push(m.tierLat[t], r.Total, latencyWindow)
	text := oneLine(r.Text, 60)
	m.decision = push(m.decision, Decision{At: m.now(), Text: text, Tier: t, Reason: r.Reason, Total: r.Total}, decisionLimit)
	kind := "info"
	if t == TierDeferred {
		kind = "warn"
	} else if t == TierFailed {
		kind = "bad"
	}
	m.feed = push(m.feed, FeedLine{At: m.now(), Kind: kind, Text: t + ": " + text}, feedWindow)
}
