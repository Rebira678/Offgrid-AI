package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"offgrid/internal/soak"
	"offgrid/internal/store/sqlstore"
)

// sharedKeyWarning is printed by every sync command that still authenticates
// with the shared key (Day 37). It must say what is wrong, what replaces it,
// and that nothing breaks today.
const sharedKeyWarning = "warning: the shared sync key is deprecated. Every device that holds it looks the same to its peers, so it cannot say WHICH device sent a request.\n" +
	"         Ops are signed per device now (make a key with: offgrid identity init); per-device TLS replaces the shared key on Day 40. It still works for now."

func warnSharedKey(w io.Writer) { fmt.Fprintln(w, sharedKeyWarning) }

const statsHelp = "append a memory and goroutine sample to this file every -stats-every (for a long soak; read it with: offgrid sync stats FILE)"

type statsFlags struct {
	file  *string
	every *time.Duration
}

func addStatsFlags(fs *flag.FlagSet) statsFlags {
	return statsFlags{
		file:  fs.String("stats", "", statsHelp),
		every: fs.Duration("stats-every", 10*time.Second, "time between -stats samples"),
	}
}

// work are the counters a sync loop keeps for the sampler.
type work struct{ rounds, failures atomic.Int64 }

// start begins sampling if -stats was given, and returns a function that stops
// it and waits for the last line to be written. Call it before the store closes.
func (f statsFlags) start(ctx context.Context, stderr io.Writer, role string, st *sqlstore.Store, w *work) (stop func(), err error) {
	if *f.file == "" {
		return func() {}, nil
	}
	out, err := os.OpenFile(*f.file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("-stats: %w", err)
	}
	sctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		extra := func(s *soak.Sample) {
			if w != nil {
				s.Rounds, s.Failures = w.rounds.Load(), w.failures.Load()
			}
			qctx, qcancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer qcancel()
			if sum, err := st.Summary(qctx); err == nil {
				for _, seq := range sum {
					s.Ops += seq
				}
			}
		}
		if err := soak.Run(sctx, out, role, *f.every, extra); err != nil {
			fmt.Fprintln(stderr, "warning: -stats stopped:", err)
		}
	}()
	return func() { cancel(); wg.Wait(); out.Close() }, nil
}

// cmdSyncStats judges a stats file: offgrid sync stats FILE.
func cmdSyncStats(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sync stats", flag.ContinueOnError)
	fs.SetOutput(stderr)
	lim := soak.DefaultLimits
	fs.Float64Var(&lim.WarmUp, "warmup", lim.WarmUp, "share of the run (0 to 1) ignored at the start")
	fs.IntVar(&lim.MaxGoroutineGrowth, "max-goroutine-growth", lim.MaxGoroutineGrowth, "goroutines the end may exceed the steady level by")
	fs.Float64Var(&lim.MaxRSSGrowthPct, "max-rss-growth", lim.MaxRSSGrowthPct, "percent the end RSS may exceed the steady level by")
	fs.IntVar(&lim.MinSamples, "min-samples", lim.MinSamples, "fewer samples than this gives no verdict")
	fs.DurationVar(&lim.MinDuration, "min-duration", lim.MinDuration, "a run shorter than this gives no verdict")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: offgrid sync stats [-warmup F] [-max-goroutine-growth N] [-max-rss-growth PCT] FILE")
		return 2
	}
	f, err := os.Open(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	defer f.Close()
	rep, err := soak.Summarize(f, lim)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprint(stdout, rep.Format())
	if rep.Verdict != "PASS" {
		return 1
	}
	return 0
}
