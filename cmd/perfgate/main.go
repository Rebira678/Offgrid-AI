// Command perfgate is the performance gate (Day 45).
//
//	perfgate -new new.txt [-old old.txt] [-budgets perf/budgets.json] [-override "reason in words"] [-md report.md]
//
// It reads `go test -bench` output (scripts/perf/bench.sh makes it), compares the new run with
// the old one, checks the new run against budgets for the hardware it ran on, prints a report,
// and exits 1 if something regressed or is over budget and no override was given.
//
// Exit codes: 0 fine (or overridden), 1 regression or budget exceeded, 2 bad input.
//
// The override is the way past a gate that is wrong or a trade-off that is worth it. It needs a
// reason in words, and the reason is printed in the report. CI takes it from a "Perf-Override:"
// line in the pull request description; PERF_OVERRIDE in the environment does the same locally.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"offgrid/internal/perfgate"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv)) }

func run(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("perfgate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	oldPath := fs.String("old", "", "baseline `go test -bench` output (omit to check budgets only)")
	newPath := fs.String("new", "", "new `go test -bench` output")
	budgetPath := fs.String("budgets", "", "budgets JSON for named hardware (optional)")
	override := fs.String("override", getenv("PERF_OVERRIDE"), "let a failing gate pass, with a reason in words (default: $PERF_OVERRIDE)")
	mdPath := fs.String("md", "", "also write the report here as Markdown")
	pol := perfgate.DefaultPolicy()
	fs.Float64Var(&pol.Alpha, "alpha", pol.Alpha, "significance level")
	fs.Float64Var(&pol.MaxTime, "max-time", pol.MaxTime, "allowed slowdown (fraction)")
	fs.Float64Var(&pol.MaxBytes, "max-bytes", pol.MaxBytes, "allowed B/op growth (fraction)")
	fs.Float64Var(&pol.MaxAllocs, "max-allocs", pol.MaxAllocs, "allowed allocs/op growth (fraction)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *newPath == "" {
		fmt.Fprintln(stderr, "perfgate: -new is required")
		return 2
	}
	reason, err := perfgate.Override(*override)
	if err != nil {
		fmt.Fprintln(stderr, "perfgate:", err)
		return 2
	}
	cur, err := load(*newPath)
	if err != nil {
		fmt.Fprintln(stderr, "perfgate:", err)
		return 2
	}

	var b strings.Builder
	failed := false

	if *oldPath != "" {
		old, err := load(*oldPath)
		if err != nil {
			fmt.Fprintln(stderr, "perfgate:", err)
			return 2
		}
		if old.CPU != cur.CPU {
			fmt.Fprintf(&b, "> **Different hardware.** old: %q, new: %q. Numbers from different machines are not comparable; nothing is gated.\n\n", old.CPU, cur.CPU)
		} else {
			rows, added, gone := perfgate.Compare(old, cur, pol)
			failed = writeComparison(&b, rows, added, gone) || failed
		}
	} else {
		b.WriteString("No baseline given: only budgets are checked.\n\n")
	}

	if *budgetPath != "" {
		f, err := os.Open(*budgetPath)
		if err != nil {
			fmt.Fprintln(stderr, "perfgate:", err)
			return 2
		}
		budgets, err := perfgate.ParseBudgets(f)
		f.Close()
		if err != nil {
			fmt.Fprintln(stderr, "perfgate:", err)
			return 2
		}
		viol, enforced := budgets.Check(cur)
		switch {
		case !enforced:
			fmt.Fprintf(&b, "Budgets are for %q; this run was on %q, so they are **not enforced** here.\n\n", budgets.Hardware, cur.CPU)
		case len(viol) == 0:
			fmt.Fprintf(&b, "All %d budgets hold on %q.\n\n", len(budgets.Budgets), budgets.Hardware)
		default:
			failed = true
			fmt.Fprintf(&b, "### Over budget on %q\n\n| benchmark | metric | budget | got | why the budget exists |\n|---|---|---|---|---|\n", budgets.Hardware)
			for _, v := range viol {
				got := perfgate.Format(v.Budget.Metric, v.Got)
				if v.Missing {
					got = "**not in this run**"
				}
				fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", v.Budget.Bench, v.Budget.Metric, perfgate.Format(v.Budget.Metric, v.Budget.Max), got, v.Budget.Why)
			}
			b.WriteString("\n")
		}
	}

	code := 0
	switch {
	case failed && reason != "":
		fmt.Fprintf(&b, "**OVERRIDDEN:** %s\n", reason)
	case failed:
		b.WriteString("**FAILED.** Fix it, or say why it is worth it with `Perf-Override: <reason in words>` in the pull request description.\n")
		code = 1
	case reason != "":
		fmt.Fprintf(&b, "(An override was given (%q) but nothing failed, so it was not needed.)\n", reason)
	default:
		b.WriteString("**PASSED.**\n")
	}
	fmt.Fprint(stdout, b.String())
	if *mdPath != "" {
		if err := os.WriteFile(*mdPath, []byte(b.String()), 0o644); err != nil {
			fmt.Fprintln(stderr, "perfgate:", err)
			return 2
		}
	}
	return code
}

func load(path string) (*perfgate.Run, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r, err := perfgate.Parse(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}

// writeComparison prints every regression and improvement, and a one-line count of the rest.
// It returns true if anything regressed.
func writeComparison(b *strings.Builder, rows []perfgate.Row, added, gone []string) bool {
	var reg, imp []perfgate.Row
	same := 0
	for _, r := range rows {
		switch r.Verdict {
		case perfgate.Regression:
			reg = append(reg, r)
		case perfgate.Improvement:
			imp = append(imp, r)
		default:
			same++
		}
	}
	sort.SliceStable(reg, func(i, j int) bool { return reg[i].Delta > reg[j].Delta })
	sort.SliceStable(imp, func(i, j int) bool { return imp[i].Delta < imp[j].Delta })
	table := func(title string, rs []perfgate.Row) {
		if len(rs) == 0 {
			return
		}
		fmt.Fprintf(b, "### %s\n\n| benchmark | metric | old | new | change | p |\n|---|---|---|---|---|---|\n", title)
		for _, r := range rs {
			fmt.Fprintf(b, "| %s | %s | %s | %s | %+.1f%% | %.3f |\n", r.Name, r.Metric, perfgate.Format(r.Metric, r.Old), perfgate.Format(r.Metric, r.New), r.Delta*100, r.P)
		}
		b.WriteString("\n")
	}
	table("Regressions", reg)
	table("Improvements", imp)
	fmt.Fprintf(b, "%d metrics unchanged or within noise.", same)
	if len(added) > 0 {
		fmt.Fprintf(b, " New benchmarks (no baseline yet): %s.", strings.Join(added, ", "))
	}
	if len(gone) > 0 {
		fmt.Fprintf(b, " **Missing from this run:** %s.", strings.Join(gone, ", "))
	}
	b.WriteString("\n\n")
	return len(reg) > 0
}
