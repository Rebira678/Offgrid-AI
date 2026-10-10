package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
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
	"offgrid/internal/structured"
	"offgrid/internal/task"
	"offgrid/internal/tools"
)

// cmdAgent runs worker messages through the tiered loop: rules first, the model
// only for what the rules cannot read. Each message is printed with the tier
// that handled it and how long it took.
//
// Tasks live in memory for the length of one run (SQLite arrives on Day 6), so
// a run is a session: later messages see the effects of earlier ones.
func cmdAgent(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	host := fs.String("host", defaultHost(), "Ollama server URL (default: $OLLAMA_HOST or http://127.0.0.1:11434)")
	model := fs.String("model", envOr("OFFGRID_MODEL", defaultModel), "model for Tier 1 (default: $OFFGRID_MODEL or "+defaultModel+")")
	noModel := fs.Bool("no-model", false, "rules only: never call a model (shows what Tier 0 alone can do)")
	attempts := fs.Int("attempts", structured.DefaultMaxAttempts, "most times the model may try per message")
	timeout := fs.Duration("timeout", 2*time.Minute, "give up on one message after this long")
	seed := fs.Bool("seed", false, "start with three tasks created yesterday")
	nowFlag := fs.String("now", "", "pretend it is this moment, e.g. 2026-10-08T09:00 (default: the real time)")
	logPath := fs.String("log", "", "append one JSON line per message (tier, latency, outcome) to this file")
	runEval := fs.Bool("eval", false, "run the 30 labelled evaluation messages through the loop (implies -seed)")
	limit := fs.Int("limit", 0, "with -eval: run only the first N cases")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *attempts < 1 {
		fmt.Fprintln(stderr, "error: -attempts must be at least 1")
		return 2
	}
	now := time.Now()
	if *nowFlag != "" {
		t, err := parseNow(*nowFlag)
		if err != nil {
			fmt.Fprintf(stderr, "error: -now: %v\n", err)
			return 2
		}
		now = t
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Domain clock under our control, so seeding "yesterday" is exact.
	current := now
	store := task.NewMemStore(func() time.Time { return current })
	reg := tools.NewRegistry(store, func() time.Time { return current })
	if *seed || *runEval {
		current = now.AddDate(0, 0, -1)
		if err := seedTasks(ctx, reg); err != nil {
			fmt.Fprintf(stderr, "error: seeding: %v\n", err)
			return 1
		}
		current = now
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
	for _, m := range messages {
		if ctx.Err() != nil {
			break
		}
		o := handle(m)
		outs = append(outs, o)
		printOutcome(stdout, o)
	}
	if len(outs) > 1 {
		fmt.Fprintln(stdout)
		agent.WriteStats(stdout, outs)
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
