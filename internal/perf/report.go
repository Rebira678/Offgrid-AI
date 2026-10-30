package perf

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
)

// WriteMarkdown writes the report as Markdown that can be pasted into a README.
func WriteMarkdown(w io.Writer, r Report) error {
	var b strings.Builder
	m := r.Meta
	hw := m.Hardware
	fmt.Fprintf(&b, "**Hardware:** %s, %d threads, %.1f GiB RAM, %s  \n", orNA(hw.CPU), hw.Threads, hw.MemGiB, orNA(hw.OS))
	fmt.Fprintf(&b, "**Run:** %s, %s, median of %d runs (min to max in brackets), seed %d, files in `%s` (%s)", m.Date, m.Go, m.Repeats, m.Seed, m.Dir, orNA(m.DirFS))
	if m.Governor != "" {
		fmt.Fprintf(&b, ", CPU governor `%s`", m.Governor)
	}
	b.WriteString(". No model is involved in these numbers.\n\n")

	b.WriteString("**Store** (one SQLite file, `synchronous=FULL`: every write is on disk when it returns):\n\n")
	b.WriteString("| What | Ops | Time (s) | Ops/s | File (MiB) | Peak Go heap (MiB) | Peak RSS (MiB) |\n|---|---|---|---|---|---|---|\n")
	for _, s := range r.Store {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %.1f | %.1f | %.0f |\n", s.Name, commas(s.Ops), spreadText(s.Seconds), rate(s.PerSec), s.DBMiB, s.Mem.HeapMiB, s.Mem.RSSMiB)
	}

	b.WriteString("\n**Sync** (the replica that catches up must end with the same state hash as its source, or the run fails):\n\n")
	b.WriteString("| What | Ops | Time (s) | Ops/s | HTTP requests | Peak Go heap (MiB) | Peak RSS (MiB) |\n|---|---|---|---|---|---|---|\n")
	for _, s := range r.Sync {
		req := "-"
		if s.Requests > 0 {
			req = fmt.Sprint(s.Requests)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %.1f | %.0f |\n", s.Mode, commas(s.Ops), spreadText(s.Seconds), rate(s.PerSec), req, s.Mem.HeapMiB, s.Mem.RSSMiB)
	}

	b.WriteString("\n**Queue** (jobs that do nothing, so this is the queue's own cost: claim, run, complete, each a durable write):\n\n")
	b.WriteString("| Jobs | At a time | Enqueue (s) | Drain (s) | Jobs/s drained | Peak Go heap (MiB) | Peak RSS (MiB) |\n|---|---|---|---|---|---|---|\n")
	for _, q := range r.Queue {
		fmt.Fprintf(&b, "| %s | %d | %s | %s | %s | %.1f | %.0f |\n", commas(q.Jobs), q.Concurrency, spreadText(q.Enqueue), spreadText(q.Drain), rate(q.DrainPerSec), q.Mem.HeapMiB, q.Mem.RSSMiB)
	}

	if m.RSSPerRun {
		b.WriteString("\nPeak RSS is the largest resident memory of the whole process during that row (counter reset before each row). ")
	} else {
		b.WriteString("\nPeak RSS here is a running maximum for the whole run (this system cannot reset the counter), so later rows can only repeat earlier peaks. ")
	}
	b.WriteString("Peak Go heap is sampled every 10 ms, so a very short spike can be missed. Sync over HTTP runs on one machine: it measures protocol, signing and storage cost, not a WiFi link.\n")
	_, err := io.WriteString(w, b.String())
	return err
}

// WriteJSON writes the report for charts and for comparing runs.
func WriteJSON(w io.Writer, r Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func spreadText(s Spread) string {
	return fmt.Sprintf("%s [%s to %s]", sig(s.Median), sig(s.Min), sig(s.Max))
}

// sig prints a time with about three significant figures.
func sig(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "-"
	}
	switch {
	case v >= 100:
		return fmt.Sprintf("%.0f", v)
	case v >= 10:
		return fmt.Sprintf("%.1f", v)
	case v >= 1:
		return fmt.Sprintf("%.2f", v)
	case v >= 0.01:
		return fmt.Sprintf("%.3f", v)
	}
	return fmt.Sprintf("%.4f", v)
}

func rate(v float64) string {
	switch {
	case math.IsNaN(v) || math.IsInf(v, 0) || v <= 0:
		return "-"
	case v >= 100:
		return commas(int(v + 0.5))
	case v >= 10:
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.1f", v)
}

func orNA(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// ReplaceBlock returns text with everything between the start and end marker
// lines replaced by body. The markers stay. It refuses, rather than guessing,
// when a marker is missing, repeated or out of order, so a README can never be
// quietly left stale or damaged.
func ReplaceBlock(text, start, end, body string) (string, error) {
	if strings.Count(text, start) != 1 || strings.Count(text, end) != 1 {
		return "", fmt.Errorf("perf: the file must contain exactly one %q and one %q", start, end)
	}
	i := strings.Index(text, start)
	j := strings.Index(text, end)
	if j < i {
		return "", fmt.Errorf("perf: %q comes before %q", end, start)
	}
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return text[:i+len(start)] + "\n" + body + text[j:], nil
}
