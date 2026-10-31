package dash

import (
	"sort"
	"time"

	"offgrid/internal/obs"
)

// Summary is the same picture Render draws, as data: the web page and the
// terminal screen are two views of one merge, so they cannot disagree about
// what the system is doing.
type Summary struct {
	HaveNet    bool
	Net        string
	NetSince   time.Time
	NetStale   bool // the process that reported it is not publishing now
	NetStopped bool // ...because it exited cleanly
	NetAt      time.Time
	NetFrom    string
	Probe      *obs.Probe
	ProbesOK   int
	ProbesFail int
	ProbeMS    []int64
	Flips      []obs.Flip

	Jobs    map[string]obs.JobCounts
	JobLat  map[string]obs.Latency
	Tiers   []TierCount // fixed order, only tiers that handled something
	Feed    []FeedEntry // newest first
	Procs   []ProcState // one per publishing process, by name
	Decided []obs.Decision
}

// TierCount is how many messages one tier handled, and how long it took.
type TierCount struct {
	Name string
	N    int
	Lat  obs.Latency
}

// FeedEntry is one recent event and the process that reported it.
type FeedEntry struct {
	obs.FeedLine
	Proc string
}

// ProcState is one process that publishes snapshots.
type ProcState struct {
	Name    string
	Fresh   bool
	Stopped bool
	Age     time.Duration
}

// Summarize merges the snapshots exactly as Render does.
func Summarize(snaps []obs.Snapshot, now time.Time) Summary {
	snaps = append([]obs.Snapshot(nil), snaps...)
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].Proc < snaps[j].Proc })
	m := merge(snaps, now)
	s := Summary{
		HaveNet: m.haveNet, Net: m.net, NetSince: m.netSince, NetStale: m.netStale, NetStopped: m.netStop,
		NetAt: m.netAt, NetFrom: m.netFrom, Probe: m.probe, ProbesOK: m.probes.ok, ProbesFail: m.probes.fail,
		ProbeMS: m.probeMS, Flips: m.flips, Jobs: m.jobs, JobLat: m.jobLat,
	}
	for _, t := range tierOrder {
		if n := m.tiers[t]; n > 0 {
			s.Tiers = append(s.Tiers, TierCount{Name: t, N: n, Lat: m.tierLat[t]})
		}
	}
	for _, f := range m.feed {
		s.Feed = append(s.Feed, FeedEntry{f.FeedLine, f.proc})
	}
	for _, sn := range snaps {
		s.Procs = append(s.Procs, ProcState{Name: sn.Proc, Fresh: sn.Fresh(now), Stopped: sn.Stopped, Age: sn.Age(now)})
		s.Decided = append(s.Decided, sn.Decisions...)
	}
	sort.SliceStable(s.Decided, func(i, j int) bool { return s.Decided[i].At.After(s.Decided[j].At) })
	return s
}
