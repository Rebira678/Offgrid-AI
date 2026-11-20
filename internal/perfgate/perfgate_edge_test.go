package perfgate

import (
	"strings"
	"testing"
)

func TestParseExtremesAndMalformedLines(t *testing.T) {
	input := `
# arbitrary comments should be ignored
goos: linux
cpu: Edge Testing CPU
pkg: offgrid/internal/crdt

BenchmarkSingle-1 500 120 ns/op
BenchmarkOddFields 100 200 ns/op 300
BenchmarkProgress-4 ...
BenchmarkFail-8 --- FAIL: BenchmarkFail-8
BenchmarkPanic-2 panic: runtime error
BenchmarkNonNumericIter abc 100 ns/op

pkg: offgrid/internal/hlc
BenchmarkMultiMetrics-16 1000 55.5 ns/op 32 B/op 2 allocs/op 10.5 MB/s 500 req/s

pkg: external/vendor/lib
BenchmarkVendor-128 200 450 ns/op
`
	run, err := Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}

	if run.CPU != "Edge Testing CPU" {
		t.Fatalf("unexpected CPU: %q", run.CPU)
	}

	// BenchmarkSingle should have prefix "crdt." and suffix "-1" stripped
	if samples := run.Benchmarks["crdt.Single"][Time]; len(samples) != 1 || samples[0] != 120 {
		t.Fatalf("crdt.Single samples: %v", samples)
	}

	// BenchmarkMultiMetrics should have prefix "hlc." and all 5 metrics captured
	hlc := run.Benchmarks["hlc.MultiMetrics"]
	if len(hlc[Time]) != 1 || hlc[Time][0] != 55.5 {
		t.Fatalf("hlc.MultiMetrics Time: %v", hlc[Time])
	}
	if len(hlc[Bytes]) != 1 || hlc[Bytes][0] != 32 {
		t.Fatalf("hlc.MultiMetrics Bytes: %v", hlc[Bytes])
	}
	if len(hlc[Allocs]) != 1 || hlc[Allocs][0] != 2 {
		t.Fatalf("hlc.MultiMetrics Allocs: %v", hlc[Allocs])
	}
	if len(hlc["MB/s"]) != 1 || hlc["MB/s"][0] != 10.5 {
		t.Fatalf("hlc.MultiMetrics MB/s: %v", hlc["MB/s"])
	}
	if len(hlc["req/s"]) != 1 || hlc["req/s"][0] != 500 {
		t.Fatalf("hlc.MultiMetrics req/s: %v", hlc["req/s"])
	}

	// Vendor package should keep "external/vendor/lib.Vendor"
	if samples := run.Benchmarks["external/vendor/lib.Vendor"][Time]; len(samples) != 1 || samples[0] != 450 {
		t.Fatalf("vendor samples: %v", samples)
	}

	// Malformed lines should not produce benchmark entries
	for _, bad := range []string{"OddFields", "Progress", "Fail", "Panic", "NonNumericIter"} {
		if _, exists := run.Benchmarks["crdt."+bad]; exists {
			t.Fatalf("malformed benchmark crdt.%s was unexpectedly parsed", bad)
		}
	}
}

func TestParseCorruptNumericValue(t *testing.T) {
	input := `
goos: linux
cpu: Edge CPU
pkg: offgrid/internal/crdt
BenchmarkCorrupt-2 100 not_a_number ns/op
`
	if _, err := Parse(strings.NewReader(input)); err == nil {
		t.Fatal("expected error parsing non-numeric metric value, got nil")
	}
}

func TestPValueAsymmetricAndTieStress(t *testing.T) {
	// Fewer than 4 samples on either side must return 1.0
	if p := PValue([]float64{1, 2, 3}, []float64{10, 20, 30, 40}); p != 1.0 {
		t.Fatalf("expected 1.0 for len(a)<4, got %v", p)
	}
	if p := PValue([]float64{1, 2, 3, 4}, []float64{10, 20, 30}); p != 1.0 {
		t.Fatalf("expected 1.0 for len(b)<4, got %v", p)
	}

	// All identical samples (ties across all elements)
	allSameA := []float64{42, 42, 42, 42, 42}
	allSameB := []float64{42, 42, 42, 42, 42}
	if p := PValue(allSameA, allSameB); p != 1.0 {
		t.Fatalf("expected 1.0 for completely tied samples, got %v", p)
	}

	// Asymmetric sizes (4 vs 16) with clear separation
	smallA := []float64{1, 2, 3, 4}
	largeB := []float64{10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25}
	if p := PValue(smallA, largeB); p > 0.05 {
		t.Fatalf("expected significant p < 0.05 for clearly separated asymmetric samples, got %v", p)
	}

	// Very large and very small magnitudes
	hugeA := []float64{1e12 + 1, 1e12 + 2, 1e12 + 3, 1e12 + 4}
	hugeB := []float64{1e12 + 100, 1e12 + 101, 1e12 + 102, 1e12 + 103}
	if p := PValue(hugeA, hugeB); p > 0.05 {
		t.Fatalf("expected significant p for huge magnitudes, got %v", p)
	}
}

func TestCompareZeroTransitionsAndNoiseBand(t *testing.T) {
	pol := DefaultPolicy()

	// Case 1: Old is > 0, New drops to 0 (Improvement)
	o1 := parse(t, bench("DropToZero", rep(500, 10, 0), rep(100, 10, 0), rep(10, 10, 0)))
	n1 := parse(t, bench("DropToZero", rep(500, 10, 0), rep(0, 10, 0), rep(0, 10, 0)))
	rows1, _, _ := Compare(o1, n1, pol)
	for _, r := range rows1 {
		if r.Metric == Bytes || r.Metric == Allocs {
			if r.Verdict != Improvement || r.Delta != -1.0 {
				t.Fatalf("%s drop to 0 got verdict=%q, delta=%v", r.Metric, r.Verdict, r.Delta)
			}
		}
	}

	// Case 2: Noise band (< 2% delta with non-significant p-value) should be "same"
	o2 := parse(t, bench("NoiseBand", rep(1000, 10, 0.05), rep(50, 10, 0), rep(5, 10, 0)))
	n2 := parse(t, bench("NoiseBand", rep(1010, 10, 0.05), rep(50, 10, 0), rep(5, 10, 0))) // 1% difference
	rows2, _, _ := Compare(o2, n2, pol)
	for _, r := range rows2 {
		if r.Metric == Time {
			if r.Verdict != Same {
				t.Fatalf("1%% shift within noise band must be 'same', got %q", r.Verdict)
			}
		}
	}
}

func TestBudgetsValidationExtremes(t *testing.T) {
	// Valid budget with partial hardware substring match
	validJSON := `{
		"hardware": "AMD Ryzen",
		"budgets": [
			{"bench": "crdt.Sync", "metric": "ns/op", "max": 5000, "why": "must stay under 5us"},
			{"bench": "crdt.Sync", "metric": "allocs/op", "max": 10, "why": "must not allocate"}
		]
	}`
	budgets, err := ParseBudgets(strings.NewReader(validJSON))
	if err != nil {
		t.Fatalf("ParseBudgets failed: %v", err)
	}

	// Matching hardware (contains "AMD Ryzen")
	matchingRun := &Run{
		CPU: "AMD Ryzen 9 5950X 16-Core Processor",
		Benchmarks: map[string]map[string][]float64{
			"crdt.Sync": {
				Time:   {4000, 4200, 4100},
				Allocs: {15, 16, 15}, // Exceeds budget max 10
			},
		},
	}
	viol, enforced := budgets.Check(matchingRun)
	if !enforced {
		t.Fatal("expected budgets to be enforced on matching hardware")
	}
	if len(viol) != 1 || viol[0].Budget.Metric != Allocs {
		t.Fatalf("expected 1 allocs violation, got %v", viol)
	}

	// Missing benchmark in run
	emptyRun := &Run{
		CPU:        "AMD Ryzen 7 5800X",
		Benchmarks: map[string]map[string][]float64{},
	}
	violEmpty, enforcedEmpty := budgets.Check(emptyRun)
	if !enforcedEmpty {
		t.Fatal("expected enforced true on matching hardware")
	}
	if len(violEmpty) != 2 || !violEmpty[0].Missing || !violEmpty[1].Missing {
		t.Fatalf("expected 2 missing violations, got %+v", violEmpty)
	}

	// Invalid budget schemas
	invalidSchemas := []string{
		`{"hardware": "", "budgets": [{"bench":"x","metric":"ns/op","max":100,"why":"w"}]}`,
		`{"hardware": "CPU", "budgets": [{"bench":"","metric":"ns/op","max":100,"why":"w"}]}`,
		`{"hardware": "CPU", "budgets": [{"bench":"x","metric":"ns/op","max":-5,"why":"w"}]}`,
		`{"hardware": "CPU", "budgets": [{"bench":"x","metric":"invalid-metric","max":100,"why":"w"}]}`,
		`{"hardware": "CPU", "budgets": [{"bench":"x","metric":"ns/op","max":100,"why":"   "}]}`,
		`{"hardware": "CPU", "extra_unknown_field": 123, "budgets": []}`,
	}
	for i, bad := range invalidSchemas {
		if _, err := ParseBudgets(strings.NewReader(bad)); err == nil {
			t.Fatalf("schema %d should have been rejected: %s", i, bad)
		}
	}
}

func TestOverrideBoundaryLengths(t *testing.T) {
	// Exact boundary test:
	// Words >= 3, Length >= 15
	// "one two three" -> 3 words, 13 chars -> MUST FAIL
	if _, err := Override("one two three"); err == nil {
		t.Fatal("3 words but 13 characters must fail")
	}

	// "one two 1234567" -> 3 words, 15 chars -> MUST PASS
	if res, err := Override("one two 1234567"); err != nil || res != "one two 1234567" {
		t.Fatalf("3 words, 15 chars must pass: res=%q, err=%v", res, err)
	}

	// Multiple internal tabs, spaces, newlines should normalize to single spaces
	messy := "\t  accepting   higher   latency \n  for  safety   "
	expected := "accepting higher latency for safety"
	if res, err := Override(messy); err != nil || res != expected {
		t.Fatalf("expected normalized %q, got %q (err=%v)", expected, res, err)
	}
}

func TestFormatEdgeCases(t *testing.T) {
	tests := []struct {
		metric string
		val    float64
		want   string
	}{
		{Time, 0.1, "0.1 ns"},
		{Time, 999.4, "999.4 ns"},
		{Time, 1000.0, "1.00 µs"},
		{Time, 1500000.0, "1.50 ms"},
		{Time, 2500000000.0, "2.50 s"},

		{Bytes, 0, "0 B"},
		{Bytes, 512, "512 B"},
		{Bytes, 1024, "1.0 KiB"},
		{Bytes, 1048576, "1.00 MiB"},
		{Bytes, 52428800, "50.00 MiB"},

		{Allocs, 0, "0"},
		{Allocs, 1, "1"},
		{Allocs, 1234567, "1234567"},

		{"unknown", 42.5, "42"},
	}

	for _, tc := range tests {
		if got := Format(tc.metric, tc.val); got != tc.want {
			t.Errorf("Format(%q, %v) = %q, want %q", tc.metric, tc.val, got, tc.want)
		}
	}
}
