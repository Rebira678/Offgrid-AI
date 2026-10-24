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

	"offgrid/internal/store/sqlstore"
	"offgrid/internal/syncer"
)

const syncHelp = `offgrid sync exchanges tasks with another replica over HTTP.

  offgrid sync serve   -db FILE [-addr 127.0.0.1:7878] [-key-file FILE] [-window 5m] [-max-drift 1h]
  offgrid sync once    -db FILE -peer URL [-key-file FILE] [-batch N] [-max-drift 1h]
  offgrid sync summary -db FILE

Both replicas need the same key: -key-file FILE, or $OFFGRID_SYNC_KEY
(at least 16 bytes; make one with: openssl rand -hex 32).

Requests and responses are signed with HMAC-SHA256, so a stranger can neither
read your tasks through the server nor feed it ops. They are NOT encrypted:
use a network you trust, or put TLS in front.`

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
	case "summary":
		return cmdSyncSummary(args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "error: unknown sync command %q (serve, once or summary)\n", args[0])
	return 2
}

func openSyncStore(ctx context.Context, db string, maxDrift time.Duration) (*sqlstore.Store, error) {
	if db == "" {
		db = os.Getenv("OFFGRID_DB")
	}
	if db == "" {
		return nil, errors.New("-db FILE is required (or set $OFFGRID_DB)")
	}
	return sqlstore.Open(ctx, db, sqlstore.Options{MaxDrift: maxDrift})
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
