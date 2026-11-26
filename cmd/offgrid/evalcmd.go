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
	host := fs.String("host", defaultHost(), "model server: an Ollama address, or llamacpp@URL / openai@URL / ollama@URL (default: $OLLAMA_HOST or http://127.0.0.1:11434)")
	modelList := fs.String("models", "llama3.2:1b,llama3.2:3b", "comma-separated models to evaluate")
	attempts := fs.Int("attempts", structured.DefaultMaxAttempts, "most times the model may try each message, the first included")
	schema := fs.Bool("schema", true, "constrain replies to the command schema")
	ablation := fs.Bool("ablation", false, "measure three setups per model: prompt only, + schema, + schema and retries")
	limit := fs.Int("limit", 0, "run only the first N cases (0 = all); handy for a quick check")
	caseTimeout := fs.Duration("case-timeout", 2*time.Minute, "give up on a single case after this long")
	mdPath := fs.String("md", "", "also write the Markdown report to this file")
	jsonPath := fs.String("json", "", "also write the full results as JSON to this file")
	baselinePath := fs.String("baseline", "", "compare with this baseline file; exit 4 if a rate fell by more than -margin (needs exactly one model)")
	margin := fs.Float64("margin", 0.05, "how far a rate may fall below the baseline, as a fraction (0.05 = 5 points); real models are noisy, the CI reference uses 0.02")
	writeBaseline := fs.String("write-baseline", "", "write this run as a baseline file (needs exactly one model and every setup the baseline should hold)")
	recordPath := fs.String("record", "", "append one row per run to this CSV (the nightly history)")
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
	if (*baselinePath != "" || *writeBaseline != "") && len(models) != 1 {
		fmt.Fprintln(stderr, "error: -baseline and -write-baseline need exactly one model in -models")
		return 2
	}
	if *margin < 0 || *margin > 1 {
		fmt.Fprintln(stderr, "error: -margin is a fraction between 0 and 1")
		return 2
	}
	prov, err := llm.ParseProvider(providerSpec(*host), ollama.DefaultBaseURL)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	ver := prov.Kind + " server"
	if prov.Kind == llm.KindOllama { // only Ollama can say its version and which models it holds
		oc := ollama.NewClient(prov.URL)
		if ver, err = oc.Version(ctx); err != nil {
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

	client := chatClient(*host)
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
		if *recordPath != "" {
			if err := eval.AppendHistory(*recordPath, meta, sums); err != nil {
				fmt.Fprintf(stderr, "error: cannot record the run: %v\n", err)
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
	if *writeBaseline != "" {
		b := eval.BaselineOf(models[0], meta.Date, sums)
		if err := writeFile(*writeBaseline, b.Write); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		fmt.Fprintf(stderr, "baseline written to %s: commit it, and say why the numbers are accepted.\n", *writeBaseline)
	}
	if *baselinePath != "" {
		return gateAgainst(*baselinePath, sums, *margin, stderr)
	}
	return 0
}

// gateAgainst compares a finished run with a committed baseline: exit 0 holds, 4 regressed, 1 cannot say.
func gateAgainst(path string, sums []eval.Summary, margin float64, stderr io.Writer) int {
	base, err := eval.ReadBaseline(path)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	regs, notes, err := base.Compare(sums, margin)
	for _, n := range notes {
		fmt.Fprintln(stderr, "note:", n)
	}
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if len(regs) > 0 {
		for _, r := range regs {
			fmt.Fprintln(stderr, "REGRESSION:", r)
		}
		fmt.Fprintf(stderr, "%d rate(s) fell more than %.0f points below %s (model %s, measured %s).\n", len(regs), 100*margin, path, base.Model, base.Date)
		return 4
	}
	fmt.Fprintf(stderr, "holds: no rate fell more than %.0f points below %s.\n", 100*margin, path)
	return 0
}
