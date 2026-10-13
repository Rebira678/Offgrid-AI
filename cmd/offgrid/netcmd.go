package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"offgrid/internal/connectivity"
)

// Default probe targets. These are public "connectivity check" endpoints that
// answer 204 with no body. Probing them tells those operators that this
// machine is on the internet, so they are only the default: point -url at your
// own server (Day 19 will use the sync server) to keep it private.
var defaultProbeURLs = "https://cp.cloudflare.com/generate_204,http://connectivitycheck.gstatic.com/generate_204"

// cmdNet watches connectivity live.
//
//	offgrid net                 watch until Ctrl-C
//	offgrid net -once           one probe, exit 0 if the network answered
//	offgrid net -for 30s        watch for 30 seconds, then stop
func cmdNet(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("net", flag.ContinueOnError)
	fs.SetOutput(stderr)
	urls := fs.String("url", defaultProbeURLs, "comma-separated probe URLs; each must answer HTTP 204 (any one answering is enough)")
	dns := fs.String("dns", "", "also probe by resolving this host name (e.g. example.com)")
	interval := fs.Duration("interval", 2*time.Second, "time between probes")
	timeout := fs.Duration("timeout", 1500*time.Millisecond, "a probe slower than this has failed")
	slow := fs.Duration("slow", 800*time.Millisecond, "an answer slower than this is 'slow' (evidence for DEGRADED)")
	down := fs.Int("down", 3, "consecutive failed probes before OFFLINE")
	degrade := fs.Int("degrade", 2, "consecutive slow or failed probes before DEGRADED")
	up := fs.Int("up", 3, "consecutive good probes before coming back")
	once := fs.Bool("once", false, "probe once and exit: 0 if the network answered, 1 if not")
	forDur := fs.Duration("for", 0, "stop after this long (default: run until Ctrl-C)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "error: unexpected argument %q (net takes only flags)\n", fs.Arg(0))
		return 2
	}

	var probers []connectivity.Prober
	for _, u := range splitList(*urls) {
		probers = append(probers, connectivity.HTTPProber{URL: u})
	}
	if *dns != "" {
		probers = append(probers, connectivity.DNSProber{Host: *dns})
	}
	if len(probers) == 0 {
		fmt.Fprintln(stderr, "error: nothing to probe; give -url or -dns")
		return 2
	}
	prober := connectivity.Any(probers...)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *once {
		pctx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		r := prober.Probe(pctx)
		if !r.OK {
			fmt.Fprintf(stdout, "no answer after %s: %v\n", r.Latency.Round(time.Millisecond), r.Err)
			return 1
		}
		fmt.Fprintf(stdout, "answered in %s (%s)\n", r.Latency.Round(time.Millisecond), r.Target)
		return 0
	}

	if *forDur > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *forDur)
		defer cancel()
	}

	// The probe lines are printed by the monitor goroutine and the transition
	// lines by this one, so writes to stdout are serialized: two goroutines
	// sharing one writer is a data race (the race detector caught exactly this).
	var outMu sync.Mutex
	say := func(format string, a ...any) {
		outMu.Lock()
		defer outMu.Unlock()
		fmt.Fprintf(stdout, format, a...)
	}

	mon, err := connectivity.New(connectivity.Config{
		Prober:       prober,
		Interval:     *interval,
		ProbeTimeout: *timeout,
		SlowAfter:    *slow,
		Thresholds:   connectivity.Thresholds{DownAfter: *down, DegradeAfter: *degrade, UpAfter: *up},
		OnProbe: func(r connectivity.Result, v connectivity.Verdict, s connectivity.State) {
			detail := r.Latency.Round(time.Millisecond).String()
			if r.Err != nil {
				detail = r.Err.Error()
			}
			say("%s  probe %-4s %-9s %s\n", time.Now().Format("15:04:05"), v, s, detail)
		},
	})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}

	say("watching connectivity every %s (Ctrl-C to stop)\n", *interval)
	done := make(chan struct{})
	go func() {
		defer close(done)
		mon.Run(ctx)
	}()
	for e := range mon.Events() {
		say("%s  ==> %s -> %s  (%s)\n", e.At.Format("15:04:05"), e.From, e.To, e.Reason)
	}
	<-done
	say("stopped; last state %s\n", mon.State())
	return 0
}
