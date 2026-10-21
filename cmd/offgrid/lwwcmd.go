package main

import (
	"fmt"
	"io"
	"time"

	"offgrid/internal/crdt"
	"offgrid/internal/hlc"
)

const lwwHelp = `offgrid lww demo explains the last-writer-wins map with a small SIMULATION.

  offgrid lww demo

Alice and Bob edit the same task while apart, then meet. The demo uses fake
clocks and no files or network; the timestamps come from internal/hlc and the
merge is the real internal/crdt.Merge.`

func cmdLWW(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprintln(stderr, lwwHelp)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	if args[0] != "demo" || len(args) > 1 {
		fmt.Fprintf(stderr, "error: usage: offgrid lww demo\n")
		return 2
	}
	runLWWDemo(stdout)
	return 0
}

func runLWWDemo(w io.Writer) {
	start := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	hm := func(d time.Duration) string { return start.Add(d).Format("15:04:05") }

	// each person has a clock, a copy of the data, and a name
	type person struct {
		name string
		now  time.Time
		clk  *hlc.Clock
		data crdt.Map
	}
	newPerson := func(name string) *person {
		p := &person{name: name, now: start}
		p.clk = hlc.New(hlc.Config{Now: func() time.Time { return p.now }})
		return p
	}
	set := func(p *person, at time.Duration, id, field, value string) {
		p.now = start.Add(at)
		st := p.clk.Now()
		p.data = p.data.Put(crdt.TaskKey(id, field), crdt.Entry{Value: value, Stamp: st, Node: p.name})
		fmt.Fprintf(w, "   %-5s at %s sets %s = %-11s stamp %s\n", p.name, hm(at), field, value, st)
	}
	show := func(label string, m crdt.Map) {
		f := crdt.TaskFields(m)["T1"]
		fmt.Fprintf(w, "   %-22s priority=%-6s status=%s\n", label, f["priority"], f["status"])
	}
	// both start from the same shared copy
	shared := crdt.Map{}.
		Put(crdt.TaskKey("T1", "priority"), crdt.Entry{Value: "medium", Stamp: hlc.Timestamp{Wall: start.Add(-time.Hour).UnixNano()}, Node: "office"}).
		Put(crdt.TaskKey("T1", "status"), crdt.Entry{Value: "open", Stamp: hlc.Timestamp{Wall: start.Add(-time.Hour).UnixNano()}, Node: "office"})
	fresh := func() (*person, *person) {
		a, b := newPerson("alice"), newPerson("bob")
		a.data, b.data = shared, shared
		return a, b
	}
	meet := func(a, b *person) {
		ab, ba := crdt.Merge(a.data, b.data), crdt.Merge(b.data, a.data)
		show("Alice after merging:", ab)
		show("Bob after merging:", ba)
		if crdt.Equal(ab, ba) {
			fmt.Fprintln(w, "   -> both ended with the same data, and nobody had to decide.")
		} else {
			fmt.Fprintln(w, "   -> THEY DIFFER (this must never happen)")
		}
	}

	fmt.Fprintln(w, "SIMULATION: fake clocks, no files, no network. Task T1 starts as priority=medium, status=open.")

	fmt.Fprintln(w, "\n1. Different fields: both edits survive")
	a, b := fresh()
	set(a, 2*time.Minute, "T1", "priority", "high")
	set(b, 5*time.Minute, "T1", "status", "in_progress")
	meet(a, b)
	fmt.Fprintln(w, "   (a whole-task rule would have kept only one of them.)")

	fmt.Fprintln(w, "\n2. Same field: the later edit wins, on both sides")
	a, b = fresh()
	set(a, 2*time.Minute, "T1", "priority", "low")
	set(b, 5*time.Minute, "T1", "priority", "high")
	meet(a, b)
	fmt.Fprintln(w, "   Alice's edit was not 'wrong'. It was just overwritten, as if Bob had typed after her.")

	fmt.Fprintln(w, "\n3. Exactly the same moment: the node name breaks the tie")
	a, b = fresh()
	set(a, 3*time.Minute, "T1", "priority", "low")
	set(b, 3*time.Minute, "T1", "priority", "high")
	meet(a, b)
	fmt.Fprintln(w, "   Arbitrary (\"bob\" sorts after \"alice\"), but identical on every replica, which is all that matters.")
}
