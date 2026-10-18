package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"offgrid/internal/agent"
	"offgrid/internal/connectivity"
	"offgrid/internal/jobqueue"
	"offgrid/internal/obs"
	"offgrid/internal/router"
	"offgrid/internal/store/sqlstore"
)

// openRouting builds the router for `offgrid agent -route`: it opens the job
// queue and decides where connectivity comes from. The returned func closes
// everything, and label says where the connectivity state came from, for
// printing.
//
//	-net offline   pretend there is no network (reproducible demos and tests)
//	-net online    pretend there is
//	-net auto      probe for real, using the same probes as `offgrid net`
func openRouting(ctx context.Context, a *agent.Agent, queueFlag, netMode, probeURLs string, client *http.Client, ob *obs.Observer, stderr io.Writer) (*router.Router, func(), string, error) {
	path := queuePath(queueFlag)
	if path == "" {
		return nil, nil, "", fmt.Errorf("-route needs a queue file: use -queue FILE, or set OFFGRID_QUEUE (or OFFGRID_DB)")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, nil, "", err
	}
	db, err := sqlstore.OpenDB(path)
	if err != nil {
		return nil, nil, "", err
	}
	q, err := jobqueue.New(ctx, db, jobqueue.Config{})
	if err != nil {
		db.Close()
		return nil, nil, "", err
	}

	state, stopNet, label, err := startConnectivity(ctx, netMode, probeURLs, client, 0, ob)
	if err != nil {
		db.Close()
		return nil, nil, "", err
	}

	rt := &router.Router{Agent: a, Queue: q, Net: state}
	closeAll := func() {
		stopNet()
		db.Close()
	}
	return rt, closeAll, label, nil
}

// startConnectivity decides where the connectivity state comes from. It is
// shared by `agent -route` and `drain`. The returned stop func is never nil.
//
//	offline   pretend there is no network (reproducible demos and tests)
//	online    pretend there is
//	auto      probe for real, using the same probes as `offgrid net`
func startConnectivity(ctx context.Context, netMode, probeURLs string, client *http.Client, interval time.Duration, ob *obs.Observer) (state func() connectivity.State, stop func(), label string, err error) {
	switch netMode {
	case "offline":
		ob.Forced(connectivity.Offline, "-net offline")
		return func() connectivity.State { return connectivity.Offline }, func() {}, "OFFLINE (forced with -net offline)", nil
	case "online":
		ob.Forced(connectivity.Online, "-net online")
		return func() connectivity.State { return connectivity.Online }, func() {}, "ONLINE (forced with -net online)", nil
	case "auto":
		var probers []connectivity.Prober
		for _, u := range splitList(probeURLs) {
			probers = append(probers, connectivity.HTTPProber{URL: u, Client: client})
		}
		mon, err := connectivity.New(connectivity.Config{Prober: connectivity.Any(probers...), Interval: interval, OnProbe: ob.Probe})
		if err != nil {
			return nil, nil, "", err
		}
		mctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer close(done); mon.Run(mctx) }()
		go func() { // keep the events channel drained so nothing waits on us
			for e := range mon.Events() {
				ob.Transition(e)
			}
		}()
		stop := func() { cancel(); <-done }
		// The first probe decides the state at once; wait briefly for it so
		// the first decision is not made on "unknown".
		deadline := time.Now().Add(4 * time.Second)
		for mon.State() == connectivity.Unknown && time.Now().Before(deadline) && ctx.Err() == nil {
			time.Sleep(20 * time.Millisecond)
		}
		return mon.State, stop, "probing the network (state shown per message)", nil
	}
	return nil, nil, "", fmt.Errorf("-net must be auto, online or offline (got %q)", netMode)
}
