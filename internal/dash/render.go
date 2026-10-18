// Package dash draws Offgrid's one-screen view of the whole system:
// connection state, the queue, and which tier handled each message.
//
// Render is a pure function from a View to a string, so the screen can be
// tested exactly and a frame can be pasted into a bug report. Live (live.go)
// only repaints it.
//
// It uses plain ANSI escape codes and the standard library, not a terminal UI
// framework, so it adds no dependency to a program that is meant to be one
// small static binary.
package dash

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"offgrid/internal/jobqueue"
	"offgrid/internal/obs"
)

// View is everything one frame shows.
type View struct {
	Now       time.Time
	Snaps     []obs.Snapshot // one per publishing process
	Queue     *jobqueue.Stats
	QueuePath string
	QueueErr  string   // why the queue could not be read, if so
	Problems  []string // unreadable snapshot files
}

// Options control drawing only.
type Options struct {
	Width int  // total columns, default 80 (minimum 64)
	Color bool // use ANSI colours
}

var ansi = regexp.MustCompile("\x1b\\[[0-9;?]*[A-Za-z]")

func visLen(s string) int { return utf8.RuneCountInString(ansi.ReplaceAllString(s, "")) }

type palette struct{ on bool }

func (p palette) wrap(code, s string) string {
	if !p.on || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}
func (p palette) green(s string) string  { return p.wrap("32", s) }
func (p palette) yellow(s string) string { return p.wrap("33", s) }
func (p palette) red(s string) string    { return p.wrap("31", s) }
func (p palette) dim(s string) string    { return p.wrap("2", s) }
func (p palette) bold(s string) string   { return p.wrap("1", s) }
func (p palette) cyan(s string) string   { return p.wrap("36", s) }

// Render draws one frame. Lines never exceed the width, and the frame has no
// trailing newline.
func Render(v View, o Options) string {
	w := o.Width
	if w == 0 {
		w = 80
	}
	w = max(w, 64)
	p := palette{o.Color}
	var out []string
	inner := w - 4

	panel := func(title string, lines []string) {
		head := "┌─ " + title + " "
		out = append(out, p.dim(head+strings.Repeat("─", max(w-1-utf8.RuneCountInString(head), 0))+"┐"))
		for _, l := range lines {
			if visLen(l) > inner {
				l = clip(l, inner)
			}
			out = append(out, p.dim("│")+" "+l+strings.Repeat(" ", inner-visLen(l))+" "+p.dim("│"))
		}
		out = append(out, p.dim("└"+strings.Repeat("─", w-2)+"┘"))
	}

	snaps := append([]obs.Snapshot(nil), v.Snaps...)
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].Proc < snaps[j].Proc })

	// ---- header
	title := p.bold("OFFGRID") + "  " + v.Now.Format("Mon 02 Jan 15:04:05")
	var procs []string
	for _, s := range snaps {
		procs = append(procs, procLabel(s, v.Now, p))
	}
	if len(procs) == 0 {
		procs = []string{p.yellow("no process is publishing")}
	}
	out = append(out, title+strings.Repeat(" ", max(w-visLen(title)-visLen(strings.Join(procs, "  ")), 2))+strings.Join(procs, "  "))

	m := merge(snaps, v.Now)

	panel("CONNECTION", connLines(m, v.Now, p, inner))
	panel("QUEUE", queueLines(v, m, p, inner))
	panel("TIERS  (who handled each message: latest agent run)", tierLines(m, p, inner))
	panel("RECENT", recentLines(m, p))

	var notes []string
	for _, pr := range v.Problems {
		notes = append(notes, p.yellow("! "+pr))
	}
	for _, n := range notes {
		out = append(out, clip(n, w))
	}
	return strings.Join(out, "\n")
}

func clip(s string, n int) string {
	if visLen(s) <= n {
		return s
	}
	// Strip colour when cutting: a half-cut escape would bleed into the frame.
	r := []rune(ansi.ReplaceAllString(s, ""))
	return string(r[:max(n-1, 0)]) + "…"
}

func procLabel(s obs.Snapshot, now time.Time, p palette) string {
	switch {
	case s.Fresh(now):
		return p.green("● ") + s.Proc
	case s.Stopped:
		return p.dim("○ " + s.Proc + " stopped")
	}
	return p.yellow("○ " + s.Proc + " silent " + dur(s.Age(now)))
}

// merged is the whole system's state as seen across process snapshots.
type merged struct {
	net      string
	netSince time.Time
	netStale bool      // the process that reported it is not publishing now
	netStop  bool      // ...because it exited cleanly
	netAt    time.Time // when that process last published
	netFrom  string    // which process
	probe    *obs.Probe
	probes   struct{ ok, fail int }
	probeMS  []int64
	flips    []obs.Flip
	jobs     map[string]obs.JobCounts
	jobLat   map[string]obs.Latency
	tiers    map[string]int
	tierLat  map[string]obs.Latency
	feed     []feedItem
	haveNet  bool
}

type feedItem struct {
	obs.FeedLine
	proc string
}

func merge(snaps []obs.Snapshot, now time.Time) merged {
	m := merged{jobs: map[string]obs.JobCounts{}, jobLat: map[string]obs.Latency{}, tiers: map[string]int{}, tierLat: map[string]obs.Latency{}}
	// Connection state: believe the freshest process that is alive. If none
	// is alive, show the last thing known and say so.
	var best *obs.Snapshot
	for i := range snaps {
		s := &snaps[i]
		if s.Net == "" {
			continue
		}
		switch {
		case best == nil:
			best = s
		case s.Fresh(now) != best.Fresh(now):
			if s.Fresh(now) {
				best = s
			}
		case (s.LastProbe != nil) != (best.LastProbe != nil):
			// A process that really probes beats one whose state is fixed by
			// a flag (-net offline): the flag is a pretend, the probe is not.
			if s.LastProbe != nil {
				best = s
			}
		case s.At.After(best.At):
			best = s
		}
	}
	if best != nil {
		m.haveNet = true
		m.net, m.netSince, m.netFrom = best.Net, best.NetSince, best.Proc
		m.netStale, m.netStop, m.netAt = !best.Fresh(now), best.Stopped, best.At
		m.probe, m.probeMS, m.flips = best.LastProbe, best.ProbeMS, best.Flips
		m.probes.ok, m.probes.fail = best.Probes.OK, best.Probes.Fail
	}
	for _, s := range snaps {
		for k, c := range s.Jobs {
			t := m.jobs[k]
			t.Claimed += c.Claimed
			t.Done += c.Done
			t.Retried += c.Retried
			t.Dead += c.Dead
			t.Released += c.Released
			t.Lost += c.Lost
			m.jobs[k] = t
		}
		for k, l := range s.JobLatency { // one worker per kind in practice; keep the larger sample
			if l.N > m.jobLat[k].N {
				m.jobLat[k] = l
			}
		}
		for k, n := range s.Tiers {
			m.tiers[k] += n
		}
		for k, l := range s.TierLat {
			if l.N > m.tierLat[k].N {
				m.tierLat[k] = l
			}
		}
		for _, f := range s.Feed {
			m.feed = append(m.feed, feedItem{f, s.Proc})
		}
	}
	sort.SliceStable(m.feed, func(i, j int) bool { return m.feed[i].At.After(m.feed[j].At) })
	return m
}

func dur(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%dµs", d.Microseconds())
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func connLines(m merged, now time.Time, p palette, inner int) []string {
	if !m.haveNet {
		return []string{p.dim("no connectivity reading yet (start `offgrid drain` or `agent -route`)")}
	}
	dot, state := "●", strings.ToUpper(m.net)
	switch strings.ToLower(m.net) {
	case "online":
		state = p.green(dot + " " + state)
	case "degraded":
		state = p.yellow(dot + " " + state)
	case "offline":
		state = p.red(dot + " " + state)
	default:
		state = p.dim(dot + " " + state)
	}
	l1 := state
	switch {
	case m.netStale:
		// Never present old news as live: say who reported it and when it went quiet.
		why := "stopped"
		if !m.netStop {
			why = "went silent"
		}
		l1 += "  " + p.yellow("last known: "+m.netFrom+" "+why+" "+dur(max(now.Sub(m.netAt), 0))+" ago")
	case !m.netSince.IsZero():
		l1 += "  for " + dur(max(now.Sub(m.netSince), 0))
	}
	lines := []string{l1}
	if m.probe != nil {
		res := p.green("ok")
		if !m.probe.OK {
			res = p.red("failed")
		}
		l := fmt.Sprintf("last probe %s in %s   (%d ok, %d failed so far)", res, dur(m.probe.Latency), m.probes.ok, m.probes.fail)
		if !m.probe.OK && m.probe.Err != "" {
			l += p.dim("  " + oneLine(m.probe.Err, 40))
		}
		lines = append(lines, l)
	} else {
		lines = append(lines, p.dim("not probed (the state is fixed by a flag, or no probe has finished)"))
	}
	if len(m.probeMS) > 0 {
		lines = append(lines, "probes "+spark(m.probeMS, inner-7, p))
	}
	for i := len(m.flips) - 1; i >= 0 && len(m.flips)-i <= 3; i-- {
		f := m.flips[i]
		l := fmt.Sprintf("%s  %s → %s", f.At.Format("15:04:05"), f.From, f.To)
		if f.Reason != "" {
			l += p.dim("  " + oneLine(f.Reason, 36))
		}
		lines = append(lines, l)
	}
	return lines
}

func queueLines(v View, m merged, p palette, inner int) []string {
	if v.Queue == nil {
		msg := "queue not read"
		if v.QueueErr != "" {
			msg = "queue unreadable: " + v.QueueErr
		}
		return []string{p.yellow(msg)}
	}
	s := *v.Queue
	lines := []string{fmt.Sprintf("%s %d   %s %d   %s %d   %s %d",
		p.yellow("queued"), s.Queued, p.cyan("running"), s.Running, p.green("done"), s.Done, p.red("dead"), s.Dead)}
	if total := s.Total(); total > 0 {
		lines = append(lines, bar(s, total, inner, p))
	}
	switch {
	case s.Queued > 0 && !s.NextReady.IsZero():
		if d := s.NextReady.Sub(v.Now); d > 0 {
			lines = append(lines, p.dim("next job ready in "+dur(d)))
		} else {
			lines = append(lines, p.dim("next job is ready now"))
		}
	case s.Queued == 0 && s.Running == 0:
		lines = append(lines, p.dim("nothing waiting"))
	}
	kinds := make([]string, 0, len(m.jobs))
	for k := range m.jobs {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	if len(kinds) > 0 {
		lines = append(lines, p.dim(fmt.Sprintf("%-12s %7s %5s %5s %5s %5s %s", "this worker", "started", "done", "retry", "dead", "back", "latency p50 / p95")))
	}
	for _, k := range kinds {
		c, l := m.jobs[k], m.jobLat[k]
		lat := "-"
		if l.N > 0 {
			lat = dur(l.P50) + " / " + dur(l.P95)
		}
		lines = append(lines, fmt.Sprintf("%-12s %7d %5d %5d %5d %5d %s", k, c.Claimed, c.Done, c.Retried, c.Dead, c.Released+c.Lost, lat))
	}
	return lines
}

func bar(s jobqueue.Stats, total, width int, p palette) string {
	width = max(width-2, 10)
	cells := func(n int) int { return n * width / total }
	d, r, q, x := cells(s.Done), cells(s.Running), cells(s.Queued), cells(s.Dead)
	// give rounding slack to the largest non-empty part so the bar fills up
	if used := d + r + q + x; used < width {
		switch {
		case s.Done >= s.Queued && s.Done >= s.Running && s.Done >= s.Dead:
			d += width - used
		case s.Queued >= s.Running && s.Queued >= s.Dead:
			q += width - used
		case s.Running >= s.Dead:
			r += width - used
		default:
			x += width - used
		}
	}
	return "[" + p.green(strings.Repeat("█", d)) + p.cyan(strings.Repeat("█", r)) + p.yellow(strings.Repeat("░", q)) + p.red(strings.Repeat("█", x)) + "]"
}

var tierOrder = []string{obs.TierRules, obs.TierModel, obs.TierCloud, obs.TierDeferred, obs.TierStatus, obs.TierUnread, obs.TierFailed}

func tierLines(m merged, p palette, inner int) []string {
	total := 0
	for _, n := range m.tiers {
		total += n
	}
	if total == 0 {
		return []string{p.dim("no messages routed yet (try: offgrid agent -route ...)")}
	}
	var lines []string
	barW := max(inner-38, 8)
	for _, t := range tierOrder {
		n := m.tiers[t]
		if n == 0 {
			continue
		}
		l := m.tierLat[t]
		lat := ""
		if l.N > 0 {
			lat = fmt.Sprintf("p50 %s  p95 %s", dur(l.P50), dur(l.P95))
		}
		fill := max(n*barW/total, 1)
		color := p.green
		switch t {
		case obs.TierDeferred:
			color = p.yellow
		case obs.TierFailed, obs.TierUnread:
			color = p.red
		}
		lines = append(lines, fmt.Sprintf("%-9s %4d  %s %s", t, n, color(strings.Repeat("█", fill))+strings.Repeat(" ", barW-fill), p.dim(lat)))
	}
	return lines
}

func recentLines(m merged, p palette) []string {
	if len(m.feed) == 0 {
		return []string{p.dim("nothing has happened yet")}
	}
	var lines []string
	for i, f := range m.feed {
		if i == 8 {
			break
		}
		txt := f.At.Format("15:04:05") + "  " + f.Text
		switch f.Kind {
		case "ok":
			txt = p.green(txt)
		case "warn":
			txt = p.yellow(txt)
		case "bad":
			txt = p.red(txt)
		}
		lines = append(lines, txt)
	}
	return lines
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

var sparkRunes = []rune("▁▂▃▄▅▆▇█")

// spark draws probe latencies as a one-line chart of at most width columns
// (the most recent values): bars scaled to the slowest shown, and a red ✗ for
// a failed probe (a negative value).
func spark(vals []int64, width int, p palette) string {
	if width < 1 || len(vals) == 0 {
		return ""
	}
	if len(vals) > width {
		vals = vals[len(vals)-width:]
	}
	var hi int64 = 1
	for _, v := range vals {
		hi = max(hi, v)
	}
	var b strings.Builder
	for _, v := range vals {
		if v < 0 {
			b.WriteString(p.red("✗"))
			continue
		}
		i := int(v * int64(len(sparkRunes)-1) / hi)
		b.WriteString(p.cyan(string(sparkRunes[min(max(i, 0), len(sparkRunes)-1)])))
	}
	return b.String()
}
