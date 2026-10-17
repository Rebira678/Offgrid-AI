package netfault

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// A Source answers "is the network partitioned right now?". It is what
// Config.Partitioned is made of, so the switch can be a variable, a file a
// script rewrites, a timetable, or any combination.
type Source = func() (bool, error)

// ---- in-process switch ---------------------------------------------------

// Switch is a partition flag a test can flip.
type Switch struct{ on atomic.Bool }

// Set turns the partition on or off.
func (s *Switch) Set(partitioned bool) { s.on.Store(partitioned) }

// Source adapts the switch for Config.Partitioned.
func (s *Switch) Source() Source { return func() (bool, error) { return s.on.Load(), nil } }

// ---- control file --------------------------------------------------------

// The control file is how a script (or you, in another terminal) flips the
// partition of a RUNNING offgrid process: write "partition" or "ok" into it.
const (
	StatePartition = "partition"
	StateOK        = "ok"
)

// FileSource reads the control file on every request. A missing file means
// "ok". Any content other than the two words is an error, so a typo breaks
// loudly instead of quietly leaving the network up. Reading per request is
// cheap (a few bytes from the page cache) and keeps the switch instant.
func FileSource(path string) Source {
	return func() (bool, error) {
		b, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		switch s := strings.TrimSpace(string(b)); s {
		case StatePartition:
			return true, nil
		case StateOK:
			return false, nil
		default:
			return false, fmt.Errorf("%s: content %q is not %q or %q", path, s, StatePartition, StateOK)
		}
	}
}

// WriteState sets the control file. It writes a temporary file and renames it,
// which is atomic: a reader sees the old content or the new content, never a
// half-written file (a plain write is truncate-then-write, and a reader that
// lands in the gap sees an empty file).
func WriteState(path string, partitioned bool) error {
	state := StateOK
	if partitioned {
		state = StatePartition
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".fault-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.WriteString(state + "\n"); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// ---- timetable -------------------------------------------------------------

type step struct {
	partitioned bool
	d           time.Duration
}

// Schedule is a repeating timetable such as "ok:5s,partition:3s": five
// seconds fine, three seconds down, then again, forever. It is a pure
// function of elapsed time, so a run is reproducible and testable without
// waiting.
type Schedule struct {
	steps []step
	total time.Duration
}

// ParseSchedule reads "state:duration,state:duration,...". State is ok or
// partition; each duration must be positive.
func ParseSchedule(spec string) (Schedule, error) {
	var s Schedule
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, dur, ok := strings.Cut(part, ":")
		if !ok {
			return Schedule{}, fmt.Errorf("schedule step %q: want state:duration, like ok:5s", part)
		}
		var st step
		switch strings.TrimSpace(name) {
		case StateOK:
		case StatePartition:
			st.partitioned = true
		default:
			return Schedule{}, fmt.Errorf("schedule step %q: state must be %q or %q", part, StateOK, StatePartition)
		}
		d, err := time.ParseDuration(strings.TrimSpace(dur))
		if err != nil {
			return Schedule{}, fmt.Errorf("schedule step %q: %v", part, err)
		}
		if d <= 0 {
			return Schedule{}, fmt.Errorf("schedule step %q: duration must be positive", part)
		}
		st.d = d
		s.steps = append(s.steps, st)
		s.total += d
	}
	if len(s.steps) == 0 {
		return Schedule{}, errors.New("schedule is empty")
	}
	return s, nil
}

// PartitionedAt says whether the network is down elapsed time after the
// start. The timetable repeats.
func (s Schedule) PartitionedAt(elapsed time.Duration) bool {
	if elapsed < 0 {
		elapsed = 0
	}
	t := elapsed % s.total
	for _, st := range s.steps {
		if t < st.d {
			return st.partitioned
		}
		t -= st.d
	}
	return false // unreachable: t < total
}

// Source starts the clock now and follows the timetable.
func (s Schedule) Source(now func() time.Time) Source {
	start := now()
	return func() (bool, error) { return s.PartitionedAt(now().Sub(start)), nil }
}

// ---- combining -------------------------------------------------------------

// Either is partitioned when any source says so. The first error wins.
func Either(sources ...Source) Source {
	return func() (bool, error) {
		out := false
		for _, s := range sources {
			if s == nil {
				continue
			}
			p, err := s()
			if err != nil {
				return false, err
			}
			out = out || p
		}
		return out, nil
	}
}
