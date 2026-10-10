package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// TierStat summarises the requests one tier handled.
type TierStat struct {
	Tier     Tier
	Count    int
	Median   time.Duration
	Max      time.Duration
	Statuses map[Status]int
}

// Stats groups outcomes by tier, in the order rules, model, none. Tiers that
// handled nothing are omitted.
func Stats(outs []Outcome) []TierStat {
	groups := map[Tier][]Outcome{}
	for _, o := range outs {
		groups[o.Tier] = append(groups[o.Tier], o)
	}
	var stats []TierStat
	for _, tier := range []Tier{TierRules, TierModel, TierNone} {
		g := groups[tier]
		if len(g) == 0 {
			continue
		}
		st := TierStat{Tier: tier, Count: len(g), Statuses: map[Status]int{}}
		var times []time.Duration
		for _, o := range g {
			st.Statuses[o.Status]++
			times = append(times, o.Total)
			if o.Total > st.Max {
				st.Max = o.Total
			}
		}
		sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
		st.Median = medianDuration(times)
		stats = append(stats, st)
	}
	return stats
}

// medianDuration expects a sorted slice.
func medianDuration(sorted []time.Duration) time.Duration {
	n := len(sorted)
	switch {
	case n == 0:
		return 0
	case n%2 == 1:
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// WriteStats prints the per-tier table.
func WriteStats(w io.Writer, outs []Outcome) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TIER\tREQUESTS\tMEDIAN\tSLOWEST\tOUTCOMES")
	for _, st := range Stats(outs) {
		var parts []string
		for _, s := range []Status{StatusOK, StatusRefused, StatusDeclined, StatusNotUnderstood, StatusFailed} {
			if n := st.Statuses[s]; n > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", n, s))
			}
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n", st.Tier, st.Count, FormatDuration(st.Median), FormatDuration(st.Max), strings.Join(parts, ", "))
	}
	tw.Flush()
}

// FormatDuration prints a latency with a sensible unit: microseconds for rules,
// milliseconds or seconds for the model.
func FormatDuration(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%d µs", d.Microseconds())
	case d < time.Second:
		return fmt.Sprintf("%.1f ms", float64(d)/float64(time.Millisecond))
	}
	return fmt.Sprintf("%.2f s", d.Seconds())
}

// LogEntry is one line of the JSONL request log.
type LogEntry struct {
	Time         string  `json:"time"`
	Text         string  `json:"text"`
	Tier         int     `json:"tier"` // 0 rules, 1 model, -1 none
	Rule         string  `json:"rule,omitempty"`
	Intent       string  `json:"intent,omitempty"`
	Status       string  `json:"status"`
	Attempts     int     `json:"attempts,omitempty"`
	UnderstandMS float64 `json:"understand_ms"`
	ExecuteMS    float64 `json:"execute_ms"`
	TotalMS      float64 `json:"total_ms"`
	Fallback     string  `json:"fallback,omitempty"`
	Error        string  `json:"error,omitempty"`
}

// NewLogEntry converts an outcome; at is when the request happened.
func NewLogEntry(at time.Time, o Outcome) LogEntry {
	ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	e := LogEntry{
		Time: at.Format(time.RFC3339), Text: o.Text, Tier: int(o.Tier), Rule: o.Rule,
		Intent: string(o.Command.Intent), Status: string(o.Status), Attempts: o.Attempts,
		UnderstandMS: ms(o.Understand), ExecuteMS: ms(o.Execute), TotalMS: ms(o.Total), Fallback: o.Fallback,
	}
	if o.Err != nil {
		e.Error = o.Err.Error()
	}
	return e
}

// WriteLog appends one JSON line.
func WriteLog(w io.Writer, at time.Time, o Outcome) error {
	b, err := json.Marshal(NewLogEntry(at, o))
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", b)
	return err
}
