package perf

import (
	"fmt"
	"html"
	"io"
	"math"
	"strings"
)

// Palette: the validated reference palette's first two categorical slots
// (blue, orange) on its light surface; text in text tokens, never series colour.
const (
	cSurface = "#fcfcfb"
	cInk     = "#0b0b0b"
	cInk2    = "#52514e"
	cGrid    = "#e4e3df"
	cBlue    = "#2a78d6"
	cOrange  = "#eb6834"
)

type bar struct {
	label string
	value float64
	text  string
	color string
}

// WriteSVG draws the report as one 1200x675 picture for a post: how long a new
// replica takes to catch up, how fast the queue drains, and how much memory it
// all needs, with the machine named at the top. Values are printed on the bars,
// because a picture on a timeline cannot be hovered.
func WriteSVG(w io.Writer, r Report) error {
	var b strings.Builder
	hw := r.Meta.Hardware
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1200 675" width="1200" height="675" font-family="Helvetica, Arial, sans-serif">`+"\n")
	fmt.Fprintf(&b, `<rect width="1200" height="675" fill="%s"/>`+"\n", cSurface)
	text(&b, 50, 62, 34, 700, cInk, "What Offgrid costs on this machine")
	text(&b, 50, 96, 20, 400, cInk2, esc(fmt.Sprintf("%s, %d threads, %.1f GiB RAM, %s. No model involved.", orNA(hw.CPU), hw.Threads, hw.MemGiB, orNA(hw.OS))))

	// panel 1: catch-up time by log size, in process vs HTTP
	var p1 []bar
	for _, s := range r.Sync {
		switch {
		case s.Mode == "new replica, in process":
			p1 = append(p1, bar{fmt.Sprintf("%s ops, in process", commas(s.Ops)), s.Seconds.Median, dur(s.Seconds.Median), cBlue})
		case s.Mode == "new replica, HTTP on localhost":
			p1 = append(p1, bar{fmt.Sprintf("%s ops, over HTTP", commas(s.Ops)), s.Seconds.Median, dur(s.Seconds.Median), cOrange})
		}
	}
	panel(&b, 50, 140, 700, "A new replica catches up (time, lower is better)", p1, 300)

	// panel 2: queue drain
	var p2 []bar
	for _, q := range r.Queue {
		p2 = append(p2, bar{fmt.Sprintf("%s jobs, %d at a time", commas(q.Jobs), q.Concurrency), q.DrainPerSec, rate(q.DrainPerSec) + " jobs/s", cBlue})
	}
	panel(&b, 800, 140, 350, "Queue drain (jobs per second)", p2, 190)

	// panel 3: peak memory of the heaviest row of each kind
	var p3 []bar
	if m, ok := heaviestStore(r, "rebuild"); ok {
		p3 = append(p3, bar{fmt.Sprintf("rebuild, %s ops", commas(m.Ops)), m.Mem.RSSMiB, fmt.Sprintf("%.0f MiB", m.Mem.RSSMiB), cBlue})
	}
	if m, ok := heaviestSync(r); ok {
		p3 = append(p3, bar{fmt.Sprintf("sync over HTTP, %s ops", commas(m.Ops)), m.Mem.RSSMiB, fmt.Sprintf("%.0f MiB", m.Mem.RSSMiB), cBlue})
	}
	if len(r.Queue) > 0 {
		q := r.Queue[len(r.Queue)-1]
		p3 = append(p3, bar{fmt.Sprintf("queue, %s jobs", commas(q.Jobs)), q.Mem.RSSMiB, fmt.Sprintf("%.0f MiB", q.Mem.RSSMiB), cBlue})
	}
	panel(&b, 50, 430, 700, "Peak memory of the whole process (MiB)", p3, 300)

	// legend for panel 1
	legend(&b, 800, 460, cBlue, "in process (no network)")
	legend(&b, 800, 490, cOrange, "HTTP on localhost, HMAC-signed")
	text(&b, 800, 540, 17, 400, cInk2, esc(fmt.Sprintf("Median of %d runs, seed %d, %s.", r.Meta.Repeats, r.Meta.Seed, r.Meta.Go)))
	text(&b, 800, 566, 17, 400, cInk2, esc(fmt.Sprintf("Files on %s. %s.", orNA(r.Meta.DirFS), r.Meta.Date)))
	text(&b, 800, 592, 17, 400, cInk2, "Network is localhost, not WiFi.")
	text(&b, 50, 645, 17, 400, cInk2, "Reproduce it yourself: make bench")
	b.WriteString("</svg>\n")
	_, err := io.WriteString(w, b.String())
	return err
}

func heaviestStore(r Report, prefix string) (StoreRow, bool) {
	var best StoreRow
	ok := false
	for _, s := range r.Store {
		if strings.HasPrefix(s.Name, prefix) && (!ok || s.Ops > best.Ops) {
			best, ok = s, true
		}
	}
	return best, ok
}

func heaviestSync(r Report) (SyncRow, bool) {
	var best SyncRow
	ok := false
	for _, s := range r.Sync {
		if s.Mode == "new replica, HTTP on localhost" && (!ok || s.Ops > best.Ops) {
			best, ok = s, true
		}
	}
	return best, ok
}

// panel draws one horizontal bar chart: a title, then one row per bar, bars
// anchored on a common baseline, value printed after each bar.
func panel(b *strings.Builder, x, y, w int, title string, bars []bar, labelW int) {
	text(b, x, y+20, 21, 700, cInk, esc(title))
	if len(bars) == 0 {
		text(b, x, y+60, 18, 400, cInk2, "no data")
		return
	}
	maxV := 0.0
	for _, br := range bars {
		if br.value > maxV {
			maxV = br.value
		}
	}
	if maxV <= 0 {
		maxV = 1
	}
	rowH := 38
	if len(bars) > 7 {
		rowH = 32
	}
	x0 := x + labelW
	span := float64(w - labelW - 110)
	fmt.Fprintf(b, `<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="%s" stroke-width="1"/>`+"\n", x0, y+38, x0, y+44+rowH*len(bars), cGrid)
	for i, br := range bars {
		cy := y + 44 + i*rowH
		bw := span * br.value / maxV
		if bw < 3 {
			bw = 3
		}
		text(b, x0-10, cy+rowH/2+2, 16, 400, cInk2, esc(br.label), "end")
		fmt.Fprintf(b, `<rect x="%d" y="%d" width="%.1f" height="%d" rx="4" fill="%s"/>`+"\n", x0, cy+4, bw, rowH-12, br.color)
		text(b, x0+int(bw)+8, cy+rowH/2+2, 17, 700, cInk, esc(br.text))
	}
}

func legend(b *strings.Builder, x, y int, color, label string) {
	fmt.Fprintf(b, `<rect x="%d" y="%d" width="16" height="16" rx="4" fill="%s"/>`+"\n", x, y-13, color)
	text(b, x+26, y, 17, 400, cInk, esc(label))
}

func text(b *strings.Builder, x, y, size, weight int, fill, s string, anchor ...string) {
	a := ""
	if len(anchor) > 0 {
		a = fmt.Sprintf(` text-anchor="%s"`, anchor[0])
	}
	fmt.Fprintf(b, `<text x="%d" y="%d" font-size="%d" font-weight="%d" fill="%s"%s>%s</text>`+"\n", x, y, size, weight, fill, a, s)
}

func esc(s string) string { return html.EscapeString(s) }

// dur prints seconds the way a person reads them.
func dur(s float64) string {
	if math.IsNaN(s) || math.IsInf(s, 0) || s < 0 {
		return "-"
	}
	switch {
	case s >= 10:
		return fmt.Sprintf("%.1f s", s)
	case s >= 1:
		return fmt.Sprintf("%.2f s", s)
	case s >= 0.001:
		return fmt.Sprintf("%.0f ms", s*1000)
	}
	return fmt.Sprintf("%.0f µs", s*1e6)
}
