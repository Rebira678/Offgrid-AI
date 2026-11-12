// Package soak samples a running process (memory and goroutines) and judges a
// long run. A node that is installed once and left must not slowly grow: a
// leaked goroutine per sync round, or memory that is never given back, is
// invisible in a ten-second test and fatal after a week. The soak is how it is
// seen in an hour.
//
// The sampler writes one JSON line per sample, so the file can be copied off a
// Raspberry Pi and read anywhere. Summarize turns it into a verdict with
// thresholds that are written down, not decided after the numbers are in.
package soak

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Sample is one reading.
type Sample struct {
	At         time.Time `json:"at"`
	Role       string    `json:"role,omitempty"` // "serve", "run", ...
	Goroutines int       `json:"goroutines"`
	HeapInuse  uint64    `json:"heap_inuse"` // bytes the Go heap holds
	RSSKB      int64     `json:"rss_kb"`     // what the OS says the process uses; 0 where unknown (not Linux)
	Ops        int64     `json:"ops,omitempty"`
	Rounds     int64     `json:"rounds,omitempty"`
	Failures   int64     `json:"failures,omitempty"`
}

// Extra lets the owner of the loop add its own counters to each sample.
type Extra func(*Sample)

// Take reads the process now.
func Take(role string, extra Extra) Sample {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	s := Sample{At: time.Now().UTC(), Role: role, Goroutines: runtime.NumGoroutine(), HeapInuse: ms.HeapInuse, RSSKB: rssKB()}
	if extra != nil {
		extra(&s)
	}
	return s
}

// rssKB reads VmRSS from /proc (Linux, which is a Pi and the WSL2 desktop).
// Elsewhere it returns 0 and the report says RSS was not measured.
func rssKB() int64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	return parseRSS(string(b))
}

func parseRSS(status string) int64 {
	for _, line := range strings.Split(status, "\n") {
		if rest, ok := strings.CutPrefix(line, "VmRSS:"); ok {
			f := strings.Fields(rest)
			if len(f) >= 1 {
				if n, err := strconv.ParseInt(f[0], 10, 64); err == nil {
					return n
				}
			}
		}
	}
	return 0
}

// Run writes a sample now and then every interval until ctx ends, then one last
// sample, so a run always has a first and a last reading.
func Run(ctx context.Context, w io.Writer, role string, every time.Duration, extra Extra) error {
	if every <= 0 {
		return errors.New("soak: the interval must be positive")
	}
	enc := json.NewEncoder(w)
	write := func() error { return enc.Encode(Take(role, extra)) }
	if err := write(); err != nil {
		return err
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return write()
		case <-t.C:
			if err := write(); err != nil {
				return err
			}
		}
	}
}

// Limits are the thresholds, fixed before a run.
type Limits struct {
	// WarmUp is the share of the run (0..1) ignored at the start: caches fill and
	// pools grow in the first minutes, and that is not a leak.
	WarmUp float64
	// MaxGoroutineGrowth is how many more goroutines the end may show than the
	// steady level after warm-up. A leak per round shows as growth in proportion to rounds.
	MaxGoroutineGrowth int
	// MaxRSSGrowthPct is how far the end may be above the steady level after warm-up, in percent.
	MaxRSSGrowthPct float64
	// MinSamples and MinDuration below which no verdict is given: a short run
	// proves nothing, however many samples it took.
	MinSamples  int
	MinDuration time.Duration
}

// DefaultLimits: 10% warm-up; 5 goroutines; 20% RSS. The goroutine limit is
// small on purpose: a server with a bounded number of connections has a flat
// goroutine count. RSS is looser because the Go runtime returns memory lazily.
var DefaultLimits = Limits{WarmUp: 0.10, MaxGoroutineGrowth: 5, MaxRSSGrowthPct: 20, MinSamples: 20, MinDuration: 10 * time.Minute}

// Report is what a soak showed.
type Report struct {
	Samples  int
	Duration time.Duration
	Role     string

	GoroutinesStart, GoroutinesMax, GoroutinesEnd int
	GoroutinesSteady                              int // median after warm-up
	RSSStartKB, RSSMaxKB, RSSEndKB                int64
	RSSSteadyKB                                   int64 // median after warm-up
	RSSMeasured                                   bool
	Ops, Rounds, Failures                         int64

	Problems []string // empty means the run passed
	Verdict  string   // PASS, FAIL or TOO SHORT
}

// Summarize reads samples (one JSON per line) and judges them against lim.
func Summarize(r io.Reader, lim Limits) (Report, error) {
	var all []Sample
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var s Sample
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			return Report{}, fmt.Errorf("soak: line %d is not a sample: %w", len(all)+1, err)
		}
		all = append(all, s)
	}
	if err := sc.Err(); err != nil {
		return Report{}, err
	}
	if len(all) == 0 {
		return Report{}, errors.New("soak: no samples")
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].At.Before(all[j].At) })

	rep := Report{Samples: len(all), Role: all[0].Role, Duration: all[len(all)-1].At.Sub(all[0].At)}
	first, last := all[0], all[len(all)-1]
	rep.GoroutinesStart, rep.GoroutinesEnd = first.Goroutines, last.Goroutines
	rep.RSSStartKB, rep.RSSEndKB = first.RSSKB, last.RSSKB
	rep.Ops, rep.Rounds, rep.Failures = last.Ops, last.Rounds, last.Failures
	for _, s := range all {
		if s.Goroutines > rep.GoroutinesMax {
			rep.GoroutinesMax = s.Goroutines
		}
		if s.RSSKB > rep.RSSMaxKB {
			rep.RSSMaxKB = s.RSSKB
		}
	}
	rep.RSSMeasured = rep.RSSMaxKB > 0

	if len(all) < lim.MinSamples {
		rep.Verdict = "TOO SHORT"
		rep.Problems = append(rep.Problems, fmt.Sprintf("only %d samples (need %d): a short run proves nothing", len(all), lim.MinSamples))
		return rep, nil
	}
	if rep.Duration < lim.MinDuration {
		rep.Verdict = "TOO SHORT"
		rep.Problems = append(rep.Problems, fmt.Sprintf("the run lasted %s (need %s): a leak that costs one goroutine per sync round does not show in seconds", rep.Duration.Round(time.Second), lim.MinDuration))
		return rep, nil
	}
	skip := int(float64(len(all)) * lim.WarmUp)
	steady := all[skip:]
	var gs []int
	var rs []int64
	for _, s := range steady {
		gs = append(gs, s.Goroutines)
		rs = append(rs, s.RSSKB)
	}
	rep.GoroutinesSteady = medianInt(gs)
	rep.RSSSteadyKB = medianInt64(rs)

	// The end is judged by the median of the last few samples, not one reading:
	// one sample can land in the middle of a burst.
	tail := steady
	if len(tail) > 5 {
		tail = tail[len(tail)-5:]
	}
	var tg []int
	var tr []int64
	for _, s := range tail {
		tg = append(tg, s.Goroutines)
		tr = append(tr, s.RSSKB)
	}
	endG, endR := medianInt(tg), medianInt64(tr)

	// The steady level is the LOWEST median of the first half of the steady
	// part: if the run only ever climbs, the early level is the honest baseline.
	half := steady[:len(steady)/2+1]
	var hg []int
	var hr []int64
	for _, s := range half {
		hg = append(hg, s.Goroutines)
		hr = append(hr, s.RSSKB)
	}
	baseG, baseR := medianInt(hg), medianInt64(hr)

	if endG-baseG > lim.MaxGoroutineGrowth {
		rep.Problems = append(rep.Problems, fmt.Sprintf("goroutines grew from %d to %d after warm-up (limit +%d): something is not being stopped", baseG, endG, lim.MaxGoroutineGrowth))
	}
	if rep.RSSMeasured && baseR > 0 {
		pct := float64(endR-baseR) * 100 / float64(baseR)
		if pct > lim.MaxRSSGrowthPct {
			rep.Problems = append(rep.Problems, fmt.Sprintf("RSS grew %.1f%% after warm-up (%d to %d KB; limit %.0f%%)", pct, baseR, endR, lim.MaxRSSGrowthPct))
		}
	}
	if rep.Rounds > 0 && rep.Failures*2 > rep.Rounds {
		rep.Problems = append(rep.Problems, fmt.Sprintf("%d of %d sync rounds failed: a soak with a broken link says nothing about leaks", rep.Failures, rep.Rounds))
	}
	rep.Verdict = "PASS"
	if len(rep.Problems) > 0 {
		rep.Verdict = "FAIL"
	}
	return rep, nil
}

func medianInt(v []int) int {
	if len(v) == 0 {
		return 0
	}
	c := append([]int(nil), v...)
	sort.Ints(c)
	return c[len(c)/2]
}

func medianInt64(v []int64) int64 {
	if len(v) == 0 {
		return 0
	}
	c := append([]int64(nil), v...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return c[len(c)/2]
}

// Format prints a report for a person.
func (r Report) Format() string {
	var b strings.Builder
	fmt.Fprintf(&b, "soak %s: %d samples over %s\n", orDash(r.Role), r.Samples, r.Duration.Round(time.Second))
	fmt.Fprintf(&b, "  goroutines  start %d  max %d  end %d  (steady %d)\n", r.GoroutinesStart, r.GoroutinesMax, r.GoroutinesEnd, r.GoroutinesSteady)
	if r.RSSMeasured {
		fmt.Fprintf(&b, "  RSS         start %d KB  max %d KB  end %d KB  (steady %d KB)\n", r.RSSStartKB, r.RSSMaxKB, r.RSSEndKB, r.RSSSteadyKB)
	} else {
		fmt.Fprintf(&b, "  RSS         not measured (no /proc on this system)\n")
	}
	if r.Rounds > 0 || r.Ops > 0 {
		fmt.Fprintf(&b, "  work        %d rounds, %d failed, %d ops\n", r.Rounds, r.Failures, r.Ops)
	}
	for _, p := range r.Problems {
		fmt.Fprintf(&b, "  PROBLEM: %s\n", p)
	}
	fmt.Fprintf(&b, "RESULT: %s\n", r.Verdict)
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
