package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"offgrid/internal/agent"
	"offgrid/internal/connectivity"
	"offgrid/internal/jobqueue"
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
func openRouting(ctx context.Context, a *agent.Agent, queueFlag, netMode, probeURLs string, stderr io.Writer) (*router.Router, func(), string, error) {
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

	var state func() connectivity.State
	var stopNet func()
	var label string
	switch netMode {
	case "offline":
		state, label = func() connectivity.State { return connectivity.Offline }, "OFFLINE (forced with -net offline)"
	case "online":
		state, label = func() connectivity.State { return connectivity.Online }, "ONLINE (forced with -net online)"
	case "auto":
		var probers []connectivity.Prober
		for _, u := range splitList(probeURLs) {
			probers = append(probers, connectivity.HTTPProber{URL: u})
		}
		mon, err := connectivity.New(connectivity.Config{Prober: connectivity.Any(probers...)})
		if err != nil {
			db.Close()
			return nil, nil, "", err
		}
		mctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer close(done); mon.Run(mctx) }()
		go func() { // keep the events channel drained so nothing waits on us
			for range mon.Events() {
			}
		}()
		stopNet = func() { cancel(); <-done }
		// The first probe decides the state at once; wait briefly for it so
		// the first message is not routed on "unknown".
		deadline := time.Now().Add(4 * time.Second)
		for mon.State() == connectivity.Unknown && time.Now().Before(deadline) && ctx.Err() == nil {
			time.Sleep(20 * time.Millisecond)
		}
		state, label = mon.State, "probing the network (state shown per message)"
	default:
		db.Close()
		return nil, nil, "", fmt.Errorf("-net must be auto, online or offline (got %q)", netMode)
	}

	rt := &router.Router{Agent: a, Queue: q, Net: state}
	closeAll := func() {
		if stopNet != nil {
			stopNet()
		}
		db.Close()
	}
	return rt, closeAll, label, nil
}
