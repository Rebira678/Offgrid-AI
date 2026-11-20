// Package perfgate compares two runs of `go test -bench` and decides whether a change made
// the code slower, and checks a run against budgets set for named hardware.
//
// It exists next to benchstat, not instead of it: benchstat is the readable report a person
// looks at; this is the part that can say "fail" and be tested. It uses only the standard
// library, so the project's go.mod does not grow a dependency for a CI check.
//
// The gate is deliberately conservative about what counts as a regression: a metric must be
// worse by more than a threshold AND the difference must be statistically visible
// (Mann-Whitney U, as benchstat uses), so noise on a shared runner does not fail good changes.
// A gate can still be wrong, so every failure can be overridden with a written reason
// (see Override), and the override is printed in the report.
package perfgate

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Metric names as `go test -bench -benchmem` prints them.
const (
	Time   = "ns/op"
	Bytes  = "B/op"
	Allocs = "allocs/op"
)

// Run is one parsed benchmark file: for each benchmark, every sample of every metric.
type Run struct {
	CPU        string // the "cpu:" header line, which names the hardware
	Benchmarks map[string]map[string][]float64
	Order      []string // benchmark names in first-seen order
}

var procSuffix = regexp.MustCompile(`-\d+$`)

// Parse reads `go test -bench` output. Benchmark names are made "pkg.Name" with the module
// prefix "offgrid/internal/" and the "Benchmark" prefix and the GOMAXPROCS suffix removed,
// so a run on 2 cores and a run on 12 cores name the same benchmark the same way.
func Parse(r io.Reader) (*Run, error) {
	run := &Run{Benchmarks: map[string]map[string][]float64{}}
	pkg := ""
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "pkg:"):
			pkg = strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(line, "pkg:")), "offgrid/internal/")
		case strings.HasPrefix(line, "cpu:"):
			if run.CPU == "" {
				run.CPU = strings.TrimSpace(strings.TrimPrefix(line, "cpu:"))
			}
		case strings.HasPrefix(line, "Benchmark"):
			f := strings.Fields(line)
			if len(f) < 4 || len(f)%2 != 0 {
				continue // a line like "BenchmarkX-2 --- FAIL" or a progress line
			}
			if _, err := strconv.Atoi(f[1]); err != nil {
				continue
			}
			name := procSuffix.ReplaceAllString(strings.TrimPrefix(f[0], "Benchmark"), "")
			if pkg != "" {
				name = pkg + "." + name
			}
			vals := run.Benchmarks[name]
			for i := 2; i+1 < len(f); i += 2 {
				v, err := strconv.ParseFloat(f[i], 64)
				if err != nil {
					return nil, fmt.Errorf("benchmark %s: %q is not a number", name, f[i])
				}
				if vals == nil {
					vals = map[string][]float64{}
					run.Benchmarks[name] = vals
					run.Order = append(run.Order, name)
				}
				vals[f[i+1]] = append(vals[f[i+1]], v)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(run.Benchmarks) == 0 {
		return nil, fmt.Errorf("no benchmark results found (is this `go test -bench` output?)")
	}
	return run, nil
}

// Median of xs (not modified). The median, not the mean: one slow sample (a GC, a noisy
// neighbour) must not move the answer.
func Median(xs []float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	if n := len(s); n%2 == 1 {
		return s[n/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

// PValue is the two-sided p-value of the Mann-Whitney U test (normal approximation with
// continuity and tie corrections) that a and b come from the same distribution. With fewer
// than 4 samples on a side it returns 1: too few to say anything, so nothing is "significant".
func PValue(a, b []float64) float64 {
	n1, n2 := len(a), len(b)
	if n1 < 4 || n2 < 4 {
		return 1
	}
	type obs struct {
		v     float64
		first bool
	}
	all := make([]obs, 0, n1+n2)
	for _, v := range a {
		all = append(all, obs{v, true})
	}
	for _, v := range b {
		all = append(all, obs{v, false})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].v < all[j].v })
	n := float64(n1 + n2)
	var r1, tie float64
	for i := 0; i < len(all); {
		j := i
		for j < len(all) && all[j].v == all[i].v {
			j++
		}
		rank := float64(i+j+1) / 2 // average of ranks i+1 .. j
		for k := i; k < j; k++ {
			if all[k].first {
				r1 += rank
			}
		}
		t := float64(j - i)
		tie += t*t*t - t
		i = j
	}
	f1, f2 := float64(n1), float64(n2)
	u := r1 - f1*(f1+1)/2
	mu := f1 * f2 / 2
	sigma2 := f1 * f2 / 12 * ((n + 1) - tie/(n*(n-1)))
	if sigma2 <= 0 {
		return 1
	}
	z := (math.Abs(u-mu) - 0.5) / math.Sqrt(sigma2)
	if z < 0 {
		z = 0
	}
	return math.Erfc(z / math.Sqrt2)
}

// Policy says what counts as a regression.
type Policy struct {
	Alpha     float64 // significance level, e.g. 0.05
	MaxTime   float64 // allowed slowdown as a fraction of the old median, e.g. 0.10
	MaxBytes  float64 // allowed growth of B/op
	MaxAllocs float64 // allowed growth of allocs/op
}

// DefaultPolicy is the project's: 10% on time and bytes, 5% on allocation counts (which do not
// jitter, so a smaller step is already a real change), at the 5% significance level.
func DefaultPolicy() Policy {
	return Policy{Alpha: 0.05, MaxTime: 0.10, MaxBytes: 0.10, MaxAllocs: 0.05}
}

func (p Policy) limit(metric string) (float64, bool) {
	switch metric {
	case Time:
		return p.MaxTime, true
	case Bytes:
		return p.MaxBytes, true
	case Allocs:
		return p.MaxAllocs, true
	}
	return 0, false // custom metrics (MB/s, ops/s) are reported by benchstat, not gated here
}

// Verdicts of one comparison.
const (
	Regression  = "regression"
	Improvement = "improvement"
	Same        = "same"
	Noisy       = "not significant"
)

// Row is one benchmark and one metric, old against new.
type Row struct {
	Name, Metric string
	Old, New     float64 // medians
	Delta        float64 // (New-Old)/Old; positive is worse
	P            float64
	Verdict      string
}

// Compare returns one Row per gated metric of every benchmark present in both runs, in the
// order of the new run, plus the names present in only one of them.
func Compare(old, cur *Run, pol Policy) (rows []Row, added, gone []string) {
	for _, name := range cur.Order {
		ov, ok := old.Benchmarks[name]
		if !ok {
			added = append(added, name)
			continue
		}
		for _, metric := range []string{Time, Bytes, Allocs} {
			os, ns := ov[metric], cur.Benchmarks[name][metric]
			limit, _ := pol.limit(metric)
			if len(os) == 0 || len(ns) == 0 {
				continue
			}
			o, n := Median(os), Median(ns)
			row := Row{Name: name, Metric: metric, Old: o, New: n, P: PValue(os, ns)}
			switch {
			case o == 0 && n == 0:
				row.Verdict = Same
			case o == 0:
				row.Delta = math.Inf(1)
				row.Verdict = Regression
				if row.P > pol.Alpha && len(os) >= 4 && len(ns) >= 4 {
					row.Verdict = Noisy
				}
			default:
				row.Delta = (n - o) / o
				switch {
				case row.P > pol.Alpha:
					row.Verdict = Noisy
					if math.Abs(row.Delta) < 0.02 {
						row.Verdict = Same
					}
				case row.Delta > limit:
					row.Verdict = Regression
				case row.Delta < -limit:
					row.Verdict = Improvement
				default:
					row.Verdict = Same
				}
			}
			rows = append(rows, row)
		}
	}
	for _, name := range old.Order {
		if _, ok := cur.Benchmarks[name]; !ok {
			gone = append(gone, name)
		}
	}
	return rows, added, gone
}

// Budget is a ceiling set from a service-level objective, for one benchmark on named hardware.
type Budget struct {
	Bench  string  `json:"bench"`
	Metric string  `json:"metric"`
	Max    float64 `json:"max"`
	Why    string  `json:"why"`
}

// Budgets are the ceilings that hold on Hardware, a substring of the "cpu:" line.
type Budgets struct {
	Hardware string   `json:"hardware"`
	Note     string   `json:"note,omitempty"`
	Budgets  []Budget `json:"budgets"`
}

// ParseBudgets reads and checks a budgets file.
func ParseBudgets(r io.Reader) (*Budgets, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var b Budgets
	if err := dec.Decode(&b); err != nil {
		return nil, fmt.Errorf("budgets: %w", err)
	}
	if strings.TrimSpace(b.Hardware) == "" {
		return nil, fmt.Errorf("budgets: \"hardware\" must name the machine the numbers are for")
	}
	for i, x := range b.Budgets {
		if x.Bench == "" || x.Max <= 0 || strings.TrimSpace(x.Why) == "" {
			return nil, fmt.Errorf("budgets: entry %d needs a bench, a max above 0 and a reason (why)", i+1)
		}
		if _, ok := (Policy{}).limit(x.Metric); !ok {
			return nil, fmt.Errorf("budgets: entry %d (%s): metric must be one of %s, %s, %s", i+1, x.Bench, Time, Bytes, Allocs)
		}
	}
	return &b, nil
}

// Violation is a benchmark over its budget, or a budgeted benchmark the run did not contain.
type Violation struct {
	Budget  Budget
	Got     float64
	Missing bool
}

// Check compares a run with the budgets. enforced is false when the run was made on other
// hardware than the budgets name: ceilings set on one machine mean nothing on another, so
// nothing is reported as violated, and the caller says so.
func (b *Budgets) Check(run *Run) (violations []Violation, enforced bool) {
	if !strings.Contains(run.CPU, b.Hardware) {
		return nil, false
	}
	for _, x := range b.Budgets {
		xs := run.Benchmarks[x.Bench][x.Metric]
		if len(xs) == 0 {
			violations = append(violations, Violation{Budget: x, Missing: true})
			continue
		}
		if got := Median(xs); got > x.Max {
			violations = append(violations, Violation{Budget: x, Got: got})
		}
	}
	return violations, true
}

// Override is the explicit way past a failing gate: a reason in words, at least a few of them,
// so that "override" is never an empty flag. It returns the cleaned reason.
func Override(reason string) (string, error) {
	reason = strings.Join(strings.Fields(reason), " ")
	if reason == "" {
		return "", nil
	}
	if len(strings.Fields(reason)) < 3 || len(reason) < 15 {
		return "", fmt.Errorf("an override needs a reason in words (at least 3 words, 15 characters), got %q", reason)
	}
	return reason, nil
}

// Format renders a number the way a person reads it.
func Format(metric string, v float64) string {
	switch metric {
	case Time:
		switch {
		case v >= 1e9:
			return fmt.Sprintf("%.2f s", v/1e9)
		case v >= 1e6:
			return fmt.Sprintf("%.2f ms", v/1e6)
		case v >= 1e3:
			return fmt.Sprintf("%.2f µs", v/1e3)
		}
		return fmt.Sprintf("%.1f ns", v)
	case Bytes:
		switch {
		case v >= 1<<20:
			return fmt.Sprintf("%.2f MiB", v/(1<<20))
		case v >= 1<<10:
			return fmt.Sprintf("%.1f KiB", v/(1<<10))
		}
		return fmt.Sprintf("%.0f B", v)
	}
	return fmt.Sprintf("%.0f", v)
}
