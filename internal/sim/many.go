package sim

import (
	"sort"
	"sync"
	"time"
)

// Summary is what a sweep of many seeds found.
type Summary struct {
	From, To int64 // seeds From..To inclusive
	Runs     int
	Failed   []Result // sorted by seed; traces included
	Elapsed  time.Duration
	Total    Stats // faults injected, summed over every run
	Ops      int   // ops in all the converged logs, summed
}

// Many runs seeds from..from+n-1 on workers goroutines. Each run is a pure
// function of its seed and has its own files, so the workers share nothing and
// the answer for a seed does not depend on how many workers there are.
func Many(from int64, n, workers int, cfg Config) Summary {
	if workers < 1 {
		workers = 1
	}
	start := time.Now()
	seeds := make(chan int64)
	var mu sync.Mutex
	sum := Summary{From: from, To: from + int64(n) - 1, Runs: n}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s := range seeds {
				r := Run(s, cfg)
				mu.Lock()
				sum.Total.add(r.Stats)
				sum.Ops += r.Ops
				if r.Failure != nil {
					sum.Failed = append(sum.Failed, r)
				}
				mu.Unlock()
			}
		}()
	}
	for i := 0; i < n; i++ {
		seeds <- from + int64(i)
	}
	close(seeds)
	wg.Wait()
	sort.Slice(sum.Failed, func(i, j int) bool { return sum.Failed[i].Seed < sum.Failed[j].Seed })
	sum.Elapsed = time.Since(start)
	return sum
}

func (s *Stats) add(o Stats) {
	s.Edits += o.Edits
	s.Syncs += o.Syncs
	s.Partitions += o.Partitions
	s.Blocked += o.Blocked
	s.Skews += o.Skews
	s.Crashes += o.Crashes
	s.Dropped += o.Dropped
	s.Duplicated += o.Duplicated
	s.Delayed += o.Delayed
	s.Truncated += o.Truncated
	s.DriftRefusals += o.DriftRefusals
	s.Rebuilds += o.Rebuilds
	s.Reversed += o.Reversed
	s.Gapped += o.Gapped
	s.GapRefused += o.GapRefused
}

// Shrink cuts a failing schedule down to a small one that fails the same way
// (same Failure.Class). It is delta debugging: try removing big chunks, then
// smaller ones, keeping every removal that still fails, until no single action
// can be removed. The result is a schedule you can read.
//
// Because actions never name state (see Action), any subset is a valid
// schedule, so removal is always allowed. A shrunk schedule is not "the same
// run with steps missing": later actions may pick different tasks. It only has
// to fail, and the tests check that it does.
func Shrink(res Result, cfg Config) Result {
	if res.Failure == nil {
		return res
	}
	return shrink(res, func(a []Action) Result { return RunActions(res.Seed, a, cfg) })
}

func shrink(res Result, run func([]Action) Result) Result {
	class := res.Failure.Class
	cur := res
	still := func(a []Action) (Result, bool) {
		r := run(a)
		return r, r.Failure != nil && r.Failure.Class == class
	}
	for chunk := len(cur.Actions) / 2; chunk >= 1; {
		removed := false
		for i := 0; i+chunk <= len(cur.Actions); {
			cand := append(append([]Action{}, cur.Actions[:i]...), cur.Actions[i+chunk:]...)
			if r, ok := still(cand); ok {
				cur = r
				removed = true
				continue // the same index now holds new actions
			}
			i += chunk
		}
		if !removed || chunk == 1 {
			if chunk == 1 {
				if !removed {
					break
				}
				continue // one more pass at single actions until stable
			}
			chunk /= 2
		}
	}
	return cur
}
