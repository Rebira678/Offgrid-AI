package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"offgrid/internal/sim"
)

// Seams for the tests: a real store never fails the sweep, so the failure
// report is exercised with stand-ins.
var (
	simMany   = sim.Many
	simShrink = sim.Shrink
)

// cmdSim is the deterministic simulation test (Day 24).
//
//	offgrid sim [-seeds N] [-from S] [-j W]        run N schedules, each a pure function of its seed
//	offgrid sim -seed S                            replay one seed and print its whole trace
//	offgrid sim -replay FILE                       replay a (shrunk) action list
//
// It exits 1 if any schedule breaks a promise, and prints the failing seed,
// what broke, and a shrunk schedule.
func cmdSim(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sim", flag.ContinueOnError)
	fs.SetOutput(stderr)
	seeds := fs.Int("seeds", 1000, "how many seeds to run")
	from := fs.Int64("from", 1, "first seed of the sweep")
	one := fs.Int64("seed", 0, "replay this one seed and print its trace")
	replay := fs.String("replay", "", "replay the actions in FILE (as printed by a failing sweep) and print the trace")
	steps := fs.Int("steps", 80, "actions per schedule")
	replicas := fs.Int("replicas", 3, "replicas per schedule")
	workers := fs.Int("j", runtime.NumCPU(), "schedules run at once (the result for a seed does not depend on this)")
	shared := fs.Bool("shared", false, "start the replicas from one common task instead of from nothing")
	shrink := fs.Bool("shrink", true, "cut the first failing schedule down to the few actions that matter")
	dir := fs.String("dir", "", "where the replicas' database files go (default: /dev/shm if it exists, else the temp dir)")
	keep := fs.Bool("keep", false, "keep the database files of a failing run")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "error: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *seeds < 1 || *steps < 1 || *replicas < 2 || *workers < 1 {
		fmt.Fprintln(stderr, "error: -seeds and -steps must be at least 1, -replicas at least 2, -j at least 1")
		return 2
	}
	cfg := sim.Config{Replicas: *replicas, Steps: *steps, Dir: *dir, Keep: *keep, Shared: *shared}

	if *replay != "" {
		acts, err := readActions(*replay)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 2
		}
		seed := *one
		if seed == 0 {
			seed = 1
		}
		return printRun(stdout, sim.RunActions(seed, acts, cfg))
	}
	if *one != 0 {
		return printRun(stdout, sim.Run(*one, cfg))
	}

	fmt.Fprintf(stdout, "offgrid sim: %d schedules (seeds %d..%d), %d replicas, %d actions each, %d at a time\n",
		*seeds, *from, *from+int64(*seeds)-1, *replicas, *steps, *workers)
	sum := simMany(*from, *seeds, *workers, cfg)
	t := sum.Total
	fmt.Fprintf(stdout, "thrown: %d edits, %d syncs (%d dropped, %d duplicated, %d delayed, %d cut short, %d reversed, %d with a hole in them), %d partitions (%d syncs blocked),\n",
		t.Edits, t.Syncs, t.Dropped, t.Duplicated, t.Delayed, t.Truncated, t.Reversed, t.Gapped, t.Partitions, t.Blocked)
	fmt.Fprintf(stdout, "        %d clock skews (%d syncs refused as too far ahead), %d crashes and restarts, %d refolds of a view\n", t.Skews, t.DriftRefusals, t.Crashes, t.Rebuilds)
	fmt.Fprintf(stdout, "checked: every new edit sorts after what its replica had seen; a refused batch changes nothing; at the end the same ops and the same state everywhere,\n")
	fmt.Fprintf(stdout, "         each view == a replay of its own log, rebuild changes nothing, a fresh replica fed the ops in another order agrees,\n")
	fmt.Fprintf(stdout, "         and delete-vs-edit and suggestion outcomes match a recomputation from the raw log\n")
	if len(sum.Failed) == 0 {
		fmt.Fprintf(stdout, "PASS: %d of %d schedules converged (%.1fs, %d ops converged in total)\n", sum.Runs, sum.Runs, sum.Elapsed.Seconds(), sum.Ops)
		return 0
	}
	fmt.Fprintf(stdout, "FAIL: %d of %d schedules broke a promise (%.1fs). Failing seeds:", len(sum.Failed), sum.Runs, sum.Elapsed.Seconds())
	for i, f := range sum.Failed {
		if i == 12 {
			fmt.Fprintf(stdout, " ... (%d more)", len(sum.Failed)-12)
			break
		}
		fmt.Fprintf(stdout, " %d", f.Seed)
	}
	fmt.Fprintln(stdout)
	byClass := map[string]int{}
	for _, f := range sum.Failed {
		byClass[f.Failure.Class]++
	}
	fmt.Fprintf(stdout, "by kind: %v\n", byClass)
	first := sum.Failed[0]
	fmt.Fprintf(stdout, "\nfirst failure: seed %d, %v\n", first.Seed, first.Failure)
	fmt.Fprintf(stdout, "replay it exactly: offgrid sim -seed %d", first.Seed)
	if *shared {
		fmt.Fprint(stdout, " -shared")
	}
	fmt.Fprintln(stdout)
	if *shrink {
		small := simShrink(first, cfg)
		fmt.Fprintf(stdout, "\nshrunk from %d to %d actions, still failing as %q. Save these lines to a file and run `offgrid sim -replay FILE`:\n\n", len(first.Actions), len(small.Actions), small.Failure.Class)
		for _, a := range small.Actions {
			fmt.Fprintln(stdout, a)
		}
		fmt.Fprintf(stdout, "\ntrace of the shrunk schedule:\n%s\n=> %v\n", strings.Join(small.Trace, "\n"), small.Failure)
	}
	return 1
}

func printRun(w io.Writer, r sim.Result) int {
	for _, l := range r.Trace {
		fmt.Fprintln(w, l)
	}
	if r.Failure != nil {
		fmt.Fprintf(w, "\nFAIL (seed %d): %v\n", r.Seed, r.Failure)
		return 1
	}
	fmt.Fprintf(w, "\nPASS (seed %d): every replica ended with %d ops and state %s\n", r.Seed, r.Ops, r.Hash[:16])
	return 0
}

func readActions(path string) ([]sim.Action, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []sim.Action
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		a, err := sim.ParseAction(line)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, sc.Err()
}
