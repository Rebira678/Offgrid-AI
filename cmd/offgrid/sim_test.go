package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"offgrid/internal/sim"
)

func TestSimPassesAndReportsWhatItThrew(t *testing.T) {
	code, out, errOut := runCLI(t, "", "sim", "-seeds", "6", "-j", "2")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{"6 schedules (seeds 1..6)", "thrown:", "checked:", "PASS: 6 of 6 schedules converged"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
}

func TestSimReplaysOneSeedExactly(t *testing.T) {
	_, a, _ := runCLI(t, "", "sim", "-seed", "4", "-steps", "30")
	code, b, _ := runCLI(t, "", "sim", "-seed", "4", "-steps", "30")
	if code != 0 || a != b || !strings.Contains(a, "PASS (seed 4)") || !strings.Contains(a, "030  ") {
		t.Fatalf("two runs of one seed differ or failed (exit %d):\n%s\n---\n%s", code, a, b)
	}
	// the sweep's result for a seed does not depend on the worker count
	_, one, _ := runCLI(t, "", "sim", "-seeds", "4", "-from", "9", "-j", "1")
	_, two, _ := runCLI(t, "", "sim", "-seeds", "4", "-from", "9", "-j", "3")
	pick := func(s string) string { return s[strings.Index(s, "thrown:"):strings.Index(s, "checked:")] }
	if pick(one) != pick(two) {
		t.Fatalf("1 and 3 workers disagree:\n%s\n%s", pick(one), pick(two))
	}
}

func TestSimReplaysAnActionFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "acts.txt")
	os.WriteFile(f, []byte("# a tiny schedule\ncreate r0 n=5\ncreate r1 n=9\nsync r1-r0 n=3\n\ncrash r2 n=3\n"), 0o644)
	code, out, errOut := runCLI(t, "", "sim", "-replay", f)
	if code != 0 || !strings.Contains(out, "PASS") || !strings.Contains(out, "r0 create") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	os.WriteFile(f, []byte("dance r0 n=1\n"), 0o644)
	if code, _, errOut := runCLI(t, "", "sim", "-replay", f); code != 2 || !strings.Contains(errOut, "cannot read action") {
		t.Fatalf("exit %d %s", code, errOut)
	}
	if code, _, _ := runCLI(t, "", "sim", "-replay", filepath.Join(t.TempDir(), "missing")); code != 2 {
		t.Fatalf("missing file: exit %d", code)
	}
}

func TestSimRefusesBadArguments(t *testing.T) {
	for _, args := range [][]string{{"-seeds", "0"}, {"-steps", "0"}, {"-replicas", "1"}, {"-j", "0"}, {"extra"}, {"-nope"}} {
		if code, _, _ := runCLI(t, "", append([]string{"sim"}, args...)...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

// A real store never fails the sweep, so the failure report is checked with
// stand-ins for the sweep and the shrinker.
func TestSimReportsAFailingSeedWithAShrunkScheduleAndExitsOne(t *testing.T) {
	oldMany, oldShrink := simMany, simShrink
	defer func() { simMany, simShrink = oldMany, oldShrink }()
	fail := sim.Result{Seed: 77, Actions: make([]sim.Action, 9), Failure: &sim.Failure{Class: "diverged", Msg: "r0 vs r1"}}
	simMany = func(from int64, n, workers int, cfg sim.Config) sim.Summary {
		return sim.Summary{From: from, To: from + int64(n) - 1, Runs: n, Failed: []sim.Result{fail}}
	}
	simShrink = func(r sim.Result, cfg sim.Config) sim.Result {
		r.Actions = []sim.Action{{Kind: sim.Create, A: 1, N: 4}, {Kind: sim.Sync, A: 0, B: 1, N: 2}}
		r.Trace = []string{"001  r1 create"}
		return r
	}
	code, out, _ := runCLI(t, "", "sim", "-seeds", "10", "-shared")
	for _, want := range []string{"FAIL: 1 of 10", "77", "by kind: map[diverged:1]", "offgrid sim -seed 77 -shared", "shrunk from 9 to 2 actions", "create r1 n=4", "sync r0-r1 n=2", "001  r1 create"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if code, out, _ := runCLI(t, "", "sim", "-seeds", "10", "-shrink=false"); code != 1 || strings.Contains(out, "shrunk from") {
		t.Fatalf("-shrink=false: exit %d\n%s", code, out)
	}
}

// The schedule the simulator shrank its first real failure to. On the Day 23
// code it ends in "two replicas created different tasks with the same ID".
func TestTheShrunkScheduleOfTheDay24BugNowPasses(t *testing.T) {
	code, out, errOut := runCLI(t, "", "sim", "-replay", "../../examples/day24-id-collision.txt")
	if code != 0 || !strings.Contains(out, "PASS") || !strings.Contains(out, "r2 create") || !strings.Contains(out, "T1.r2") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
}
