package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"offgrid/internal/eval"
	"offgrid/internal/llm"
	"offgrid/internal/ollama"
	"offgrid/internal/structured"
	"offgrid/internal/sysinfo"
)

// setup is one combination of techniques to measure.
type setup struct {
	schema   bool
	attempts int
}

// evalSetups returns what to measure. With ablation, each technique is added
// one at a time (prompt alone, then the schema, then retries on top), so the
// report shows what each one is actually worth instead of only the total.
func evalSetups(schema bool, attempts int, ablation bool) []setup {
	if !ablation {
		return []setup{{schema, attempts}}
	}
	candidates := []setup{{false, 1}, {true, 1}, {true, attempts}}
	var out []setup
	for _, s := range candidates {
		if len(out) == 0 || out[len(out)-1] != s {
			out = append(out, s)
		}
	}
	return out
}

// cmdEval measures how reliably each model turns worker messages into valid,
// correct commands.
func cmdEval(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	fs.SetOutput(stderr)
	host := fs.String("host", defaultHost(), "Ollama server URL (default: $OLLAMA_HOST or http://127.0.0.1:11434)")
	modelList := fs.String("models", "llama3.2:1b,llama3.2:3b", "comma-separated models to evaluate")
	attempts := fs.Int("attempts", structured.DefaultMaxAttempts, "most times the model may try each message, the first included")
	schema := fs.Bool("schema", true, "constrain replies to the command schema")
	ablation := fs.Bool("ablation", false, "measure three setups per model: prompt only, + schema, + schema and retries")
	limit := fs.Int("limit", 0, "run only the first N cases (0 = all); handy for a quick check")
	caseTimeout := fs.Duration("case-timeout", 2*time.Minute, "give up on a single case after this long")
	mdPath := fs.String("md", "", "also write the Markdown report to this file")
	jsonPath := fs.String("json", "", "also write the full results as JSON to this file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	models := splitList(*modelList)
	if len(models) == 0 {
		fmt.Fprintln(stderr, "error: -models is empty")
		return 2
	}
	if *attempts < 1 {
		fmt.Fprintln(stderr, "error: -attempts must be at least 1")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	oc := ollama.NewClient(normalizeHost(*host))

	ver, err := oc.Version(ctx)
	if err != nil {
		printError(stderr, err)
		return 1
	}
	installed, err := oc.ListModels(ctx)
	if err != nil {
		printError(stderr, err)
		return 1
	}
	if missing := missingModels(installed, models); len(missing) > 0 {
		fmt.Fprintln(stderr, "error: these models are not installed yet:")
		for _, m := range missing {
			fmt.Fprintf(stderr, "  ollama pull %s\n", m)
		}
		return 1
	}

	cases, err := eval.LoadCases()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	setups := evalSetups(*schema, *attempts, *ablation)
	n := len(cases)
	if *limit > 0 && *limit < n {
		n = *limit
	}
	fmt.Fprintf(stderr, "Evaluating %d model(s) x %d setup(s) x %d case(s). Ctrl-C stops and keeps finished runs.\n", len(models), len(setups), n)
	fmt.Fprint(stderr, "Tip: close your browser and plug in the charger; nothing else should compete for the CPU.\n\n")

	client := llm.NewOllama(oc)
	var sums []eval.Summary
	var runErr error
loop:
	for _, model := range models {
		for i, s := range setups {
			sum, err := eval.Run(ctx, eval.Config{
				Client:      client,
				Model:       model,
				UseSchema:   s.schema,
				MaxAttempts: s.attempts,
				Limit:       *limit,
				CaseTimeout: *caseTimeout,
				Warmup:      i == 0, // the model is already loaded for its later setups
				Progress:    func(format string, a ...any) { fmt.Fprintf(stderr, format+"\n", a...) },
			}, cases)
			if err != nil {
				runErr = err
				break loop
			}
			sums = append(sums, sum)
		}
	}

	meta := eval.Meta{Date: time.Now().Format("2006-01-02"), OllamaVersion: ver, Hardware: sysinfo.Detect()}
	if len(sums) > 0 {
		fmt.Fprintln(stderr)
		if err := eval.WriteMarkdown(stdout, meta, sums); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		if *mdPath != "" {
			if err := writeFile(*mdPath, func(w io.Writer) error { return eval.WriteMarkdown(w, meta, sums) }); err != nil {
				fmt.Fprintf(stderr, "error: %v\n", err)
				return 1
			}
		}
		if *jsonPath != "" {
			if err := writeFile(*jsonPath, func(w io.Writer) error { return eval.WriteJSON(w, meta, sums) }); err != nil {
				fmt.Fprintf(stderr, "error: %v\n", err)
				return 1
			}
		}
	}
	if runErr != nil {
		if len(sums) > 0 {
			fmt.Fprintln(stderr, "\nstopped early; the table above has only the runs that finished.")
		}
		if errors.Is(runErr, context.Canceled) {
			return 130
		}
		return reportAskError(stderr, runErr, "", *caseTimeout)
	}
	return 0
}
