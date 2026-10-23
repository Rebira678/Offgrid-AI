package main

import (
	"fmt"
	"io"
	"strings"

	"offgrid/internal/crdt"
	"offgrid/internal/hlc"
)

const orsetHelp = `offgrid orset demo explains the add-wins set with a small SIMULATION.

  offgrid orset demo

Lidia deletes a task while Beka edits it, then they meet. Fake clocks, no files,
no network; the merge is the real internal/crdt.MergeSet.`

func cmdORSet(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprintln(stderr, orsetHelp)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	if args[0] != "demo" || len(args) > 1 {
		fmt.Fprintln(stderr, "error: usage: offgrid orset demo")
		return 2
	}
	runORSetDemo(stdout)
	return 0
}

func runORSetDemo(w io.Writer) {
	tag := func(node string, sec int64) crdt.Tag {
		return crdt.Tag{Node: node, Stamp: hlc.Timestamp{Wall: (1_792_000_000 + sec) * 1_000_000_000}}
	}
	show := func(who string, s crdt.Set) {
		els := s.Elements()
		if len(els) == 0 {
			fmt.Fprintf(w, "   %-22s (empty)\n", who)
			return
		}
		fmt.Fprintf(w, "   %-22s %s\n", who, strings.Join(els, ", "))
	}
	both := func(a, b crdt.Set) {
		ab, ba := crdt.MergeSet(a, b), crdt.MergeSet(b, a)
		show("Lidia after merging:", ab)
		show("Beka after merging:", ba)
		if crdt.EqualSet(ab, ba) {
			fmt.Fprintln(w, "   -> both ended identical, and nobody had to decide.")
		} else {
			fmt.Fprintln(w, "   -> THEY DIFFER (this must never happen)")
		}
	}

	fmt.Fprintln(w, "SIMULATION: fake clocks, no files, no network. The set holds task IDs; an edit counts as touching the task.")
	office := crdt.Set{}.Add("T1", tag("office", 0))
	fmt.Fprintf(w, "\nStart: both phones have T1 (added by the office, tag %s)\n", tag("office", 0))

	fmt.Fprintln(w, "\n1. Lidia deletes T1. Beka does nothing. The delete holds.")
	lidia, seen := office.Remove("T1")
	fmt.Fprintf(w, "   Lidia deletes T1: she can see %d tag, so she writes %d tombstone\n", len(seen), len(seen))
	both(lidia, office)

	fmt.Fprintln(w, "\n2. Lidia deletes T1 while Beka edits it. The edit survives.")
	lidia, seen = office.Remove("T1")
	fmt.Fprintf(w, "   Lidia deletes T1 (tombstones only the tag she saw: %s)\n", seen[0])
	edit := tag("beka", 30)
	beka := office.Add("T1", edit)
	fmt.Fprintf(w, "   Beka edits T1 at the same time: a new add, tag %s\n", edit)
	both(lidia, beka)
	fmt.Fprintln(w, "   Lidia's delete only covered what she had seen. Beka's edit was new to it. That is 'add wins'.")

	fmt.Fprintln(w, "\n3. The delete arrives BEFORE the thing it deletes. It still works.")
	late := tag("beka", 40)
	early := crdt.Set{}.RemoveTags(late)
	fmt.Fprintf(w, "   a tombstone for %s arrives first, then the add itself\n", late)
	show("after the add arrives:", early.Add("T1", late))

	fmt.Fprintln(w, "\n4. Beka sees the delete and then adds T1 again on purpose. It comes back.")
	gone, _ := office.Remove("T1")
	back := gone.Add("T1", tag("beka", 50))
	show("Beka after re-adding:", back)
	fmt.Fprintf(w, "   cost: %d tombstone kept forever so far (they are never forgotten yet).\n", back.Tombstones())
}
