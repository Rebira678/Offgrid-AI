package crdt

// Property tests for the OR-Set. The structure is the same as props_test.go:
// the laws are written against a merge FUNCTION, so orset_broken_test.go can
// run the same laws on merges that were broken on purpose.

import (
	"sort"
	"testing"

	"pgregory.net/rapid"
)

type setMergeFn func(a, b Set) Set

var setElems = []string{"a", "b", "c"}

func genTag() *rapid.Generator[Tag] {
	return rapid.Custom(func(t *rapid.T) Tag {
		return Tag{
			Node:  rapid.SampledFrom([]string{"x", "y", "z"}).Draw(t, "node"),
			Stamp: at(rapid.Int64Range(0, 3).Draw(t, "wall"), rapid.Uint32Range(0, 1).Draw(t, "counter")),
		}
	})
}

// genSet builds an arbitrary set: adds and tombstones drawn independently, so
// it includes shapes no replica would produce (a tombstone for a tag that was
// never added, one tag added with two different elements). The merge laws
// must hold for all of them.
func genSet() *rapid.Generator[Set] {
	return rapid.Custom(func(t *rapid.T) Set {
		s := Set{}
		for i, n := 0, rapid.IntRange(0, 5).Draw(t, "adds"); i < n; i++ {
			s = s.Add(rapid.SampledFrom(setElems).Draw(t, "elem"), genTag().Draw(t, "tag"))
		}
		for i, n := 0, rapid.IntRange(0, 4).Draw(t, "removes"); i < n; i++ {
			s = s.RemoveTags(genTag().Draw(t, "dead tag"))
		}
		return s
	})
}

// fresh returns a tag no generated set can contain (generated nodes are x, y, z).
func fresh(n int64) Tag { return Tag{Node: "fresh", Stamp: at(100+n, 0)} }

type setLaw struct {
	name  string
	check func(t *rapid.T, merge setMergeFn)
}

var setLaws = []setLaw{
	{"commutative", func(t *rapid.T, merge setMergeFn) {
		a, b := genSet().Draw(t, "a"), genSet().Draw(t, "b")
		if !EqualSet(merge(a, b), merge(b, a)) {
			t.Fatalf("merge(a,b) != merge(b,a)\n a=%+v\n b=%+v", a, b)
		}
	}},
	{"associative", func(t *rapid.T, merge setMergeFn) {
		a, b, c := genSet().Draw(t, "a"), genSet().Draw(t, "b"), genSet().Draw(t, "c")
		if !EqualSet(merge(merge(a, b), c), merge(a, merge(b, c))) {
			t.Fatalf("grouping changed the result")
		}
	}},
	{"idempotent", func(t *rapid.T, merge setMergeFn) {
		a, b := genSet().Draw(t, "a"), genSet().Draw(t, "b")
		if !EqualSet(merge(a, a), a) {
			t.Fatalf("merge(a,a) != a: %+v", merge(a, a))
		}
		ab := merge(a, b)
		if !EqualSet(merge(ab, b), ab) {
			t.Fatalf("merging b in a second time changed the result")
		}
	}},
	{"the empty set is the identity", func(t *rapid.T, merge setMergeFn) {
		a := genSet().Draw(t, "a")
		if !EqualSet(merge(a, Set{}), a) || !EqualSet(merge(Set{}, a), a) {
			t.Fatalf("merging with the empty set changed %+v", a)
		}
	}},
	{"no add and no tombstone is ever lost", func(t *rapid.T, merge setMergeFn) {
		a, b := genSet().Draw(t, "a"), genSet().Draw(t, "b")
		m := merge(a, b)
		for _, in := range []Set{a, b} {
			for tag := range in.removed {
				if _, ok := m.removed[tag]; !ok {
					t.Fatalf("tombstone %v was lost", tag)
				}
			}
			for tag := range in.adds {
				if _, ok := m.adds[tag]; !ok {
					t.Fatalf("add %v was lost", tag)
				}
			}
		}
	}},
	{"ADD WINS: a concurrent add survives a remove that did not see it", func(t *rapid.T, merge setMergeFn) {
		start := genSet().Draw(t, "start")
		e := rapid.SampledFrom(setElems).Draw(t, "elem")
		start = start.Add(e, fresh(0)) // make sure there is something to remove
		a, _ := start.Remove(e)        // one replica removes what it can see
		b := start.Add(e, fresh(1))    // another adds it again, concurrently
		if !merge(a, b).Has(e) || !merge(b, a).Has(e) {
			t.Fatalf("the concurrent add of %q was destroyed", e)
		}
	}},
	{"REMOVE HOLDS: a remove is not undone by a replica that never saw it", func(t *rapid.T, merge setMergeFn) {
		start := genSet().Draw(t, "start")
		e := rapid.SampledFrom(setElems).Draw(t, "elem")
		start = start.Add(e, fresh(0))
		gone, _ := start.Remove(e)
		if gone.Has(e) {
			t.Fatalf("Remove left the element in")
		}
		if merge(gone, start).Has(e) || merge(start, gone).Has(e) {
			t.Fatalf("the stale copy brought %q back", e)
		}
	}},
	{"does not change its inputs and returns a set of its own", func(t *rapid.T, merge setMergeFn) {
		a, b := genSet().Draw(t, "a"), genSet().Draw(t, "b")
		ac, bc := a.clone(), b.clone()
		m := merge(a, b)
		m.adds[fresh(7)] = "scribble"
		m.removed[fresh(7)] = struct{}{}
		for tag := range m.adds {
			m.adds[tag] = "scribbled"
		}
		if !EqualSet(a, ac) || !EqualSet(b, bc) {
			t.Fatalf("an input was changed")
		}
	}},
}

func TestSetMergeLawsHoldForGeneratedSets(t *testing.T) {
	for _, l := range setLaws {
		l := l
		t.Run(l.name, rapid.MakeCheck(func(rt *rapid.T) { l.check(rt, MergeSet) }))
	}
}

// ---- replicas, scripts and an oracle

// The oracle knows only what happened, not how it was merged: every add (with
// its tag), and for every remove, the exact tags that replica could see. An
// element is live if it has an add whose tag no remove observed.
type history struct {
	adds     map[Tag]string
	observed map[Tag]struct{}
}

func (h *history) live() []string {
	seen := map[string]struct{}{}
	for tag, e := range h.adds {
		if _, dead := h.observed[tag]; !dead {
			seen[e] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for e := range seen {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}

func runScript(t *rapid.T, merge setMergeFn) (reps []Set, h *history) {
	const n = 3
	reps = make([]Set, n)
	h = &history{adds: map[Tag]string{}, observed: map[Tag]struct{}{}}
	clock := make([]int64, n) // each replica's own counter: tags are unique, like an HLC's
	nodes := []string{"x", "y", "z"}
	for s, steps := 0, rapid.IntRange(0, 16).Draw(t, "steps"); s < steps; s++ {
		i := rapid.IntRange(0, n-1).Draw(t, "replica")
		switch rapid.IntRange(0, 2).Draw(t, "action") {
		case 0:
			e := rapid.SampledFrom(setElems).Draw(t, "add")
			clock[i]++
			tag := Tag{nodes[i], at(clock[i], 0)}
			reps[i] = reps[i].Add(e, tag)
			h.adds[tag] = e
		case 1:
			e := rapid.SampledFrom(setElems).Draw(t, "remove")
			var seen []Tag
			reps[i], seen = reps[i].Remove(e)
			for _, tag := range seen {
				h.observed[tag] = struct{}{}
			}
		default:
			j := rapid.IntRange(0, n-1).Draw(t, "from")
			reps[i] = merge(reps[i], reps[j]) // a partial, one-way sync
		}
	}
	return reps, h
}

func exchangeEverything(reps []Set, merge setMergeFn) {
	for round := 0; round < 2; round++ {
		for i := range reps {
			for j := range reps {
				reps[j] = merge(reps[j], reps[i])
			}
		}
	}
}

func TestReplicasThatExchangeEverythingConvergeToTheOracle(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		reps, h := runScript(t, MergeSet)
		exchangeEverything(reps, MergeSet)
		want := h.live()
		for i, r := range reps {
			if !EqualSet(r, reps[0]) {
				t.Fatalf("replica %d differs from replica 0 after full exchange", i)
			}
			got := r.Elements()
			if len(got) != len(want) {
				t.Fatalf("replica %d has %v, the oracle says %v", i, got, want)
			}
			for k := range got {
				if got[k] != want[k] {
					t.Fatalf("replica %d has %v, the oracle says %v", i, got, want)
				}
			}
		}
	})
}

// Merging the same replicas in a shuffled order, with repeats, gives the same set.
func TestSyncOrderAndRepeatsDoNotMatter(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		reps, _ := runScript(t, MergeSet)
		want := Set{}
		for _, r := range reps {
			want = MergeSet(want, r)
		}
		got := Set{}
		order := rapid.Permutation([]int{0, 1, 2}).Draw(t, "order")
		for _, i := range order {
			got = MergeSet(got, reps[i])
		}
		for _, i := range rapid.SliceOfN(rapid.IntRange(0, 2), 0, 6).Draw(t, "again") {
			got = MergeSet(got, reps[i])
		}
		if !EqualSet(got, want) {
			t.Fatalf("a different order or repeated delivery changed the result")
		}
	})
}

func TestEveryElementWithALiveAddIsInTheSetAndNothingElseIs(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := genSet().Draw(t, "set")
		live := map[string]bool{}
		for tag, e := range s.adds {
			if _, dead := s.removed[tag]; !dead {
				live[e] = true
			}
		}
		for _, e := range setElems {
			if s.Has(e) != live[e] {
				t.Fatalf("Has(%q) = %v, but live adds say %v", e, s.Has(e), live[e])
			}
		}
		if len(s.Elements()) != len(live) {
			t.Fatalf("Elements %v vs live %v", s.Elements(), live)
		}
	})
}

func TestNoteElementsRoundTripForGeneratedNotes(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		task := rapid.StringMatching(`T[0-9]{1,4}`).Draw(t, "task")
		tag := Tag{
			Node:  rapid.StringMatching(`[A-Za-z0-9._-]{1,16}`).Draw(t, "node"),
			Stamp: at(rapid.Int64Range(0, 1<<62).Draw(t, "wall"), rapid.Uint32().Draw(t, "counter")),
		}
		text := rapid.String().Draw(t, "text")
		id, got, gt, ok := SplitNoteElement(NoteElement(task, tag, text))
		if !ok || id != task || got != tag || gt != text {
			t.Fatalf("%q %v %q came back as %q %v %q (%v)", task, tag, text, id, got, gt, ok)
		}
	})
}
