package router

import (
	"fmt"
	"io"
)

// WriteGateReport measures the complexity gate on the labelled cases and the
// held-out cases and prints what it got wrong. It needs no model, no queue and
// no network: the gate is a handful of string checks.
func WriteGateReport(w io.Writer) {
	fmt.Fprintln(w, "Complexity gate: which messages would be deferred as hard.")
	fmt.Fprintln(w, "A false defer makes someone wait for a request that could have been answered now.")
	fmt.Fprintln(w, "A missed hard request goes to the local model instead; if that fails to produce")
	fmt.Fprintln(w, "a valid command it is deferred anyway (the output-validity check).")
	fmt.Fprintln(w)
	gateSet(w, "tuned-on cases (the gate was adjusted until these passed: optimistic)", Cases)
	gateSet(w, "held-out cases (written after tuning, never used to tune: the honest estimate)", HeldOut)
	fmt.Fprintln(w, "All cases were written by the project author, not collected from real crews.")
}

func gateSet(w io.Writer, name string, cases []GateCase) {
	var simple, hard, falseDefer, missed int
	var fdList, missList []string
	for _, c := range cases {
		got := Assess(c.Text).Hard
		if c.Hard {
			hard++
			if !got {
				missed++
				missList = append(missList, c.Text)
			}
		} else {
			simple++
			if got {
				falseDefer++
				fdList = append(fdList, c.Text)
			}
		}
	}
	fmt.Fprintf(w, "%s\n", name)
	fmt.Fprintf(w, "  %d cases: %d should be answered now, %d should be deferred\n", len(cases), simple, hard)
	fmt.Fprintf(w, "  false defers: %d of %d simple   missed hard: %d of %d hard (caught %d)\n", falseDefer, simple, missed, hard, hard-missed)
	for _, s := range fdList {
		fmt.Fprintf(w, "    false defer: %q\n", s)
	}
	for _, s := range missList {
		fmt.Fprintf(w, "    missed:      %q\n", s)
	}
	fmt.Fprintln(w)
}
