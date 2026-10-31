package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"offgrid/internal/jobqueue"
	"offgrid/internal/store/sqlstore"
	"offgrid/internal/web"
)

const webHelp = `offgrid web serves a dashboard in your browser. The page is inside this
program: no other files, no internet, works with the WiFi off.

  offgrid web -db FILE [-queue FILE] [-metrics DIR] [-listen 127.0.0.1:8787] [-read-only]

It shows the replicas this log has heard from and what arrived from them, the
connection state, the queue, which tier handled each message, your tasks, and
the AI suggestions waiting for you, with Accept and Dismiss. Accepting a
suggestion the task has changed under is refused, exactly as on the command
line. Everything else on the page is read-only.

It listens on this machine only (127.0.0.1, ::1 or localhost) and refuses any
other address: it has no login and no encryption. To use another device, sync
to it (offgrid sync) and run the dashboard there. -read-only removes even
Accept and Dismiss.

Start "offgrid drain" or "offgrid agent -route" with the same queue to fill in
the connection and tier panels.`

func cmdWeb(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("web", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, webHelp); fs.PrintDefaults() }
	dbPath := fs.String("db", os.Getenv("OFFGRID_DB"), dbHelp)
	queueFlag := fs.String("queue", "", "the queue file (default: $OFFGRID_QUEUE, or queue.db next to $OFFGRID_DB)")
	metrics := fs.String("metrics", "", "directory the processes publish to (default: metrics/ next to the queue file)")
	listen := fs.String("listen", "127.0.0.1:8787", "address to listen on (this machine only)")
	readOnly := fs.Bool("read-only", false, "do not allow Accept or Dismiss")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "error: unexpected argument %q (web takes only flags)\n", fs.Arg(0))
		return 2
	}
	if *dbPath == "" {
		fmt.Fprintln(stderr, "error: the dashboard shows the task database: use -db FILE or set OFFGRID_DB")
		return 2
	}
	if err := web.RequireLoopback(*listen); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	if _, err := os.Stat(*dbPath); err != nil {
		fmt.Fprintf(stderr, "error: no database at %s (create one with: offgrid exec -seed -db %s '{\"intent\":\"list_tasks\"}')\n", *dbPath, *dbPath)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := sqlstore.Open(ctx, *dbPath, sqlstore.Options{NodeID: nodeFromEnv()})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	defer st.Close()

	cfg := web.Config{Store: st, ReadOnly: *readOnly}
	qpath := queuePath(*queueFlag)
	if qpath == "" && *dbPath != "" {
		candidate := filepath.Join(filepath.Dir(*dbPath), "queue.db")
		if _, err := os.Stat(candidate); err == nil {
			qpath = candidate
		}
	}
	if qpath != "" {
		reader := &queueReader{path: qpath}
		defer reader.close()
		var mu sync.Mutex // two browser tabs poll at once; the reader is not safe for that
		cfg.Queue = func(ctx context.Context) (jobqueue.Stats, error) {
			mu.Lock()
			defer mu.Unlock()
			return reader.stats(ctx)
		}
		cfg.MetricsDir = filepath.Join(filepath.Dir(qpath), "metrics")
	}
	if *metrics != "" {
		cfg.MetricsDir = *metrics
	} else if cfg.MetricsDir == "" && *dbPath != "" {
		candidate := filepath.Join(filepath.Dir(*dbPath), "metrics")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			cfg.MetricsDir = candidate
		}
	}
	err = web.Serve(ctx, *listen, cfg, func(a net.Addr) {
		fmt.Fprintf(stdout, "offgrid web: node %s, dashboard on http://%s  (Ctrl-C to stop)\n", st.NodeID(), a)
	})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	return 0
}
