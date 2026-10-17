package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"offgrid/internal/agent"
	"offgrid/internal/eval"
	"offgrid/internal/intent"
	"offgrid/internal/llm"
	"offgrid/internal/ollama"
	"offgrid/internal/router"
	"offgrid/internal/structured"
	"offgrid/internal/tools"
)

// cmdAgent runs worker messages through the tiered loop: rules first, the model
// only for what the rules cannot read. Each message is printed with the tier
// that handled it and how long it took.
//
// Without -db, tasks live in memory for the length of one run, so a run is a
// session: later messages see the effects of earlier ones. With -db they are
// kept in a SQLite file and survive between runs.
func cmdAgent(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	host := fs.String("host", defaultHost(), "Ollama server URL (default: $OLLAMA_HOST or http://127.0.0.1:11434)")
	model := fs.String("model", envOr("OFFGRID_MODEL", defaultModel), "model for Tier 1 (default: $OFFGRID_MODEL or "+defaultModel+")")
	noModel := fs.Bool("no-model", false, "rules only: never call a model (shows what Tier 0 alone can do)")
	attempts := fs.Int("attempts", structured.DefaultMaxAttempts, "most times the model may try per message")
	timeout := fs.Duration("timeout", 2*time.Minute, "give up on one message after this long")
	seed := fs.Bool("seed", false, "start with three tasks created yesterday")
	dbPath := fs.String("db", os.Getenv("OFFGRID_DB"), dbHelp)
	nowFlag := fs.String("now", "", "pretend it is this moment, e.g. 2026-10-08T09:00 (default: the real time)")
	logPath := fs.String("log", "", "append one JSON line per message (tier, latency, outcome) to this file")
	runEval := fs.Bool("eval", false, "run the 30 labelled evaluation messages through the loop (implies -seed)")
	limit := fs.Int("limit", 0, "with -eval: run only the first N cases")
	route := fs.Bool("route", false, "decide per message: answer now, or save hard ones in the durable queue (needs a queue file)")
	queueFlag := fs.String("queue", "", "with -route: the queue file (default: $OFFGRID_QUEUE, or queue.db next to $OFFGRID_DB)")
	netMode := fs.String("net", "auto", "with -route: connectivity: auto (probe the network), online or offline (force it)")
	probeURLs := fs.String("url", defaultProbeURLs, "with -route -net auto: comma-separated probe URLs that answer 204")
	faults := addFaultFlags(fs)
	routeEval := fs.Bool("route-eval", false, "measure the complexity gate on labelled messages (no model, queue or network needed) and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *attempts < 1 {
		fmt.Fprintln(stderr, "error: -attempts must be at least 1")
		return 2
	}
	if *routeEval {
		router.WriteGateReport(stdout)
		return 0
	}
	now := time.Now()
	var fixed *time.Time
	if *nowFlag != "" {
		t, err := parseNow(*nowFlag)
		if err != nil {
			fmt.Fprintf(stderr, "error: -now: %v\n", err)
			return 2
		}
		now, fixed = t, &t
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	clk := newSessionClock(fixed)
	store, closeStore, err := openTaskStore(ctx, *dbPath, clk)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	defer closeStore()
	reg := tools.NewRegistry(store, clk.Now)
	if *seed || *runEval {
		clk.override = now.AddDate(0, 0, -1)
		err := seedTasks(ctx, reg)
		clk.override = time.Time{}
		if err != nil {
			fmt.Fprintf(stderr, "error: seeding: %v\n", err)
			return 1
		}
	}

	a := &agent.Agent{Rules: agent.DefaultRules(), Registry: reg}
	if !*noModel {
		a.Model = timeoutModel{timeout: *timeout, inner: intent.Classifier{
			Client:      llm.NewOllama(ollama.NewClient(normalizeHost(*host))),
			Model:       *model,
			UseSchema:   true,
			MaxAttempts: *attempts,
		}}
	}

	var logFile *os.File
	if *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		defer f.Close()
		logFile = f
	}
	record := func(o agent.Outcome) {
		if logFile != nil {
			if err := agent.WriteLog(logFile, time.Now(), o); err != nil {
				fmt.Fprintf(stderr, "warning: could not write the log: %v\n", err)
			}
		}
	}
	handle := func(text string) agent.Outcome {
		o := a.Handle(ctx, text)
		record(o)
		return o
	}

	if *runEval {
		cases, err := eval.LoadCases()
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		if *limit > 0 && *limit < len(cases) {
			cases = cases[:*limit]
		}
		results := agent.RunCases(ctx, a, cases, record)
		agent.WriteCases(stdout, results)
		fmt.Fprintln(stdout)
		var outs []agent.Outcome
		for _, r := range results {
			outs = append(outs, r.Outcome)
		}
		agent.WriteStats(stdout, outs)
		if ctx.Err() != nil {
			fmt.Fprintln(stderr, "\ncancelled: results above are partial")
			return 130
		}
		return 0
	}

	var faultClient *http.Client
	if faults.active() {
		if !*route || *netMode != "auto" {
			fmt.Fprintln(stderr, "error: -fault-* flags break the network probes, so they need -route with -net auto (forced -net offline/online never touches the network)")
			return 2
		}
		ftr, fdesc, ferr := faults.build()
		if ferr != nil {
			fmt.Fprintf(stderr, "error: %v\n", ferr)
			return 2
		}
		faultClient = probeClient(ftr)
		fmt.Fprintf(stdout, "FAULT INJECTION ON (simulated, not your real network): %s\n", fdesc)
		defer printFaultStats(stdout, ftr)
	}

	var rt *router.Router
	if *route {
		var closeRoute func()
		var netLabel string
		rt, closeRoute, netLabel, err = openRouting(ctx, a, *queueFlag, *netMode, *probeURLs, faultClient, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 2
		}
		defer closeRoute()
		fmt.Fprintf(stdout, "routing on. connectivity: %s\n\n", netLabel)
	}

	// Messages: the arguments, or one per line from standard input.
	var messages []string
	if rest := strings.TrimSpace(strings.Join(fs.Args(), " ")); rest != "" {
		messages = []string{rest}
	} else {
		sc := bufio.NewScanner(stdin)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" && !strings.HasPrefix(line, "#") {
				messages = append(messages, line)
			}
		}
		if err := sc.Err(); err != nil {
			fmt.Fprintf(stderr, "error: reading standard input: %v\n", err)
			return 1
		}
	}
	if len(messages) == 0 {
		fmt.Fprintln(stderr, `error: no message given. Try: offgrid agent -seed "list open tasks"`)
		return 2
	}

	var outs []agent.Outcome
	routes := map[router.Route]int{}
	for _, m := range messages {
		if ctx.Err() != nil {
			break
		}
		if rt != nil {
			res := rt.Handle(ctx, m)
			routes[res.Route]++
			if res.Outcome.Text != "" {
				record(res.Outcome)
			}
			// The tier table counts only what the local agent really
			// answered; a message it could not read and the queue took over
			// is not a "failed" answer.
			if res.Route == router.RouteLocal {
				outs = append(outs, res.Outcome)
			}
			printRouted(stdout, res)
			continue
		}
		o := handle(m)
		outs = append(outs, o)
		printOutcome(stdout, o)
	}
	if len(outs) > 1 {
		fmt.Fprintln(stdout)
		agent.WriteStats(stdout, outs)
	}
	if rt != nil && len(messages) > 1 {
		fmt.Fprintf(stdout, "\nROUTES  answered now: %d   deferred to the queue: %d   queue questions: %d\n",
			routes[router.RouteLocal]+routes[router.RouteCloud], routes[router.RouteDeferred], routes[router.RouteStatus])
		if n := routes[router.RouteFailed]; n > 0 {
			fmt.Fprintf(stdout, "        could not be saved: %d\n", n)
		}
		fmt.Fprintln(stdout, rt.Handle(ctx, "queue").Message)
	}
	if ctx.Err() != nil {
		return 130
	}
	return 0
}

// timeoutModel gives every Tier 1 call its own deadline, so one slow message
// cannot hold up the rest.
type timeoutModel struct {
	inner   agent.Model
	timeout time.Duration
}

func (m timeoutModel) Classify(ctx context.Context, text string) (structured.Result[intent.Command], error) {
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	return m.inner.Classify(ctx, text)
}

func printOutcome(w io.Writer, o agent.Outcome) {
	fmt.Fprintf(w, "> %s\n", o.Text)
	label := o.Tier.String()
	switch o.Tier {
	case agent.TierRules:
		label += ", rule " + o.Rule
	case agent.TierModel:
		label += fmt.Sprintf(", %d attempt(s)", o.Attempts)
	}
	fmt.Fprintf(w, "  [%s, %s] %s: %s\n", label, agent.FormatDuration(o.Total), o.Status, strings.ReplaceAll(o.Message(), "\n", "\n  "))
	if o.Fallback != "" {
		fmt.Fprintf(w, "  (note: %s)\n", o.Fallback)
	}
}

// printRouted shows one routed message: where it went, how long the decision
// took, and the status a person needs to read.
func printRouted(w io.Writer, r router.Result) {
	switch r.Route {
	case router.RouteLocal:
		printOutcome(w, r.Outcome) // the same line as without routing: tier, latency, outcome
	case router.RouteDeferred:
		fmt.Fprintf(w, "> %s\n  [deferred to queue, %s] %s\n", r.Text, agent.FormatDuration(r.Total), r.Message)
	case router.RouteCloud:
		fmt.Fprintf(w, "> %s\n  [cloud model, %s] %s\n", r.Text, agent.FormatDuration(r.Total), r.Message)
	case router.RouteStatus:
		fmt.Fprintf(w, "> %s\n  [queue status, %s] %s\n", r.Text, agent.FormatDuration(r.Total), strings.ReplaceAll(r.Message, "\n", "\n  "))
	default:
		fmt.Fprintf(w, "> %s\n  [%s, %s] %s\n", r.Text, r.Route, agent.FormatDuration(r.Total), r.Message)
	}
}
