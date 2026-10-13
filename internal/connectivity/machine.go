// Package connectivity decides, from a stream of probe results, whether this
// machine is ONLINE, DEGRADED or OFFLINE, and tells the rest of the program
// when that changes.
//
// It is built in three layers so each can be tested alone:
//
//	Machine   a pure state machine: results in, transitions out. No clock,
//	          no goroutines, no network. Hysteresis lives here.
//	Prober    one probe of the network (HTTP, DNS, several at once).
//	Monitor   the goroutine: probe on a timer, feed the Machine, publish events.
package connectivity

import "fmt"

// State is how usable the network is right now.
type State int

const (
	// Unknown is only the state before the first probe has finished.
	Unknown State = iota
	// Online: probes succeed and are fast. Safe to sync and call the cloud.
	Online
	// Degraded: the network answers, but slowly or only sometimes. Worth
	// trying small things; do not start anything big.
	Degraded
	// Offline: probes fail. Work must go to the local queue.
	Offline
)

func (s State) String() string {
	switch s {
	case Online:
		return "ONLINE"
	case Degraded:
		return "DEGRADED"
	case Offline:
		return "OFFLINE"
	default:
		return "UNKNOWN"
	}
}

// Verdict is what a single probe told us.
type Verdict int

const (
	Good Verdict = iota // answered, quickly
	Slow                // answered, but slower than the slow threshold
	Fail                // did not answer, or answered wrongly
)

func (v Verdict) String() string {
	switch v {
	case Good:
		return "good"
	case Slow:
		return "slow"
	default:
		return "fail"
	}
}

// Thresholds are the hysteresis settings: how many consecutive probes of one
// kind it takes to change our mind. They are in probes, not seconds, so the
// time to react is count x probe interval.
type Thresholds struct {
	// DownAfter consecutive failures make the state OFFLINE.
	DownAfter int
	// DegradeAfter consecutive non-good probes (slow or failed) move ONLINE
	// to DEGRADED.
	DegradeAfter int
	// UpAfter consecutive successful probes bring us back from OFFLINE; from
	// DEGRADED, UpAfter consecutive good (fast) probes bring us to ONLINE.
	UpAfter int
}

// DefaultThresholds are deliberately asymmetric: we go down a little faster
// than we come up, because acting on "online" when it is not wastes work,
// while acting on "offline" when it is not only delays it.
func DefaultThresholds() Thresholds {
	return Thresholds{DownAfter: 3, DegradeAfter: 2, UpAfter: 3}
}

// Validate rejects settings that would make the machine nonsensical.
func (t Thresholds) Validate() error {
	if t.DownAfter < 1 || t.DegradeAfter < 1 || t.UpAfter < 1 {
		return fmt.Errorf("thresholds must be at least 1 (got down=%d degrade=%d up=%d)", t.DownAfter, t.DegradeAfter, t.UpAfter)
	}
	return nil
}

// Transition describes a change of state.
type Transition struct {
	From, To State
	Reason   string
}

// Machine is the hysteresis state machine. It is not safe for concurrent use;
// the Monitor owns one and is its only caller.
//
// It counts consecutive runs ("streaks") of each kind of evidence. A single
// odd probe resets a streak instead of changing the state, so one dropped
// packet cannot flip anything: that is hysteresis.
type Machine struct {
	th    Thresholds
	state State

	fail int // consecutive Fail
	bad  int // consecutive Slow-or-Fail
	good int // consecutive Good
	ok   int // consecutive Good-or-Slow
}

// NewMachine starts in Unknown.
func NewMachine(th Thresholds) (*Machine, error) {
	if err := th.Validate(); err != nil {
		return nil, err
	}
	return &Machine{th: th}, nil
}

// State is the current state.
func (m *Machine) State() State { return m.state }

// Observe feeds one probe verdict. It returns the transition if the state
// changed, and nil otherwise.
func (m *Machine) Observe(v Verdict) *Transition {
	m.count(v)

	if m.state == Unknown {
		// Fast start: the very first probe decides at once. Waiting for a
		// streak would leave the program not knowing for several intervals
		// right at startup, when it matters most.
		switch v {
		case Good:
			return m.move(Online, "first probe answered quickly")
		case Slow:
			return m.move(Degraded, "first probe answered slowly")
		default:
			return m.move(Offline, "first probe failed")
		}
	}

	switch m.state {
	case Online:
		if m.fail >= m.th.DownAfter {
			return m.move(Offline, fmt.Sprintf("%d probes failed in a row", m.fail))
		}
		if m.bad >= m.th.DegradeAfter {
			return m.move(Degraded, fmt.Sprintf("%d slow or failed probes in a row", m.bad))
		}
	case Degraded:
		if m.fail >= m.th.DownAfter {
			return m.move(Offline, fmt.Sprintf("%d probes failed in a row", m.fail))
		}
		if m.good >= m.th.UpAfter {
			return m.move(Online, fmt.Sprintf("%d fast probes in a row", m.good))
		}
	case Offline:
		if m.ok >= m.th.UpAfter {
			// Come back carefully: ONLINE only if every probe of the run was
			// fast, otherwise DEGRADED.
			if m.good >= m.th.UpAfter {
				return m.move(Online, fmt.Sprintf("%d fast probes in a row", m.good))
			}
			return m.move(Degraded, fmt.Sprintf("%d probes answered in a row, not all fast", m.ok))
		}
	}
	return nil
}

func (m *Machine) count(v Verdict) {
	switch v {
	case Good:
		m.good++
		m.ok++
		m.fail = 0
		m.bad = 0
	case Slow:
		m.good = 0
		m.ok++
		m.fail = 0
		m.bad++
	default:
		m.good = 0
		m.ok = 0
		m.fail++
		m.bad++
	}
}

func (m *Machine) move(to State, reason string) *Transition {
	t := &Transition{From: m.state, To: to, Reason: reason}
	m.state = to
	// The streaks are deliberately NOT reset here. They are facts about the
	// most recent probes, not about the state: three failures in a row are
	// three failures in a row whether we were Online or already Degraded.
	// Resetting would make us need DegradeAfter + DownAfter failures to reach
	// OFFLINE instead of DownAfter.
	return t
}
