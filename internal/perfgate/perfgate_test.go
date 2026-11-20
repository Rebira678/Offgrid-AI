package perfgate

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

const header = "goos: linux\ncpu: Test CPU 9000\npkg: offgrid/internal/store/sqlstore\n"

// bench builds `go test -bench` output for one benchmark with the given samples.
func bench(name string, ns, bytes, allocs []float64) string {
	var b strings.Builder
	for i := range ns {
		b.WriteString("Benchmark" + name + "-2 \t 100 \t " + f(ns[i]) + " ns/op \t " + f(bytes[i]) + " B/op \t " + f(allocs[i]) + " allocs/op\n")
	}
	return b.String()
}

func f(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func rep(v float64, n int, jitter float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		// a small deterministic wobble around v
		out[i] = v * (1 + jitter*float64((i*7)%11-5)/5)
	}
	return out
}

func parse(t *testing.T, s string) *Run {
	t.Helper()
	r, err := Parse(strings.NewReader(header + s))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestParseNamesAndSamples(t *testing.T) {
	r := parse(t, "BenchmarkRebuild/1000-ops-2 \t 3 \t 5.5e+07 ns/op \t 5379408 B/op \t 117690 allocs/op\n"+
		"BenchmarkRebuild/1000-ops-2 \t 3 \t 57550651 ns/op \t 5379408 B/op \t 117690 allocs/op\n"+
		"BenchmarkOdd-2 --- FAIL: BenchmarkOdd-2\n"+
		"BenchmarkApp-2 \t 10 \t 100 ns/op \t 7.5 MB/s\n")
	if r.CPU != "Test CPU 9000" {
		t.Fatalf("cpu %q", r.CPU)
	}
	name := "store/sqlstore.Rebuild/1000-ops"
	if got := r.Benchmarks[name][Time]; len(got) != 2 || got[0] != 5.5e7 {
		t.Fatalf("%v: %v", name, r.Benchmarks)
	}
	if len(r.Benchmarks["store/sqlstore.Odd"]) != 0 {
		t.Fatal("a failing line must not become a benchmark")
	}
	if got := r.Benchmarks["store/sqlstore.App"]["MB/s"]; len(got) != 1 {
		t.Fatal("custom metrics are kept")
	}
	if _, err := Parse(strings.NewReader("ok pkg 1s\n")); err == nil {
		t.Fatal("no benchmarks must be an error, not an empty pass")
	}
}

func TestMedianIgnoresOneOutlier(t *testing.T) {
	if got := Median([]float64{10, 11, 12, 10, 9999}); got != 11 {
		t.Fatalf("%v", got)
	}
	if got := Median([]float64{1, 3}); got != 2 {
		t.Fatalf("%v", got)
	}
	if !math.IsNaN(Median(nil)) {
		t.Fatal("empty has no median")
	}
}

func TestPValue(t *testing.T) {
	lo := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	hi := []float64{11, 12, 13, 14, 15, 16, 17, 18, 19, 20}
	if p := PValue(lo, hi); p > 0.001 {
		t.Fatalf("completely separated samples: p=%v", p)
	}
	mixed := []float64{1.5, 2.5, 3.5, 4.5, 5.5, 6.5, 7.5, 8.5, 9.5, 10.5}
	if p := PValue(lo, mixed); p < 0.5 {
		t.Fatalf("interleaved samples look the same: p=%v", p)
	}
	if p := PValue(lo, lo); p < 0.9 {
		t.Fatalf("identical samples: p=%v", p)
	}
	same := []float64{5, 5, 5, 5, 5}
	if p := PValue(same, same); p != 1 {
		t.Fatalf("all equal: p=%v", p)
	}
	if PValue(lo[:3], hi[:3]) != 1 {
		t.Fatal("three samples cannot be significant")
	}
	if a, b := PValue(lo, hi), PValue(hi, lo); math.Abs(a-b) > 1e-12 {
		t.Fatalf("the test is two-sided: %v vs %v", a, b)
	}
}

func verdicts(t *testing.T, oldNs, newNs, oldB, newB, oldA, newA []float64) map[string]string {
	t.Helper()
	o := parse(t, bench("X", oldNs, oldB, oldA))
	n := parse(t, bench("X", newNs, newB, newA))
	rows, _, _ := Compare(o, n, DefaultPolicy())
	out := map[string]string{}
	for _, r := range rows {
		out[r.Metric] = r.Verdict
	}
	return out
}

func TestCompareFlagsRealRegressionsOnly(t *testing.T) {
	flat := rep(1000, 10, 0)
	// 30% slower, clearly separated: a regression. Bytes and allocs unchanged.
	v := verdicts(t, rep(1000, 10, 0.01), rep(1300, 10, 0.01), flat, flat, flat, flat)
	if v[Time] != Regression || v[Bytes] != Same || v[Allocs] != Same {
		t.Fatalf("%v", v)
	}
	// 30% slower on a median but buried in noise: not significant, not a regression.
	v = verdicts(t, rep(1000, 10, 0.6), rep(1300, 10, 0.6), flat, flat, flat, flat)
	if v[Time] == Regression {
		t.Fatalf("noise failed the gate: %v", v)
	}
	// Significant but under the threshold (5% slower, limit 10%): fine.
	v = verdicts(t, rep(1000, 10, 0.005), rep(1050, 10, 0.005), flat, flat, flat, flat)
	if v[Time] != Same {
		t.Fatalf("%v", v)
	}
	// Faster: an improvement, never a failure.
	v = verdicts(t, rep(1000, 10, 0.01), rep(500, 10, 0.01), flat, flat, flat, flat)
	if v[Time] != Improvement {
		t.Fatalf("%v", v)
	}
	// An allocation count that grows 20% is a regression even though time did not move.
	v = verdicts(t, flat, flat, flat, flat, rep(100, 10, 0), rep(120, 10, 0))
	if v[Allocs] != Regression || v[Time] != Same {
		t.Fatalf("%v", v)
	}
	// Bytes the same way.
	v = verdicts(t, flat, flat, rep(1000, 10, 0), rep(1500, 10, 0), flat, flat)
	if v[Bytes] != Regression {
		t.Fatalf("%v", v)
	}
}

func TestCompareZeroAllocationsThatStartAllocating(t *testing.T) {
	flat := rep(1000, 10, 0)
	v := verdicts(t, flat, flat, rep(0, 10, 0), rep(48, 10, 0), rep(0, 10, 0), rep(2, 10, 0))
	if v[Allocs] != Regression || v[Bytes] != Regression {
		t.Fatalf("0 -> something is the strictest regression there is: %v", v)
	}
	v = verdicts(t, flat, flat, rep(0, 10, 0), rep(0, 10, 0), rep(0, 10, 0), rep(0, 10, 0))
	if v[Allocs] != Same {
		t.Fatalf("%v", v)
	}
}

func TestCompareAddedAndGone(t *testing.T) {
	flat := rep(10, 5, 0)
	o := parse(t, bench("Old", flat, flat, flat)+bench("Both", flat, flat, flat))
	n := parse(t, bench("New", flat, flat, flat)+bench("Both", flat, flat, flat))
	_, added, gone := Compare(o, n, DefaultPolicy())
	if len(added) != 1 || added[0] != "store/sqlstore.New" || len(gone) != 1 || gone[0] != "store/sqlstore.Old" {
		t.Fatalf("added %v gone %v", added, gone)
	}
}

func TestBudgets(t *testing.T) {
	good := `{"hardware":"Test CPU","budgets":[{"bench":"store/sqlstore.X","metric":"ns/op","max":1500,"why":"a write must feel instant"}]}`
	b, err := ParseBudgets(strings.NewReader(good))
	if err != nil {
		t.Fatal(err)
	}
	flat := rep(1000, 10, 0)
	ok := parse(t, bench("X", flat, flat, flat))
	if v, enforced := b.Check(ok); !enforced || len(v) != 0 {
		t.Fatalf("within budget: %v %v", v, enforced)
	}
	slow := parse(t, bench("X", rep(2000, 10, 0), flat, flat))
	if v, _ := b.Check(slow); len(v) != 1 || v[0].Got != 2000 {
		t.Fatalf("over budget: %+v", v)
	}
	missing := parse(t, bench("Y", flat, flat, flat))
	if v, _ := b.Check(missing); len(v) != 1 || !v[0].Missing {
		t.Fatalf("a budgeted benchmark that vanished must fail: %+v", v)
	}
	other, _ := Parse(strings.NewReader(strings.Replace(header, "Test CPU 9000", "Other CPU", 1) + bench("X", rep(9e9, 10, 0), flat, flat)))
	if v, enforced := b.Check(other); enforced || len(v) != 0 {
		t.Fatalf("budgets for other hardware must not be enforced: %v %v", v, enforced)
	}
	for _, bad := range []string{
		`{"budgets":[]}`,
		`{"hardware":"x","budgets":[{"bench":"a","metric":"ns/op","max":0,"why":"w"}]}`,
		`{"hardware":"x","budgets":[{"bench":"a","metric":"ns/op","max":1,"why":""}]}`,
		`{"hardware":"x","budgets":[{"bench":"a","metric":"MB/s","max":1,"why":"w"}]}`,
		`{"hardware":"x","typo":1,"budgets":[]}`,
	} {
		if _, err := ParseBudgets(strings.NewReader(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

func TestOverrideNeedsAReason(t *testing.T) {
	if r, err := Override("  "); r != "" || err != nil {
		t.Fatalf("%q %v", r, err)
	}
	for _, bad := range []string{"ok", "because", "just ship it", "x y z"} {
		if _, err := Override(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	r, err := Override("  trading 12% on Rebuild   for crash safety ")
	if err != nil || r != "trading 12% on Rebuild for crash safety" {
		t.Fatalf("%q %v", r, err)
	}
}

func TestFormat(t *testing.T) {
	for _, c := range []struct {
		m    string
		v    float64
		want string
	}{
		{Time, 12.5, "12.5 ns"}, {Time, 2500, "2.50 µs"}, {Time, 3.5e6, "3.50 ms"}, {Time, 2e9, "2.00 s"},
		{Bytes, 100, "100 B"}, {Bytes, 2048, "2.0 KiB"}, {Bytes, 3 << 20, "3.00 MiB"}, {Allocs, 394, "394"},
	} {
		if got := Format(c.m, c.v); got != c.want {
			t.Errorf("%s %v: %q want %q", c.m, c.v, got, c.want)
		}
	}
}
