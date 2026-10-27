// Package idle answers one question: is this laptop quiet enough to start a
// long, CPU-hungry job?
//
// Offgrid's slow tier runs a 7-8B model. On a CPU-only laptop that model uses
// every core for a minute or more, and the fast tier (the 1-3B model that
// answers a person standing in front of the screen) would crawl beside it. So
// slow jobs start only when the machine is quiet.
//
// "Quiet" is measured, not guessed: the share of CPU time spent not-idle since
// the previous look, from the kernel's own counters (/proc/stat on Linux).
package idle

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Counters are the kernel's running totals, in any unit (jiffies). Only their
// difference between two looks means anything.
type Counters struct {
	Idle  uint64 // time doing nothing (including waiting for the disk)
	Total uint64 // all time
}

// ParseProcStat reads the aggregate "cpu" line of /proc/stat.
func ParseProcStat(r *bufio.Scanner) (Counters, error) {
	for r.Scan() {
		f := strings.Fields(r.Text())
		if len(f) == 0 || f[0] != "cpu" {
			continue
		}
		if len(f) < 5 {
			return Counters{}, errors.New("idle: the cpu line has too few fields")
		}
		var c Counters
		// user nice system idle iowait irq softirq steal (guest times are already in user/nice)
		for i, s := range f[1:] {
			if i >= 8 {
				break
			}
			v, err := strconv.ParseUint(s, 10, 64)
			if err != nil {
				return Counters{}, fmt.Errorf("idle: bad number %q in the cpu line", s)
			}
			c.Total += v
			if i == 3 || i == 4 {
				c.Idle += v
			}
		}
		return c, nil
	}
	return Counters{}, errors.New("idle: no cpu line")
}

// ReadProcStat reads the counters from /proc/stat.
func ReadProcStat() (Counters, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return Counters{}, err
	}
	defer f.Close()
	return ParseProcStat(bufio.NewScanner(f))
}

// Sampler turns counters into a verdict. Safe for concurrent use.
type Sampler struct {
	// Read returns the counters. Default: ReadProcStat.
	Read func() (Counters, error)
	// MaxBusy is the busiest the CPU may be, 0 to 1, and still count as quiet.
	// Default 0.5.
	MaxBusy float64
	// Window is how much time must pass before a new measurement is taken;
	// calls inside it repeat the last verdict. Default 2 seconds.
	Window time.Duration
	// Now is the clock. Default time.Now.
	Now func() time.Time

	mu      sync.Mutex
	started bool
	last    Counters
	at      time.Time
	quiet   bool
	busy    float64
}

// Quiet reports whether the machine was quiet over the last window. The very
// first call has nothing to compare with and says false. If the
// counters cannot be read (a platform without /proc/stat) it says true: the
// gate must not be what stops a person's work from ever running.
func (s *Sampler) Quiet() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	read, now := s.Read, s.Now
	if read == nil {
		read = ReadProcStat
	}
	if now == nil {
		now = time.Now
	}
	maxBusy, window := s.MaxBusy, s.Window
	if maxBusy <= 0 {
		maxBusy = 0.5
	}
	if window <= 0 {
		window = 2 * time.Second
	}
	t := now()
	if s.started && t.Sub(s.at) < window {
		return s.quiet
	}
	c, err := read()
	if err != nil {
		s.started, s.at, s.quiet = true, t, true
		return true
	}
	if !s.started {
		// No earlier look to compare with: take a baseline and say "not yet".
		// Starting a minute of 7B inference on a machine we know nothing about
		// is the one mistake this gate exists to prevent; waiting one Window
		// (2 seconds) on a quiet machine costs nothing.
		s.started, s.last, s.at, s.quiet = true, c, t, false
		return false
	}
	dTotal := c.Total - s.last.Total
	dIdle := c.Idle - s.last.Idle
	if c.Total < s.last.Total || c.Idle < s.last.Idle || dTotal == 0 || dIdle > dTotal {
		// Counters went backwards or did not move: no information, keep the old verdict.
		s.last, s.at = c, t
		return s.quiet
	}
	s.busy = 1 - float64(dIdle)/float64(dTotal)
	s.quiet = s.busy <= maxBusy
	s.last, s.at = c, t
	return s.quiet
}

// Busy is the CPU busy share (0 to 1) measured at the last look.
func (s *Sampler) Busy() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy
}
