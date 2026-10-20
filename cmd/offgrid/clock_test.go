package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestClockDemoShowsTheWallClockLosingAndTheHLCWinning(t *testing.T) {
	code, out, errOut := runCLI(t, "", "clock", "demo")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	for _, want := range []string{
		"SIMULATION",
		"by the wall clock:  Mercy's older edit wins (WRONG",
		"by the HLC:         Biruk's edit wins (correct)",
		"refused: hlc: remote timestamp",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "WENT BACKWARDS") {
		t.Errorf("the HLC went backwards:\n%s", out)
	}
	if n := strings.Count(out, "   ok\n"); n != 6 {
		t.Errorf("%d ok lines, want 6", n)
	}
}

func TestClockDemoWithNoSkewSaysThatBothAgree(t *testing.T) {
	_, out, _ := runCLI(t, "", "clock", "demo", "-skew", "0s")
	if !strings.Contains(out, "Both agree") {
		t.Errorf("no-skew output:\n%s", out)
	}
}

func TestClockCommandErrors(t *testing.T) {
	for _, args := range [][]string{{"clock"}, {"clock", "nope"}, {"clock", "demo", "-skew", "48h"}, {"clock", "demo", "extra"}, {"clock", "demo", "-skew", "x"}} {
		if code, _, _ := runCLI(t, "", args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if code, _, e := runCLI(t, "", "clock", "help"); code != 0 || !strings.Contains(e, "SIMULATION") {
		t.Errorf("help: %d %q", code, e)
	}
}

func TestStoreOpsShowsTheHLCAndInfoShowsTheNewestStamp(t *testing.T) {
	db := dbPath(t)
	runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"Leaking pump","priority":"high"}`)
	code, out, _ := runCLI(t, "", "store", "-db", db, "ops")
	if code != 0 {
		t.Fatal(out)
	}
	line := strings.Split(strings.TrimSpace(out), "\n")[0]
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatal(err, line)
	}
	h, _ := m["hlc"].(string)
	if !strings.Contains(h, "#") || m["hlc_counter"] == nil {
		t.Errorf("op json lacks hlc: %s", line)
	}
	_, info, _ := runCLI(t, "", "store", "-db", db, "info")
	if !strings.Contains(info, "newest timestamp") || strings.Contains(info, "WARNING") {
		t.Errorf("info:\n%s", info)
	}
}
