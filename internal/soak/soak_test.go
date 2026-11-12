package soak

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)

// series builds n samples, 10 s apart, from functions of the sample index.
func series(n int, g func(i int) int, rss func(i int) int64) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		s := Sample{At: t0.Add(time.Duration(i) * 10 * time.Second), Role: "run", Goroutines: g(i), RSSKB: rss(i), Rounds: int64(i), Ops: int64(i * 3)}
		j, _ := json.Marshal(s)
		b.Write(j)
		b.WriteByte('\n')
	}
	return b.String()
}

func flat(v int) func(int) int       { return func(int) int { return v } }
func flat64(v int64) func(int) int64 { return func(int) int64 { return v } }

func summarize(t *testing.T, s string) Report {
	t.Helper()
	r, err := Summarize(strings.NewReader(s), DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAFlatRunPasses(t *testing.T) {
	r := summarize(t, series(100, flat(12), flat64(30000)))
	if r.Verdict != "PASS" || len(r.Problems) != 0 {
		t.Fatalf("%+v", r)
	}
	if r.Samples != 100 || r.Duration != 990*time.Second || r.GoroutinesEnd != 12 || !r.RSSMeasured {
		t.Fatalf("%+v", r)
	}
}

func TestOneGoroutineLeakedPerSampleFails(t *testing.T) {
	r := summarize(t, series(100, func(i int) int { return 12 + i/4 }, flat64(30000)))
	if r.Verdict != "FAIL" || !strings.Contains(strings.Join(r.Problems, "|"), "goroutines grew") {
		t.Fatalf("%+v", r)
	}
}

func TestMemoryCreepFails(t *testing.T) {
	r := summarize(t, series(100, flat(12), func(i int) int64 { return 30000 + int64(i)*200 })) // +66% over the run
	if r.Verdict != "FAIL" || !strings.Contains(strings.Join(r.Problems, "|"), "RSS grew") {
		t.Fatalf("%+v", r)
	}
}

func TestWarmUpIsNotALeak(t *testing.T) {
	// The first tenth is high (caches filling, pools growing), then the process settles lower and flat.
	g := func(i int) int {
		if i < 10 {
			return 40
		}
		return 12
	}
	rss := func(i int) int64 {
		if i < 10 {
			return 90000
		}
		return 30000
	}
	if r := summarize(t, series(100, g, rss)); r.Verdict != "PASS" {
		t.Fatalf("%+v", r)
	}
}

func TestOneBurstAtTheEndIsNotAVerdict(t *testing.T) {
	// A single sample in the middle of a burst must not fail a healthy run: the end is a median of the last few.
	g := func(i int) int {
		if i == 99 {
			return 60
		}
		return 12
	}
	if r := summarize(t, series(100, g, flat64(30000))); r.Verdict != "PASS" {
		t.Fatalf("%+v", r)
	}
}

func TestSmallWobbleWithinTheLimitsPasses(t *testing.T) {
	g := func(i int) int { return 12 + i%3 }
	rss := func(i int) int64 { return 30000 + int64(i%7)*300 }
	if r := summarize(t, series(100, g, rss)); r.Verdict != "PASS" {
		t.Fatalf("%+v", r)
	}
}

func TestAShortRunGivesNoVerdict(t *testing.T) {
	r := summarize(t, series(10, flat(12), flat64(30000)))
	if r.Verdict != "TOO SHORT" || len(r.Problems) != 1 {
		t.Fatalf("%+v", r)
	}
}

func TestWithoutRSSOnlyGoroutinesAreJudged(t *testing.T) {
	r := summarize(t, series(100, flat(12), flat64(0)))
	if r.Verdict != "PASS" || r.RSSMeasured || !strings.Contains(r.Format(), "RSS         not measured") {
		t.Fatalf("%+v\n%s", r, r.Format())
	}
	r = summarize(t, series(100, func(i int) int { return 12 + i }, flat64(0)))
	if r.Verdict != "FAIL" {
		t.Fatalf("a goroutine leak must fail even where RSS is unknown: %+v", r)
	}
}

func TestARunOfFailedRoundsSaysNothingAboutLeaks(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 100; i++ {
		j, _ := json.Marshal(Sample{At: t0.Add(time.Duration(i) * 10 * time.Second), Goroutines: 12, RSSKB: 30000, Rounds: int64(i + 1), Failures: int64(i + 1)})
		b.Write(j)
		b.WriteByte('\n')
	}
	r := summarize(t, b.String())
	if r.Verdict != "FAIL" || !strings.Contains(strings.Join(r.Problems, "|"), "rounds failed") {
		t.Fatalf("%+v", r)
	}
}

func TestSamplesAreSortedByTimeBeforeJudging(t *testing.T) {
	lines := strings.Split(strings.TrimSpace(series(100, func(i int) int { return 12 + i/4 }, flat64(30000))), "\n")
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	r := summarize(t, strings.Join(lines, "\n"))
	if r.GoroutinesStart != 12 || r.Verdict != "FAIL" {
		t.Fatalf("a file written out of order must read the same: %+v", r)
	}
}

func TestMalformedAndEmptyInputAreErrors(t *testing.T) {
	if _, err := Summarize(strings.NewReader("not json\n"), DefaultLimits); err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("err = %v", err)
	}
	if _, err := Summarize(strings.NewReader("\n\n"), DefaultLimits); err == nil {
		t.Fatal("an empty file is not a pass")
	}
}

func TestParseRSS(t *testing.T) {
	if got := parseRSS("Name:\tx\nVmRSS:\t   12345 kB\nThreads:\t4\n"); got != 12345 {
		t.Fatalf("got %d", got)
	}
	if parseRSS("Name:\tx\n") != 0 || parseRSS("VmRSS: lots kB") != 0 {
		t.Fatal("unreadable RSS must read as 0 (not measured), not as a number")
	}
}

func TestRunWritesAFirstAndALastSampleAndStopsWithItsContext(t *testing.T) {
	var buf bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	n := int64(0)
	if err := Run(ctx, &buf, "serve", 20*time.Millisecond, func(s *Sample) { n++; s.Ops = n }); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) < 4 {
		t.Fatalf("want a first sample, some in between and a last: %d lines", len(lines))
	}
	var first, last Sample
	json.Unmarshal([]byte(lines[0]), &first)
	json.Unmarshal([]byte(lines[len(lines)-1]), &last)
	if first.Role != "serve" || first.Goroutines < 1 || last.Ops <= first.Ops || !last.At.After(first.At) {
		t.Fatalf("first %+v last %+v", first, last)
	}
	if err := Run(context.Background(), &buf, "x", 0, nil); err == nil {
		t.Fatal("a zero interval must be refused, not spin")
	}
}

func TestFormatNamesEveryProblemAndTheResult(t *testing.T) {
	r := Report{Samples: 3, Verdict: "FAIL", Problems: []string{"a", "b"}}
	out := r.Format()
	for _, want := range []string{"PROBLEM: a", "PROBLEM: b", "RESULT: FAIL"} {
		if !strings.Contains(out, want) {
			t.Fatalf("%q missing from\n%s", want, out)
		}
	}
	_ = fmt.Sprint
}

func TestManySamplesInAFewSecondsAreStillTooShort(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 100; i++ {
		j, _ := json.Marshal(Sample{At: t0.Add(time.Duration(i) * 50 * time.Millisecond), Goroutines: 12, RSSKB: 30000})
		b.Write(j)
		b.WriteByte('\n')
	}
	r := summarize(t, b.String())
	if r.Verdict != "TOO SHORT" || !strings.Contains(strings.Join(r.Problems, "|"), "lasted") {
		t.Fatalf("%+v", r)
	}
	lim := DefaultLimits
	lim.MinDuration = time.Second
	if r, _ := Summarize(strings.NewReader(b.String()), lim); r.Verdict != "PASS" {
		t.Fatalf("the limit must be adjustable: %+v", r)
	}
}

func TestFewSamplesOverALongTimeAreTooShortToo(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 10; i++ { // 10 samples two minutes apart: long enough, too sparse to judge
		j, _ := json.Marshal(Sample{At: t0.Add(time.Duration(i) * 2 * time.Minute), Goroutines: 12, RSSKB: 30000})
		b.Write(j)
		b.WriteByte('\n')
	}
	r := summarize(t, b.String())
	if r.Verdict != "TOO SHORT" || !strings.Contains(strings.Join(r.Problems, "|"), "only 10 samples") {
		t.Fatalf("%+v", r)
	}
}
