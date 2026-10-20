package main

import (
	"flag"
	"fmt"
	"io"
	"time"

	"offgrid/internal/hlc"
)

const clockHelp = `offgrid clock explains the hybrid logical clock with a small SIMULATION.

  offgrid clock demo [-skew 10m]

Two people edit the same task. Biruk's phone clock is -skew behind real time.
The demo uses fake clocks (nothing here reads your real clock or network), but
the timestamps come from the real internal/hlc package.`

func cmdClock(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprintln(stderr, clockHelp)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	if args[0] != "demo" {
		fmt.Fprintf(stderr, "error: unknown clock command %q (use: demo)\n", args[0])
		return 2
	}
	fs := flag.NewFlagSet("clock demo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	skew := fs.Duration("skew", 10*time.Minute, "how far BEHIND real time Biruk's clock is (negative: ahead)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "error: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *skew > 24*time.Hour || *skew < -24*time.Hour {
		fmt.Fprintln(stderr, "error: -skew must be within 24h either way")
		return 2
	}
	runClockDemo(stdout, *skew)
	return 0
}

// manual is a physical clock the demo moves by hand.
type manual struct{ t time.Time }

func (m *manual) now() time.Time { return m.t }

func runClockDemo(w io.Writer, skew time.Duration) {
	start := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	hm := func(t time.Time) string { return t.Format("15:04:05") }

	fmt.Fprintf(w, "SIMULATION: fake clocks, no real time or network. Biruk's phone is %s behind.\n\n", skew)

	// ---- 1. a slow clock, and an edit that happens later in reality
	a := &manual{t: start}
	b := &manual{t: start.Add(-skew)}
	ca := hlc.New(hlc.Config{Now: a.now})
	cb := hlc.New(hlc.Config{Now: b.now})

	fmt.Fprintln(w, "1. Mercy edits, it syncs to Biruk, then Biruk edits later")
	mercyWall, mercyHLC := a.t, ca.Now()
	fmt.Fprintf(w, "   real %s  Mercy sets priority=low     wall clock %s   HLC %s\n", hm(start), hm(mercyWall), mercyHLC)

	a.t, b.t = a.t.Add(30*time.Second), b.t.Add(30*time.Second)
	recv, err := cb.Update(mercyHLC)
	if err != nil {
		fmt.Fprintf(w, "   Biruk refuses Mercy's timestamp: %v\n", err)
		return
	}
	fmt.Fprintf(w, "   real %s  Mercy's edit reaches Biruk  (Biruk folds her timestamp in: HLC %s)\n", hm(a.t), recv)

	a.t, b.t = a.t.Add(30*time.Second), b.t.Add(30*time.Second)
	birukWall, birukHLC := b.t, cb.Now()
	fmt.Fprintf(w, "   real %s  Biruk sets priority=high    wall clock %s   HLC %s\n\n", hm(a.t), hm(birukWall), birukHLC)

	wallBirukWins := birukWall.After(mercyWall)
	hlcBirukWins := birukHLC.After(mercyHLC)
	fmt.Fprintf(w, "   Last writer wins, ordering by the wall clock:  %s\n", verdict(wallBirukWins))
	fmt.Fprintf(w, "   Last writer wins, ordering by the HLC:         %s\n", verdict(hlcBirukWins))
	switch {
	case hlcBirukWins && !wallBirukWins:
		fmt.Fprintln(w, "   -> Biruk really edited last. The wall clock throws his edit away; the HLC keeps it.")
	case hlcBirukWins && wallBirukWins:
		fmt.Fprintln(w, "   -> Both agree this time: Biruk's clock is not behind enough to matter. Try -skew 10m.")
	default:
		fmt.Fprintln(w, "   -> unexpected: the HLC ordered Biruk's later edit first")
	}

	// ---- 2. the clock goes backwards
	fmt.Fprintln(w, "\n2. A phone's clock is set backwards (a dead battery, a wrong time zone, an automatic correction)")
	c := &manual{t: start}
	cc := hlc.New(hlc.Config{Now: c.now})
	prev := hlc.Timestamp{}
	steps := []struct {
		label string
		set   time.Duration
	}{{"clock reads 10:00:00", 0}, {"clock reads 10:00:05", 5 * time.Second}, {"clock jumps BACK to 09:00:00", -time.Hour}, {"clock still 09:00:00", -time.Hour}, {"clock reads 10:00:10", 10 * time.Second}, {"clock reads 10:00:11", 11 * time.Second}}
	for _, s := range steps {
		c.t = start.Add(s.set)
		ts := cc.Now()
		mark := "ok"
		if !ts.After(prev) {
			mark = "WENT BACKWARDS"
		}
		fmt.Fprintf(w, "   %-30s event stamped %s   %s\n", s.label, ts, mark)
		prev = ts
	}

	// ---- 3. a clock far in the future
	fmt.Fprintln(w, "\n3. A peer's clock is a year ahead")
	d := &manual{t: start}
	cd := hlc.New(hlc.Config{Now: d.now})
	far := hlc.Timestamp{Wall: start.AddDate(1, 0, 0).UnixNano()}
	_, err = cd.Update(far)
	if err != nil {
		fmt.Fprintf(w, "   refused: %v\n", err)
		fmt.Fprint(w, "   this node's clock is unchanged, so it is not dragged into next year.\n")
		fmt.Fprintln(w, "   Cost: edits made here afterwards are ordered BEFORE that peer's, so on a conflict the peer's value wins.")
		fmt.Fprintln(w, "   The data itself is not dropped; a person should fix that phone's clock.")
	} else {
		fmt.Fprintln(w, "   accepted: nothing limited the drift")
	}
}

func verdict(birukWins bool) string {
	if birukWins {
		return "Biruk's edit wins (correct)"
	}
	return "Mercy's older edit wins (WRONG: Biruk's later edit is lost)"
}
