package sim

import (
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// Checkpoint 2: a signed group under attack. Every schedule has one device revoked,
// forged ops of four kinds, the revoked device writing and syncing on, and old
// requests replayed. The convergence checks of the plain simulator all still hold, and
// so do the security checks: no forgery is ever stored, the void set is exactly the
// revoked device's work past its cutoff, on every replica.

func secureCfg() Config { return Config{Secure: true, Steps: 80} }

func runSeeds(from int64, n, workers int, cfg Config) (failed, all []Result) {
	var mu sync.Mutex
	seeds := make(chan int64)
	var wg sync.WaitGroup
	for k := 0; k < workers; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for sd := range seeds {
				r := Run(sd, cfg)
				mu.Lock()
				all = append(all, r)
				if r.Failure != nil {
					failed = append(failed, r)
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
	return
}

func TestThreeThousandHostileSchedulesPass(t *testing.T) {
	n := 3000
	if testing.Short() {
		n = 300
	}
	failed, all := runSeeds(1, n, runtime.NumCPU(), secureCfg())
	for i, r := range failed {
		if i >= 3 {
			break
		}
		t.Errorf("seed %d: %v\n%s\nreplay: Run(%d, Config{Secure: true, Steps: 80})", r.Seed, r.Failure,
			strings.Join(r.Trace[max(0, len(r.Trace)-8):], "\n"), r.Seed)
	}
	if len(failed) > 0 {
		t.Fatalf("%d of %d schedules failed", len(failed), len(all))
	}
	var total Stats
	for _, r := range all {
		s := r.Stats
		total.add(s)
		// Per schedule, not only in total: every run must have been hostile.
		switch {
		case s.Revokes != 1:
			t.Fatalf("seed %d: %d revocations, want exactly 1", r.Seed, s.Revokes)
		case s.Forged < 5:
			t.Fatalf("seed %d: only %d forged ops tried (want one of each of 5 kinds)", r.Seed, s.Forged)
		case s.ForgedRefused+s.ForgedIgnored != s.Forged:
			t.Fatalf("seed %d: %d forged, %d refused, %d ignored: a forgery was neither refused nor ignored", r.Seed, s.Forged, s.ForgedRefused, s.ForgedIgnored)
		case s.ForgedRefused < 2:
			t.Fatalf("seed %d: only %d forgeries were refused", r.Seed, s.ForgedRefused)
		case s.Replays < 2:
			t.Fatalf("seed %d: %d replays", r.Seed, s.Replays)
		case s.Rogue < 1:
			t.Fatalf("seed %d: %d rogue actions", r.Seed, s.Rogue)
		}
	}
	if total.VoidOps == 0 {
		t.Fatal("no schedule ended with void ops: the revoked device never got work past its cutoff")
	}
	t.Logf("%d schedules: %+v", len(all), total)
}

func TestAHostileScheduleReplaysExactlyFromItsSeed(t *testing.T) {
	cfg := secureCfg()
	for _, seed := range []int64{1, 5, 11, 100, 2024} {
		a, b := Run(seed, cfg), Run(seed, cfg)
		if a.Failure != nil || b.Failure != nil {
			t.Fatalf("seed %d: %v / %v", seed, a.Failure, b.Failure)
		}
		if a.Hash != b.Hash || !reflect.DeepEqual(a.Trace, b.Trace) || !reflect.DeepEqual(a.Actions, b.Actions) {
			t.Fatalf("seed %d: two runs of one seed differ", seed)
		}
		// The printed schedule is enough to replay it: parse every line back.
		var parsed []Action
		for _, act := range a.Actions {
			p, err := ParseAction(act.String())
			if err != nil {
				t.Fatalf("seed %d: %q does not parse: %v", seed, act.String(), err)
			}
			parsed = append(parsed, p)
		}
		c := RunActions(seed, parsed, cfg)
		if c.Hash != a.Hash || !reflect.DeepEqual(c.Trace, a.Trace) {
			t.Fatalf("seed %d: the parsed schedule did not replay the same run", seed)
		}
	}
}

func TestHostileKindsRoundTripAndPlainSchedulesAreUnchanged(t *testing.T) {
	for _, k := range []Kind{Revoke, Forge, Rogue, Replay} {
		a := Action{Kind: k, A: 1, N: 7}
		p, err := ParseAction(a.String())
		if err != nil || p != a {
			t.Fatalf("%v: %q -> %+v, %v", k, a.String(), p, err)
		}
	}
	for _, seed := range []int64{1, 2, 3} {
		for _, a := range Generate(seed, Config{Steps: 80}) {
			if isHostile(a.Kind) {
				t.Fatalf("a plain schedule contains %v: plain runs must not change", a)
			}
		}
		hostile := 0
		for _, a := range Generate(seed, secureCfg()) {
			if isHostile(a.Kind) {
				hostile++
			}
		}
		if hostile < 10 {
			t.Fatalf("seed %d: a secure schedule has only %d hostile actions", seed, hostile)
		}
	}
}

func TestAFailingHostileScheduleShrinksToTheActionsThatCauseIt(t *testing.T) {
	acts := Generate(42, secureCfg())
	// A pretend bug that needs a forgery and a replay: shrinking must keep exactly those.
	fails := func(as []Action) Result {
		var f, r bool
		for _, a := range as {
			f = f || a.Kind == Forge
			r = r || a.Kind == Replay
		}
		res := Result{Seed: 42, Actions: as}
		if f && r {
			res.Failure = &Failure{"fake", "forged and replayed"}
		}
		return res
	}
	min := shrink(fails(acts), fails)
	if min.Failure == nil || len(min.Actions) != 2 {
		t.Fatalf("shrunk to %d actions: %v", len(min.Actions), min.Actions)
	}
	got := fmt.Sprint(min.Actions[0].Kind, min.Actions[1].Kind)
	if !strings.Contains(got, "forge") && !strings.Contains(got, fmt.Sprint(Forge)) {
		t.Fatalf("kept %s", got)
	}
}
