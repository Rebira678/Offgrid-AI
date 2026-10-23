package main

import (
	"strings"
	"testing"
)

func TestORSetDemoShowsTheDeleteHoldingTheEditSurvivingAndOutOfOrderArrival(t *testing.T) {
	code, out, errOut := runCLI(t, "", "orset", "demo")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if strings.Contains(out, "THEY DIFFER") {
		t.Fatalf("replicas diverged:\n%s", out)
	}
	if n := strings.Count(out, "both ended identical"); n != 2 {
		t.Errorf("%d agreements, want 2:\n%s", n, out)
	}
	parts := strings.SplitN(out, "\n2. ", 2)
	if len(parts) != 2 {
		t.Fatalf("no section 2:\n%s", out)
	}
	s1, rest := parts[0], parts[1]
	s2 := strings.SplitN(rest, "\n3. ", 2)[0]
	if linesEndingWith(s1, "after merging:", "T1") != 0 || strings.Count(s1, "(empty)") != 2 {
		t.Errorf("section 1: the delete must hold on both sides:\n%s", s1)
	}
	if linesEndingWith(s2, "after merging:", "T1") != 2 {
		t.Errorf("section 2: the concurrent edit must keep T1 on both sides:\n%s", s2)
	}
	if !strings.Contains(out, "after the add arrives: (empty)") {
		t.Errorf("section 3: an early tombstone must still remove the add:\n%s", out)
	}
	if linesEndingWith(out, "Beka after re-adding:", "T1") != 1 {
		t.Errorf("section 4: a deliberate re-add must bring T1 back:\n%s", out)
	}
	if !strings.Contains(out, "cost: 1 tombstone kept") {
		t.Errorf("section 4 must show that the delete left a tombstone behind:\n%s", out)
	}
	if !strings.Contains(out, "she can see 1 tag, so she writes 1 tombstone") {
		t.Errorf("section 1 must show the remove writing a tombstone for what it saw:\n%s", out)
	}
	if !strings.Contains(out, "SIMULATION") {
		t.Error("the demo must say it is a simulation")
	}
}

func TestORSetCommandErrors(t *testing.T) {
	for _, args := range [][]string{{"orset"}, {"orset", "nope"}, {"orset", "demo", "extra"}} {
		if code, _, _ := runCLI(t, "", args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if code, _, e := runCLI(t, "", "orset", "help"); code != 0 || !strings.Contains(e, "SIMULATION") {
		t.Errorf("help: %d %q", code, e)
	}
}

// linesEndingWith counts lines that contain marker and whose last word is last.
func linesEndingWith(text, marker, last string) int {
	n := 0
	for _, l := range strings.Split(text, "\n") {
		f := strings.Fields(l)
		if strings.Contains(l, marker) && len(f) > 0 && f[len(f)-1] == last {
			n++
		}
	}
	return n
}
