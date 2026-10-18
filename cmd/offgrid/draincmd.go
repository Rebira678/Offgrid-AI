package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"offgrid/internal/agent"
	"offgrid/internal/connectivity"
	qd "offgrid/internal/drain"
	"offgrid/internal/intent"
	"offgrid/internal/jobqueue"
	"offgrid/internal/llm"
	"offgrid/internal/ollama"
	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
	"offgrid/internal/tools"
)

// Seams for tests: the real thing talks to Ollama; tests swap in fakes.
var (
	newChatClient = func(host string) llm.Client { return llm.NewOllama(ollama.NewClient(normalizeHost(host))) }
	newClassifier = func(host, model string, attempts int) agent.Model {
		return intent.Classifier{Client: newChatClient(host), Model: model, UseSchema: true, MaxAttempts: attempts}
	}
)

const drainHelp = `offgrid drain empties the job queue (jobs that "agent -route" saved for later).

  offgrid drain -db FILE [-queue FILE] [flags]

Run it and leave it: it waits for the network, drains the queue in order
(-concurrency jobs at once), and keeps going until Ctrl-C. With -once it
stops as soon as nothing more can run. Read a finished job's answer with:
  offgrid queue result ID

Jobs that need only this laptop (interpret) run even when offline. Jobs that
need another machine (deep_think with -deep-host) wait for the network.`

func cmdDrain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("drain", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, drainHelp); fs.PrintDefaults() }
	dbPath := fs.String("db", os.Getenv("OFFGRID_DB"), dbHelp)
	queueFlag := fs.String("queue", "", "the queue file (default: $OFFGRID_QUEUE, or queue.db next to $OFFGRID_DB)")
	netMode := fs.String("net", "auto", "connectivity: auto (probe the network), online or offline (force it)")
	probeURLs := fs.String("url", defaultProbeURLs, "with -net auto: comma-separated probe URLs that answer 204")
	host := fs.String("host", defaultHost(), "Ollama server on this laptop, for interpret jobs (default: $OLLAMA_HOST or http://127.0.0.1:11434)")
	model := fs.String("model", envOr("OFFGRID_MODEL", defaultModel), "model for interpret jobs")
	attempts := fs.Int("attempts", 3, "interpret: model tries per message")
	timeout := fs.Duration("timeout", 2*time.Minute, "interpret: give up on one model call after this long")
	deepHost := fs.String("deep-host", "", "Ollama server for deep_think jobs on ANOTHER machine; such jobs wait for the network (default: the local server, which needs no network)")
	deepModel := fs.String("deep-model", "", "model for deep_think jobs (default: -model)")
	noModel := fs.Bool("no-model", false, "run no model jobs at all (then there is nothing to drain)")
	concurrency := fs.Int("concurrency", 2, "jobs running at the same time")
	lease := fs.Duration("lease", 2*time.Minute, "how long a job may go without a heartbeat before another worker may take it")
	jobTimeout := fs.Duration("job-timeout", 10*time.Minute, "one attempt may not run longer than this")
	probeEvery := fs.Duration("probe-interval", 0, "with -net auto: time between network probes (default 2s)")
	poll := fs.Duration("poll", time.Second, "how often to look for work and check the network")
	once := fs.Bool("once", false, "stop when nothing more can run now (default: keep waiting for work and for the network)")
	forDur := fs.Duration("for", 0, "stop after this long (default: until Ctrl-C)")
	faults := addFaultFlags(fs)
	oo := addObsFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "error: unexpected argument %q (drain takes only flags)\n", fs.Arg(0))
		return 2
	}
	if *dbPath == "" {
		fmt.Fprintln(stderr, "error: drain needs the task database, because the jobs read and change tasks: use -db FILE or set OFFGRID_DB")
		return 2
	}
	qpath := queuePath(*queueFlag)
	if qpath == "" {
		fmt.Fprintln(stderr, "error: no queue file. Use -queue FILE (or set OFFGRID_QUEUE, or OFFGRID_DB)")
		return 2
	}
	if *noModel {
		fmt.Fprintln(stderr, "error: -no-model leaves nothing to run: every queued job kind needs a model")
		return 2
	}
	if *concurrency < 1 || *concurrency > 64 {
		fmt.Fprintln(stderr, "error: -concurrency must be 1 to 64")
		return 2
	}
	if _, err := os.Stat(qpath); err != nil {
		fmt.Fprintf(stderr, "error: no queue at %s: nothing has been saved yet (try: offgrid agent -route ...)\n", qpath)
		return 1
	}

	var faultClient *http.Client
	if faults.active() {
		if *netMode != "auto" {
			fmt.Fprintln(stderr, "error: -fault-* flags break the network probes, so they need -net auto (forced -net offline/online never touches the network)")
			return 2
		}
		ftr, fdesc, err := faults.build()
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 2
		}
		faultClient = probeClient(ftr)
		fmt.Fprintf(stdout, "FAULT INJECTION ON (simulated, not your real network): %s\n", fdesc)
		defer printFaultStats(stdout, ftr)
	}

	ob, closeLog, err := oo.observer(stderr)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	defer closeLog()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *forDur > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *forDur)
		defer cancel()
	}

	clk := newSessionClock(nil)
	store, closeStore, err := openTaskStore(ctx, *dbPath, clk)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	defer closeStore()
	reg := tools.NewRegistry(store, clk.Now)

	if err := os.MkdirAll(filepath.Dir(qpath), 0o755); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	qdb, err := sqlstore.OpenDB(qpath)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	defer qdb.Close()
	q, err := jobqueue.New(ctx, qdb, jobqueue.Config{})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	// What this worker can run.
	a := &agent.Agent{Rules: agent.DefaultRules(), Registry: reg,
		Model: timeoutModel{timeout: *timeout, inner: newClassifier(*host, *model, *attempts)}}
	dm := *deepModel
	if dm == "" {
		dm = *model
	}
	remote := *deepHost != ""
	dh := *host
	if remote {
		dh = *deepHost
	}
	specs := map[string]qd.Spec{
		qd.KindInterpret: qd.Interpret(a),
		qd.KindDeepThink: qd.DeepThink(qd.DeepThinkConfig{
			Client: newChatClient(dh), Model: dm, NeedsNetwork: remote,
			Context: func(ctx context.Context) (string, error) { return taskContext(ctx, store) },
		}),
	}

	state, stopNet, label, err := startConnectivity(ctx, *netMode, *probeURLs, faultClient, *probeEvery, ob)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	defer stopNet()

	stopPub := oo.publish(ctx, ob, "drain", qpath, q.Stats, stderr)
	defer stopPub()

	say := func(format string, a ...any) { fmt.Fprintf(stdout, format, a...) }
	say("draining %s\n", qpath)
	say("connectivity: %s\n", label)
	say("concurrency %d, lease %s\n", *concurrency, *lease)
	say("  interpret   reads messages with the local model %s\n", *model)
	if remote {
		say("  deep_think  asks model %s on %s (needs the network: waits while offline)\n", dm, *deepHost)
	} else {
		say("  deep_think  asks the local model %s (runs even when offline)\n", dm)
	}
	say("\n")

	w, err := qd.New(qd.Config{
		Queue: q, Specs: specs, Concurrency: *concurrency, Lease: *lease, Poll: *poll, JobTimeout: *jobTimeout,
		Online:       func() bool { return state() == connectivity.Online },
		ExitWhenIdle: *once,
		OnEvent:      func(e qd.Event) { printDrainEvent(stdout, e); ob.Drain(e) },
	})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	w.Run(ctx)

	s := w.Summary()
	say("\n%s: %d done, %d will retry, %d dead, %d given back, %d lost to another worker\n",
		map[bool]string{true: "drain finished", false: "drain stopped"}[*once && ctx.Err() == nil],
		s.Done, s.Retried, s.Dead, s.Released, s.LostClaims)
	if st, err := q.Stats(context.WithoutCancel(ctx)); err == nil {
		say("queue now: %d queued, %d running, %d done, %d dead\n", st.Queued, st.Running, st.Done, st.Dead)
		if st.Queued > 0 && *once {
			say("(%d still queued: waiting for the network, backing off after a failure, or of a kind this worker does not run)\n", st.Queued)
		}
	}
	return 0
}

func printDrainEvent(w io.Writer, e qd.Event) {
	at := e.At.Format("15:04:05")
	job := fmt.Sprintf("#%d %s", e.Job.ID, e.Job.Kind)
	oneLine := func(s string, n int) string {
		s = strings.Join(strings.Fields(s), " ")
		if len(s) > n {
			s = s[:n-3] + "..."
		}
		return s
	}
	switch e.Kind {
	case qd.EvClaimed:
		fmt.Fprintf(w, "%s  %-18s started (try %d of %d)\n", at, job, e.Job.Attempts, e.Job.MaxAttempts)
	case qd.EvDone:
		fmt.Fprintf(w, "%s  %-18s DONE: %s\n", at, job, oneLine(e.Note, 100))
	case qd.EvRetry:
		fmt.Fprintf(w, "%s  %-18s failed, will retry in %s: %s\n", at, job, e.Delay.Round(time.Millisecond), oneLine(e.Err.Error(), 100))
	case qd.EvDead:
		fmt.Fprintf(w, "%s  %-18s DEAD: %s\n", at, job, oneLine(e.Err.Error(), 160))
	case qd.EvReleased:
		fmt.Fprintf(w, "%s  %-18s given back: %s\n", at, job, e.Note)
	case qd.EvLostClaim:
		fmt.Fprintf(w, "%s  %-18s %s\n", at, job, e.Note)
	case qd.EvGate:
		fmt.Fprintf(w, "%s  network: %s\n", at, e.Note)
	case qd.EvError:
		fmt.Fprintf(w, "%s  %-18s queue problem: %v\n", at, job, e.Err)
	}
}

// taskContext is the data a deep_think answer is based on: every task, with
// its latest notes. Without it a model asked to "summarize this week's
// incidents" can only invent them.
func taskContext(ctx context.Context, s task.Store) (string, error) {
	ts, err := s.List(ctx, task.Filter{})
	if err != nil {
		return "", err
	}
	if len(ts) == 0 {
		return "(there are no tasks yet)", nil
	}
	var b strings.Builder
	for _, t := range ts {
		fmt.Fprintf(&b, "%s [%s, %s] site %s: %s (created %s)\n", t.ID, t.Priority, t.Status, t.Site, t.Title, t.CreatedAt.Format("2006-01-02"))
		from := max(len(t.Notes)-3, 0)
		for _, n := range t.Notes[from:] {
			fmt.Fprintf(&b, "    note %s: %s\n", n.At.Format("2006-01-02"), n.Text)
		}
		if b.Len() > 6000 {
			b.WriteString("(more tasks not shown)\n")
			break
		}
	}
	return b.String(), nil
}
