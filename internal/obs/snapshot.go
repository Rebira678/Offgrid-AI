package obs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"offgrid/internal/jobqueue"
)

// SnapshotVersion is bumped when the file layout changes.
const SnapshotVersion = 1

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// Snapshot is a process's counters at one moment, as written to a file.
type Snapshot struct {
	Version  int           `json:"version"`
	Proc     string        `json:"proc"` // "drain", "agent"
	PID      int           `json:"pid"`
	Started  time.Time     `json:"started"`
	At       time.Time     `json:"at"`
	Interval time.Duration `json:"interval"` // how often it republishes
	Stopped  bool          `json:"stopped"`  // the process exited cleanly

	Net       string                 `json:"net"`
	NetSince  time.Time              `json:"net_since"`
	Probes    struct{ OK, Fail int } `json:"probes"`
	LastProbe *Probe                 `json:"last_probe,omitempty"`
	ProbeMS   []int64                `json:"probe_ms,omitempty"` // recent probe latencies in ms; -1 marks a failed probe
	Flips     []Flip                 `json:"flips,omitempty"`

	Queue *jobqueue.Stats `json:"queue,omitempty"`

	Jobs       map[string]JobCounts `json:"jobs,omitempty"`
	JobLatency map[string]Latency   `json:"job_latency,omitempty"`
	Tiers      map[string]int       `json:"tiers,omitempty"`
	TierLat    map[string]Latency   `json:"tier_latency,omitempty"`
	Decisions  []Decision           `json:"decisions,omitempty"`
	Feed       []FeedLine           `json:"feed,omitempty"`
}

// Snapshot copies the counters. proc names the process; started is when it
// began (zero is fine).
func (m *Metrics) Snapshot(proc string, pid int, started time.Time, interval time.Duration) Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Snapshot{Version: SnapshotVersion, Proc: proc, PID: pid, Started: started, At: m.now(), Interval: interval,
		Net: m.net, NetSince: m.netSince, Probes: struct{ OK, Fail int }{m.probes.ok, m.probes.fail},
		Flips: append([]Flip(nil), m.flips...), Decisions: append([]Decision(nil), m.decision...),
		Feed: append([]FeedLine(nil), m.feed...),
		Jobs: map[string]JobCounts{}, JobLatency: map[string]Latency{}, Tiers: map[string]int{}, TierLat: map[string]Latency{}}
	if m.last != nil {
		p := *m.last
		s.LastProbe = &p
	}
	for _, d := range m.probeLat {
		if d < 0 {
			s.ProbeMS = append(s.ProbeMS, -1)
			continue
		}
		s.ProbeMS = append(s.ProbeMS, d.Milliseconds())
	}
	if m.queue != nil {
		q := *m.queue
		s.Queue = &q
	}
	for k, c := range m.jobs {
		s.Jobs[k] = *c
	}
	for k, v := range m.jobLat {
		s.JobLatency[k] = Summarize(v)
	}
	for k, v := range m.tiers {
		s.Tiers[k] = v
	}
	for k, v := range m.tierLat {
		s.TierLat[k] = Summarize(v)
	}
	return s
}

// WriteSnapshot writes s to path atomically (temp file, then rename), so a
// reader never sees half a file. It is not fsynced: a snapshot lost in a
// crash is simply stale, and the next one replaces it.
func WriteSnapshot(path string, s Snapshot) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".snap-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("write snapshot: %w", errFirst(werr, cerr))
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

func errFirst(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// ReadSnapshots reads every *.json snapshot in dir. A missing dir is no
// snapshots. A file that cannot be read or parsed is reported in problems
// and skipped: one bad file must not blank the screen.
func ReadSnapshots(dir string) (snaps []Snapshot, problems []string) {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, []string{err.Error()}
	}
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", filepath.Base(f), err))
			continue
		}
		var s Snapshot
		if err := json.Unmarshal(b, &s); err != nil {
			problems = append(problems, fmt.Sprintf("%s: not a snapshot (%v)", filepath.Base(f), err))
			continue
		}
		if s.Version != SnapshotVersion {
			problems = append(problems, fmt.Sprintf("%s: snapshot version %d, expected %d", filepath.Base(f), s.Version, SnapshotVersion))
			continue
		}
		snaps = append(snaps, s)
	}
	return snaps, problems
}

// Age is how long ago s was written.
func (s Snapshot) Age(now time.Time) time.Duration { return max(now.Sub(s.At), 0) }

// Fresh reports whether the process is probably still running: it wrote
// within three of its own intervals, and did not say it had stopped.
func (s Snapshot) Fresh(now time.Time) bool {
	iv := s.Interval
	if iv <= 0 {
		iv = time.Second
	}
	return !s.Stopped && s.Age(now) <= 3*iv+500*time.Millisecond
}
