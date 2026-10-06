// Command offgrid is the Offgrid command-line tool.
//
// Day 1 commands:
//
//	offgrid models   list the models installed in Ollama
//	offgrid bench    measure tokens/sec on this machine
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"offgrid/internal/bench"
	"offgrid/internal/ollama"
	"offgrid/internal/sysinfo"
)

const version = "0.1.0-day1"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main with its inputs and outputs passed in, so tests can call it
// directly. os.Exit lives only in main(): calling it deeper would skip
// deferred cleanup and make the code impossible to test.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "models":
		return cmdModels(args[1:], stdout, stderr)
	case "bench":
		return cmdBench(args[1:], stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, "offgrid", version)
		return 0
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `offgrid: an AI agent runtime that keeps working when the internet doesn't.

Usage:
  offgrid models [-host URL]
  offgrid bench  [-host URL] [-models a,b] [-runs N] [-quick] [-md FILE] [-json FILE]
  offgrid version

Run "offgrid bench -h" for the benchmark options.
`)
}

// ---- models -----------------------------------------------------------

func cmdModels(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("models", flag.ContinueOnError)
	fs.SetOutput(stderr)
	host := fs.String("host", defaultHost(), "Ollama server URL (default: $OLLAMA_HOST or http://127.0.0.1:11434)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client := ollama.NewClient(normalizeHost(*host))

	ver, err := client.Version(ctx)
	if err != nil {
		printError(stderr, err)
		return 1
	}
	models, err := client.ListModels(ctx)
	if err != nil {
		printError(stderr, err)
		return 1
	}

	fmt.Fprintf(stdout, "Ollama %s at %s\n\n", ver, client.BaseURL)
	if len(models) == 0 {
		fmt.Fprintln(stdout, "No models installed yet. Try: ollama pull llama3.2:3b")
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tPARAMS\tQUANT\tSIZE")
	for _, m := range models {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%.1f GB\n", m.Name, m.ParameterSize, m.Quantization, float64(m.SizeBytes)/1e9)
	}
	tw.Flush()
	return 0
}

// ---- bench ------------------------------------------------------------

func cmdBench(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	fs.SetOutput(stderr)
	host := fs.String("host", defaultHost(), "Ollama server URL (default: $OLLAMA_HOST or http://127.0.0.1:11434)")
	modelList := fs.String("models", "llama3.2:1b,llama3.2:3b", "comma-separated models to benchmark")
	runs := fs.Int("runs", 3, "timed runs per model and prompt (the median is reported)")
	timeout := fs.Duration("timeout", 5*time.Minute, "time limit for a single request")
	quick := fs.Bool("quick", false, "short prompt only (faster, skips the prompt-processing test)")
	mdPath := fs.String("md", "", "also write the Markdown table to this file")
	jsonPath := fs.String("json", "", "also write the results as JSON to this file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	models := splitList(*modelList)
	if len(models) == 0 {
		fmt.Fprintln(stderr, "error: -models is empty")
		return 2
	}
	if *runs < 1 {
		fmt.Fprintln(stderr, "error: -runs must be at least 1")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client := ollama.NewClient(normalizeHost(*host))

	ver, err := client.Version(ctx)
	if err != nil {
		printError(stderr, err)
		return 1
	}
	installed, err := client.ListModels(ctx)
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

	hw := sysinfo.Detect()
	fmt.Fprintf(stderr, "Machine: %s, %d threads, %.1f GiB RAM\n", hw.CPU, hw.Threads, hw.MemGiB)
	fmt.Fprintf(stderr, "Tip: close your browser and plug in the charger for honest numbers. Ctrl-C cancels.\n\n")

	rows, runErr := bench.Run(ctx, client, bench.Config{
		Models:     models,
		Prompts:    bench.DefaultPrompts(*quick),
		Runs:       *runs,
		RunTimeout: *timeout,
		Salt:       fmt.Sprint(time.Now().UnixNano()),
		Progress: func(format string, a ...any) {
			fmt.Fprintf(stderr, format+"\n", a...)
		},
	})

	meta := bench.Meta{
		Date:          time.Now().Format("2006-01-02"),
		OllamaVersion: ver,
		Hardware:      hw,
		Runs:          *runs,
	}
	if len(rows) > 0 {
		fmt.Fprintln(stderr)
		if err := bench.WriteMarkdown(stdout, meta, rows); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		if *mdPath != "" {
			if err := writeFile(*mdPath, func(w io.Writer) error { return bench.WriteMarkdown(w, meta, rows) }); err != nil {
				fmt.Fprintf(stderr, "error: %v\n", err)
				return 1
			}
		}
		if *jsonPath != "" {
			if err := writeFile(*jsonPath, func(w io.Writer) error { return bench.WriteJSON(w, meta, rows) }); err != nil {
				fmt.Fprintf(stderr, "error: %v\n", err)
				return 1
			}
		}
	}
	if runErr != nil {
		fmt.Fprintf(stderr, "\nstopped early: %v (partial results above)\n", runErr)
		return 1
	}
	for _, r := range rows {
		if r.Error != "" {
			return 1 // the table already lists which model failed and why
		}
	}
	return 0
}

// ---- helpers ----------------------------------------------------------

func defaultHost() string {
	if h := os.Getenv("OLLAMA_HOST"); h != "" {
		return h
	}
	return ollama.DefaultBaseURL
}

// normalizeHost accepts "127.0.0.1:11434" (the form OLLAMA_HOST usually uses)
// as well as a full URL.
func normalizeHost(h string) string {
	h = strings.TrimSpace(h)
	if h == "" {
		return ollama.DefaultBaseURL
	}
	if !strings.Contains(h, "://") {
		h = "http://" + h
	}
	return strings.TrimRight(h, "/")
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// canonicalModel makes "llama3.2" and "llama3.2:latest" compare equal.
func canonicalModel(name string) string {
	if !strings.Contains(name, ":") {
		return name + ":latest"
	}
	return name
}

func missingModels(installed []ollama.Model, wanted []string) []string {
	have := make(map[string]bool, len(installed))
	for _, m := range installed {
		have[canonicalModel(m.Name)] = true
	}
	var missing []string
	for _, w := range wanted {
		if !have[canonicalModel(w)] {
			missing = append(missing, w)
		}
	}
	return missing
}

func writeFile(path string, write func(io.Writer) error) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	return write(f)
}

// printError turns the client's typed errors into advice a person can act on.
func printError(w io.Writer, err error) {
	var apiErr *ollama.APIError
	switch {
	case errors.Is(err, ollama.ErrUnreachable):
		fmt.Fprintf(w, "error: %v\n\nIs Ollama running? Start it with: ollama serve\n(or check: systemctl status ollama)\n", err)
	case errors.As(err, &apiErr) && apiErr.Status == 404:
		fmt.Fprintf(w, "error: %v\n\nThe model may not be installed. See what is: offgrid models\n", err)
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(w, "cancelled")
	default:
		fmt.Fprintf(w, "error: %v\n", err)
	}
}
