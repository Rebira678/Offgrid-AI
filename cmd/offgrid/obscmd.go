package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"offgrid/internal/dash"
	"offgrid/internal/jobqueue"
	"offgrid/internal/obs"
	"offgrid/internal/store/sqlstore"
)

// obsOpts are the flags `drain` and `agent -route` share for seeing what they
// do: structured logs, and the metrics snapshot that `offgrid top` reads.
type obsOpts struct {
	slogPath   *string
	slogFormat *string
	slogLevel  *string
	metrics    *string
}

func addObsFlags(fs *flag.FlagSet) *obsOpts {
	return &obsOpts{
		slogPath:   fs.String("slog", "", "write structured logs (log/slog) to this file, or - for standard error (default: no structured logs)"),
		slogFormat: fs.String("slog-format", "text", "structured log format: text (key=value) or json"),
		slogLevel:  fs.String("slog-level", "info", "structured log level: debug (every probe), info, warn or error"),
		metrics:    fs.String("metrics", "", "directory where this process publishes its counters for `offgrid top` (default: metrics/ next to the queue file; off to disable)"),
	}
}

// observer builds the Observer. The returned func closes the log file.
func (o *obsOpts) observer(stderr io.Writer) (*obs.Observer, func(), error) {
	ob := &obs.Observer{M: obs.NewMetrics()}
	closeLog := func() {}
	switch p := *o.slogPath; p {
	case "":
		// Validate the other flags even when logging is off, so a typo is
		// reported now rather than the day someone turns logging on.
		if _, err := obs.NewLogger(io.Discard, *o.slogFormat, *o.slogLevel); err != nil {
			return nil, nil, err
		}
	default:
		var w io.Writer = stderr
		if p != "-" {
			f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				return nil, nil, fmt.Errorf("-slog: %w", err)
			}
			w, closeLog = f, func() { f.Close() }
		}
		l, err := obs.NewLogger(w, *o.slogFormat, *o.slogLevel)
		if err != nil {
			closeLog()
			return nil, nil, err
		}
		ob.Log = l
	}
	return ob, closeLog, nil
}

// metricsDir says where snapshots go: the -metrics flag, else next to the
// queue file. "off" gives "".
func (o *obsOpts) metricsDir(queuePath string) string {
	switch d := *o.metrics; d {
	case "off":
		return ""
	case "":
		if queuePath == "" {
			return ""
		}
		return filepath.Join(filepath.Dir(queuePath), "metrics")
	default:
		return d
	}
}

// publish starts writing this process's snapshot once a second and returns a
// func that stops it (after one last snapshot marked "stopped").
func (o *obsOpts) publish(ctx context.Context, ob *obs.Observer, proc, queuePath string, stats func(context.Context) (jobqueue.Stats, error), stderr io.Writer) func() {
	dir := o.metricsDir(queuePath)
	if dir == "" {
		return func() {}
	}
	pctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	var once sync.Once
	warn := func(err error) {
		once.Do(func() {
			fmt.Fprintf(stderr, "warning: cannot publish metrics to %s: %v (the work goes on; `offgrid top` will show this process as silent)\n", dir, err)
		})
	}
	go func() { defer close(done); obs.Publish(pctx, ob.M, dir, proc, time.Second, stats, warn) }()
	return func() { cancel(); <-done }
}

const topHelp = `offgrid top shows the whole system on one screen: connection state, the queue,
and which tier handled each message. It reads what "drain" and
"agent -route" publish, and reads the queue itself. It changes nothing.

  offgrid top -queue FILE [-metrics DIR] [-interval 1s] [-once]

Start it in a second terminal while "offgrid drain" runs. A process that has
stopped publishing is shown as silent, and a connection state it last
reported is labelled "last known": the screen never presents old news as live.

Ctrl-C quits. With -once it prints one frame and exits (also what you get when
the output is not a terminal, so it can go in a file or a bug report).`

func cmdTop(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("top", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, topHelp); fs.PrintDefaults() }
	queueFlag := fs.String("queue", "", "the queue file (default: $OFFGRID_QUEUE, or queue.db next to $OFFGRID_DB)")
	metrics := fs.String("metrics", "", "directory the processes publish to (default: metrics/ next to the queue file)")
	interval := fs.Duration("interval", time.Second, "time between repaints")
	once := fs.Bool("once", false, "print one frame and exit")
	colorFlag := fs.String("color", "auto", "colours: auto, always or never (auto: on for a terminal, off if NO_COLOR is set)")
	width := fs.Int("width", 0, "screen width in columns (default 80)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "error: unexpected argument %q (top takes only flags)\n", fs.Arg(0))
		return 2
	}
	if *interval < 100*time.Millisecond {
		fmt.Fprintln(stderr, "error: -interval must be at least 100ms")
		return 2
	}
	if *width != 0 && *width < 64 {
		fmt.Fprintln(stderr, "error: -width must be at least 64")
		return 2
	}
	qpath := queuePath(*queueFlag)
	dir := *metrics
	if dir == "" && qpath != "" {
		dir = filepath.Join(filepath.Dir(qpath), "metrics")
	}
	if qpath == "" && dir == "" {
		fmt.Fprintln(stderr, "error: no queue file and no metrics directory. Use -queue FILE (or set OFFGRID_QUEUE, or OFFGRID_DB), or -metrics DIR")
		return 2
	}

	tty := false
	if f, ok := stdout.(*os.File); ok {
		if st, err := f.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
			tty = true
		}
	}
	color, err := useColor(*colorFlag, tty, os.Getenv("NO_COLOR"))
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The queue is opened read-only, lazily and again if it vanishes, so the
	// screen can be started before anything has been queued.
	reader := &queueReader{path: qpath}
	defer reader.close()

	next := func() (dash.View, dash.Options) {
		v := dash.View{Now: time.Now(), QueuePath: qpath}
		if dir != "" {
			v.Snaps, v.Problems = obs.ReadSnapshots(dir)
		}
		if qpath == "" {
			v.QueueErr = "no queue file given (use -queue)"
		} else if st, err := reader.stats(ctx); err != nil {
			v.QueueErr = err.Error()
		} else {
			v.Queue = &st
		}
		return v, dash.Options{Width: *width, Color: color}
	}

	if *once {
		v, o := next()
		fmt.Fprintln(stdout, dash.Render(v, o))
		return 0
	}
	dash.Live(ctx, stdout, tty, *interval, next)
	return 0
}

// useColor decides whether to draw colours. auto means: on for a terminal,
// unless NO_COLOR is set (https://no-color.org).
func useColor(mode string, tty bool, noColor string) (bool, error) {
	switch mode {
	case "auto":
		return tty && noColor == "", nil
	case "always":
		return true, nil
	case "never":
		return false, nil
	}
	return false, fmt.Errorf("-color must be auto, always or never (got %q)", mode)
}

// queueReader reads queue depth without ever writing to the queue file.
type queueReader struct {
	path string
	db   interface{ Close() error }
	q    *jobqueue.Queue
}

func (r *queueReader) stats(ctx context.Context) (jobqueue.Stats, error) {
	if r.q == nil {
		db, err := sqlstore.OpenReadOnly(r.path)
		if err != nil {
			return jobqueue.Stats{}, fmt.Errorf("no queue yet at %s", r.path)
		}
		q, err := jobqueue.NewReadOnly(ctx, db)
		if err != nil {
			db.Close()
			return jobqueue.Stats{}, err
		}
		r.db, r.q = db, q
	}
	s, err := r.q.Stats(ctx)
	if err != nil { // reopen next frame: the file may have been replaced
		r.close()
		return jobqueue.Stats{}, err
	}
	return s, nil
}

func (r *queueReader) close() {
	if r.db != nil {
		r.db.Close()
	}
	r.db, r.q = nil, nil
}
