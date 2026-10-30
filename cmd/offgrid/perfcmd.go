package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"offgrid/internal/perf"
)

// README markers: `offgrid perf -readme README.md` and `offgrid bench -readme
// README.md` rewrite only what lies between a pair, so the rest of the README
// is never touched.
const (
	perfStart  = "<!-- perf:start -->"
	perfEnd    = "<!-- perf:end -->"
	benchStart = "<!-- bench:start -->"
	benchEnd   = "<!-- bench:end -->"
)

// cmdPerf measures what Offgrid costs on this machine, with no model: store
// speed, sync time, queue drain time, memory. `make bench` runs it.
//
//	offgrid perf [-quick] [-repeats N] [-sizes 100,1000,10000] [-md FILE] [-json FILE] [-svg FILE] [-readme FILE]
func cmdPerf(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("perf", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "where the database files go (default: a temp directory, removed afterwards). Use a directory on the disk you care about")
	repeats := fs.Int("repeats", 3, "runs per figure; the median is reported, with min and max")
	sizes := fs.String("sizes", "100,1000,10000", "log sizes (ops) for the store and sync measurements")
	jobs := fs.String("jobs", "100,1000", "queue sizes (jobs)")
	workers := fs.String("workers", "1,4", "drain concurrency levels")
	seed := fs.Int64("seed", 1, "fixes every generated title and edit")
	quick := fs.Bool("quick", false, "a fast pass: sizes 100,1000, 100 jobs, 1 run each (numbers are noisier)")
	mdPath := fs.String("md", "", "also write the Markdown report to this file")
	jsonPath := fs.String("json", "", "also write the results as JSON to this file")
	svgPath := fs.String("svg", "", "also draw the results as a 1200x675 SVG picture (for a post) to this file")
	readme := fs.String("readme", "", "rewrite the block between "+perfStart+" and "+perfEnd+" in this file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "error: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	cfg := perf.Config{Dir: *dir, Repeats: *repeats, Seed: *seed}
	var err error
	if *quick {
		cfg.Repeats, cfg.Sizes, cfg.Jobs, cfg.Workers = 1, []int{100, 1000}, []int{100}, []int{4}
	} else {
		if cfg.Sizes, err = intList(*sizes); err == nil {
			if cfg.Jobs, err = intList(*jobs); err == nil {
				cfg.Workers, err = intList(*workers)
			}
		}
	}
	if err != nil || cfg.Repeats < 1 {
		if err == nil {
			err = fmt.Errorf("-repeats must be at least 1")
		}
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	cfg.Progress = func(format string, a ...any) { fmt.Fprintf(stderr, format+"\n", a...) }

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintln(stderr, "Tip: close other programs and plug in the charger for honest numbers. Ctrl-C cancels.")
	rep, err := perf.Run(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	var md bytes.Buffer
	if err := perf.WriteMarkdown(&md, rep); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	fmt.Fprintln(stderr)
	stdout.Write(md.Bytes())
	if *mdPath != "" {
		if d := filepath.Dir(*mdPath); d != "" && d != "." {
			os.MkdirAll(d, 0o755)
		}
		if err := os.WriteFile(*mdPath, md.Bytes(), 0o644); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
	}
	if *jsonPath != "" {
		if err := writeFile(*jsonPath, func(w io.Writer) error { return perf.WriteJSON(w, rep) }); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
	}
	if *svgPath != "" {
		if err := writeFile(*svgPath, func(w io.Writer) error { return perf.WriteSVG(w, rep) }); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
	}
	if *readme != "" {
		if err := updateBlock(*readme, perfStart, perfEnd, md.String()); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		fmt.Fprintf(stderr, "updated %s\n", *readme)
	}
	return 0
}

// updateBlock rewrites the block between two marker lines in path, through a
// temporary file, so an error never leaves a half-written README.
func updateBlock(path, start, end, body string) error {
	old, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	out, err := perf.ReplaceBlock(string(old), start, end, body)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	perm := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		perm = st.Mode().Perm()
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(out), perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func intList(s string) ([]int, error) {
	var out []int
	for _, p := range splitList(s) {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("%q is not a positive whole number", p)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("an empty list of sizes")
	}
	return out, nil
}
