package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"offgrid/internal/netfault"
	"offgrid/internal/store/sqlstore"
	"offgrid/internal/syncer"
)

const syncHelp = `offgrid sync exchanges tasks with another replica over HTTP.

  offgrid sync serve   -db FILE [-addr 127.0.0.1:7878] [-key-file FILE] [-window 5m] [-max-drift 1h]
  offgrid sync once    -db FILE -peer URL [-key-file FILE] [-batch N] [-max-drift 1h]
  offgrid sync run     -db FILE -peer URL [-every 2s] [-for D] ...    keep syncing; survives the network going away
  offgrid sync summary -db FILE

once and run take the -fault-* flags (see "offgrid fault"), so the connection can
be broken on purpose: start with -fault-file F, then "offgrid fault set -file F partition".

Both replicas need the same key: -key-file FILE, or $OFFGRID_SYNC_KEY
(at least 16 bytes; make one with: openssl rand -hex 32).

Every request is HMAC-SHA256 signed. A request whose timestamp is further than
-window from this computer's clock is refused (replay defense).

sync never sends anything beyond -peer. It is designed for links that go away:
re-running it after a break brings both replicas into step.`

func cmdSync(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprintln(stderr, syncHelp)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	switch args[0] {
	case "serve":
		return cmdSyncServe(args[1:], stdout, stderr)
	case "once":
		return cmdSyncOnce(args[1:], stdout, stderr)
	case "run":
		return cmdSyncRun(args[1:], stdout, stderr)
	case "summary":
		return cmdSyncSummary(args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "error: unknown sync command %q (serve, once, run or summary)\n", args[0])
	return 2
}

func openSyncStore(ctx context.Context, db string, maxDrift time.Duration) (*sqlstore.Store, error) {
	if db == "" {
		db = os.Getenv("OFFGRID_DB")
	}
	if db == "" {
		return nil, errors.New("-db FILE is required (or set $OFFGRID_DB)")
	}
	return sqlstore.Open(ctx, db, sqlstore.Options{NodeID: nodeFromEnv(), MaxDrift: maxDrift})
}

func cmdSyncServe(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return cmdSyncServeContext(ctx, args, stdout, stderr)
}

func cmdSyncServeContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sync serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", "", dbHelp)
	addr := fs.String("addr", "127.0.0.1:7878", "address to listen on")
	keyFile := fs.String("key-file", "", "file holding the shared key (default: $OFFGRID_SYNC_KEY)")
	window := fs.Duration("window", syncer.DefaultWindow, "how far a request's time may differ from this clock")
	maxDrift := fs.Duration("max-drift", 0, "refuse ops stamped further ahead than this (default 1h)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	key, err := syncer.LoadKey(*keyFile)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	st, err := openSyncStore(ctx, *db, *maxDrift)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	defer st.Close()
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if host, _, _ := net.SplitHostPort(ln.Addr().String()); !net.ParseIP(host).IsLoopback() {
		fmt.Fprintln(stderr, "warning: listening beyond this computer. Requests are signed but NOT encrypted; use a trusted network or TLS.")
	}
	srv := &http.Server{
		Handler:           syncer.Handler(st, syncer.ServerOptions{Key: key, Window: *window, Log: log.New(stderr, "sync: ", log.LstdFlags)}),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
	}
	// Scripts and tests read this line to learn the port when -addr ends in :0.
	fmt.Fprintf(stdout, "offgrid sync: node %s serving on http://%s\n", st.NodeID(), ln.Addr())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
		fmt.Fprintln(stdout, "offgrid sync: stopped")
		return 0
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return 0
		}
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
}

func cmdSyncOnce(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sync once", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", "", dbHelp)
	peer := fs.String("peer", "", "base URL of the other replica, e.g. http://127.0.0.1:7878")
	keyFile := fs.String("key-file", "", "file holding the shared key (default: $OFFGRID_SYNC_KEY)")
	batch := fs.Int("batch", syncer.DefaultBatch, "ops per request")
	maxDrift := fs.Duration("max-drift", 0, "refuse ops stamped further ahead than this (default 1h)")
	timeout := fs.Duration("timeout", 2*time.Minute, "give up after this long")
	faults := addFaultFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *peer == "" {
		fmt.Fprintln(stderr, "error: -peer URL is required")
		return 2
	}
	key, err := syncer.LoadKey(*keyFile)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	st, err := openSyncStore(ctx, *db, *maxDrift)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	defer st.Close()
	cl := &syncer.Client{Peer: *peer, Key: key, Node: st.NodeID(), Batch: *batch}
	ftr, fdesc, ferr := faults.build()
	if ferr != nil {
		fmt.Fprintln(stderr, "error:", ferr)
		return 2
	}
	if ftr != nil {
		cl.HTTP = &http.Client{Transport: ftr, Timeout: 30 * time.Second}
		fmt.Fprintf(stdout, "FAULT INJECTION ON (simulated, not your real network): %s\n", fdesc)
	}
	rep, err := cl.Once(ctx, st)
	if err != nil {
		fmt.Fprintf(stderr, "sync with %s failed: %v\n", *peer, err)
		if rep.Pulled+rep.Pushed > 0 {
			fmt.Fprintf(stderr, "(before it stopped: pulled %d, pushed %d; those are kept)\n", rep.Pulled, rep.Pushed)
		}
		return 1
	}
	hash, _ := st.StateHash(ctx)
	fmt.Fprintf(stdout, "synced with %s: pulled %d, pushed %d (%d requests, %s)\n", rep.Peer, rep.Pulled, rep.Pushed, rep.Requests, rep.Duration.Round(time.Millisecond))
	if rep.Rebuilt {
		fmt.Fprintln(stdout, "an older edit arrived: the task view was rebuilt in timestamp order")
	}
	fmt.Fprintf(stdout, "state hash: %s\n", hash)
	return 0
}

func cmdSyncSummary(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sync summary", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", "", dbHelp)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx := context.Background()
	st, err := openSyncStore(ctx, *db, 0)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	defer st.Close()
	sum, err := st.Summary(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	hash, _ := st.StateHash(ctx)
	fmt.Fprintf(stdout, "node:        %s\n", st.NodeID())
	nodes := make([]string, 0, len(sum))
	for n := range sum {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	for _, n := range nodes {
		fmt.Fprintf(stdout, "holds:       %s up to op %d\n", n, sum[n])
	}
	if len(nodes) == 0 {
		fmt.Fprintln(stdout, "holds:       nothing yet")
	}
	fmt.Fprintf(stdout, "state hash:  %s\n", hash)
	return 0
}

// cmdSyncRun keeps a replica in step with a peer: one round every -every. A
// round that cannot reach the peer is reported and tried again; nothing is lost
// meanwhile, because the ops are in the log and the next round sends whatever
// is missing.
func cmdSyncRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sync run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", "", dbHelp)
	peer := fs.String("peer", "", "base URL of the other replica")
	keyFile := fs.String("key-file", "", "file holding the shared key (default: $OFFGRID_SYNC_KEY)")
	batch := fs.Int("batch", syncer.DefaultBatch, "ops per request")
	maxDrift := fs.Duration("max-drift", 0, "refuse ops stamped further ahead than this (default 1h)")
	every := fs.Duration("every", 2*time.Second, "time between rounds")
	roundTimeout := fs.Duration("round-timeout", 10*time.Second, "give up on one round after this long")
	forDur := fs.Duration("for", 0, "stop after this long (default: until Ctrl-C)")
	name := fs.String("name", "", "label for each line (default: this replica's node ID)")
	faults := addFaultFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *peer == "" {
		fmt.Fprintln(stderr, "error: -peer URL is required")
		return 2
	}
	key, err := syncer.LoadKey(*keyFile)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *forDur > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *forDur)
		defer cancel()
	}
	st, err := openSyncStore(ctx, *db, *maxDrift)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	defer st.Close()
	label := *name
	if label == "" {
		label = st.NodeID()
	}
	cl := &syncer.Client{Peer: *peer, Key: key, Node: st.NodeID(), Batch: *batch}
	ftr, fdesc, ferr := faults.build()
	if ferr != nil {
		fmt.Fprintln(stderr, "error:", ferr)
		return 2
	}
	if ftr != nil {
		cl.HTTP = &http.Client{Transport: ftr}
		fmt.Fprintf(stdout, "FAULT INJECTION ON (simulated, not your real network): %s\n", fdesc)
	}
	fmt.Fprintf(stdout, "%s: syncing with %s every %s\n", label, *peer, *every)

	line := func(format string, a ...any) {
		fmt.Fprintf(stdout, "%s  %s: %s\n", time.Now().Format("15:04:05"), label, fmt.Sprintf(format, a...))
	}
	last := ""
	tick := time.NewTicker(*every)
	defer tick.Stop()
	for {
		rctx, cancel := context.WithTimeout(ctx, *roundTimeout)
		rep, err := cl.Once(rctx, st)
		cancel()
		hash, _ := st.StateHash(ctx)
		short := hash
		if len(short) > 12 {
			short = short[:12]
		}
		var msg string
		switch {
		case ctx.Err() != nil:
			line("stopped")
			return 0
		case err != nil && errors.Is(err, netfault.ErrPartitioned):
			msg = "OFFLINE (network off); edits are kept and will be sent when it is back. state " + short
		case err != nil:
			msg = fmt.Sprintf("sync failed: %v. state %s", err, short)
		case rep.Pulled+rep.Pushed > 0:
			msg = fmt.Sprintf("SYNCED: pulled %d, pushed %d. state %s", rep.Pulled, rep.Pushed, short)
		default:
			msg = "in step. state " + short
		}
		// Say it when something changed; stay quiet while nothing does.
		if msg != last {
			line("%s", msg)
			last = msg
		}
		select {
		case <-ctx.Done():
			line("stopped")
			return 0
		case <-tick.C:
		}
	}
}
