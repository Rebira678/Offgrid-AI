package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"offgrid/internal/perfgate"
)

func benchText(cpu string, ns float64) string {
	var b strings.Builder
	b.WriteString("goos: linux\ncpu: " + cpu + "\npkg: offgrid/internal/store/sqlstore\n")
	for i := 0; i < 10; i++ {
		b.WriteString(fmt.Sprintf("BenchmarkAppend-2 \t 100 \t %.0f ns/op \t 20000 B/op \t 394 allocs/op\n", ns*(1+0.002*float64(i%3))))
	}
	return b.String()
}

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func gate(t *testing.T, env map[string]string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb, func(k string) string { return env[k] })
	return code, out.String(), errb.String()
}

func TestExitCodesAndOverride(t *testing.T) {
	dir := t.TempDir()
	old := write(t, dir, "old.txt", benchText("CPU A", 1000))
	same := write(t, dir, "same.txt", benchText("CPU A", 1001))
	slow := write(t, dir, "slow.txt", benchText("CPU A", 1500))
	elsewhere := write(t, dir, "else.txt", benchText("CPU B", 9000))
	budgets := write(t, dir, "b.json", `{"hardware":"CPU A","budgets":[{"bench":"store/sqlstore.Append","metric":"ns/op","max":1200,"why":"a write must feel instant"}]}`)

	if code, out, _ := gate(t, nil, "-old", old, "-new", same); code != 0 || !strings.Contains(out, "PASSED") {
		t.Fatalf("unchanged must pass: %d\n%s", code, out)
	}
	code, out, _ := gate(t, nil, "-old", old, "-new", slow)
	if code != 1 || !strings.Contains(out, "Regressions") || !strings.Contains(out, "store/sqlstore.Append") || !strings.Contains(out, "FAILED") {
		t.Fatalf("a 50%% slowdown must fail and name the benchmark: %d\n%s", code, out)
	}
	// The override passes it, and the reason is on the record.
	code, out, _ = gate(t, nil, "-old", old, "-new", slow, "-override", "crash safety is worth 50% here")
	if code != 0 || !strings.Contains(out, "OVERRIDDEN:** crash safety is worth 50% here") || !strings.Contains(out, "Regressions") {
		t.Fatalf("override: %d\n%s", code, out)
	}
	// The environment variable does the same; a one-word override is refused as bad input.
	if code, _, _ = gate(t, map[string]string{"PERF_OVERRIDE": "intentional trade for crash safety"}, "-old", old, "-new", slow); code != 0 {
		t.Fatalf("env override: %d", code)
	}
	if code, _, errs := gate(t, nil, "-old", old, "-new", slow, "-override", "ok"); code != 2 || !strings.Contains(errs, "reason") {
		t.Fatalf("a bare override must be refused: %d %s", code, errs)
	}
	// An unneeded override says so rather than pretending.
	if _, out, _ = gate(t, nil, "-old", old, "-new", same, "-override", "not needed but harmless"); !strings.Contains(out, "not needed") {
		t.Fatal(out)
	}
	// Budgets: over budget fails with no baseline at all; on other hardware they are not enforced.
	if code, out, _ = gate(t, nil, "-new", slow, "-budgets", budgets); code != 1 || !strings.Contains(out, "Over budget") || !strings.Contains(out, "a write must feel instant") {
		t.Fatalf("budget: %d\n%s", code, out)
	}
	if code, out, _ = gate(t, nil, "-new", elsewhere, "-budgets", budgets); code != 0 || !strings.Contains(out, "not enforced") {
		t.Fatalf("other hardware: %d\n%s", code, out)
	}
	// Runs from different machines are never compared.
	if code, out, _ = gate(t, nil, "-old", old, "-new", elsewhere); code != 0 || !strings.Contains(out, "Different hardware") {
		t.Fatalf("different hardware: %d\n%s", code, out)
	}
	// Bad input.
	if code, _, _ = gate(t, nil); code != 2 {
		t.Fatal("-new is required")
	}
	if code, _, _ = gate(t, nil, "-new", filepath.Join(dir, "nope.txt")); code != 2 {
		t.Fatal("a missing file is bad input")
	}
	// The report can be written for CI's step summary.
	md := filepath.Join(dir, "r.md")
	gate(t, nil, "-old", old, "-new", slow, "-md", md)
	if b, err := os.ReadFile(md); err != nil || !strings.Contains(string(b), "Regressions") {
		t.Fatalf("report file: %v", err)
	}
}

// The budgets file that ships with the project must parse and name only metrics the gate knows.
func TestTheShippedBudgetsFileIsValid(t *testing.T) {
	f, err := os.Open("../../perf/budgets.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, err := perfgate.ParseBudgets(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Budgets) < 5 {
		t.Fatalf("only %d budgets", len(b.Budgets))
	}
}
