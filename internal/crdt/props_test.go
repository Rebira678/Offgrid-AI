package crdt

// Property-based tests for the merge, using pgregory.net/rapid.
//
// An example test says "for THIS input the answer is X". A property says "for
// EVERY input, this is true", and rapid generates hundreds of inputs per run,
// then shrinks any failure to the smallest one it can find. The inputs here
// are drawn from deliberately tiny domains (3 values, 4 keys, stamps 0..3), so
// the interesting collisions (equal stamps, equal nodes, a delete against a
// write) happen constantly instead of once in a million runs.
//
// The laws are written against a merge FUNCTION so that broken_test.go can
// run the very same checks against deliberately broken merges and show that
// they are caught.

import (
	"flag"
	"os"
	"sort"
	"testing"

	"pgregory.net/rapid"
)

// TestMain raises rapid's default from 100 cases per property to 3000, so a
// plain `go test` checks thousands of generated cases per law. Pass
// -rapid.checks=N to choose another number (the command line wins).
func TestMain(m *testing.M) {
	_ = flag.Set("rapid.checks", "3000")
	os.Exit(m.Run())
}

type mergeFn func(a, b Map) Map

var propKeys = []string{
	TaskKey("T1", FieldTitle), TaskKey("T1", FieldStatus), TaskKey("T2", FieldStatus), "other",
}

func genEntry() *rapid.Generator[Entry] {
	return rapid.Custom(func(t *rapid.T) Entry {
		return Entry{
			Value:   rapid.SampledFrom([]string{"a", "b", "c"}).Draw(t, "value"),
			Deleted: rapid.Bool().Draw(t, "deleted"),
			Stamp:   at(rapid.Int64Range(0, 3).Draw(t, "wall"), rapid.Uint32Range(0, 2).Draw(t, "counter")),
			Node:    rapid.SampledFrom([]string{"x", "y", "z"}).Draw(t, "node"),
		}
	})
}

func genMap() *rapid.Generator[Map] {
	return rapid.Custom(func(t *rapid.T) Map {
		n := rapid.IntRange(0, 5).Draw(t, "entries")
		m := Map{}
		for i := 0; i < n; i++ {
			m[rapid.SampledFrom(propKeys).Draw(t, "key")] = genEntry().Draw(t, "entry")
		}
		return m
	})
}

// write is one edit: a key and the entry written to it.
type write struct {
	Key   string
	Entry Entry
}

func genWrites(max int) *rapid.Generator[[]write] {
	return rapid.SliceOfN(rapid.Custom(func(t *rapid.T) write {
		return write{rapid.SampledFrom(propKeys).Draw(t, "key"), genEntry().Draw(t, "entry")}
	}), 0, max)
}

func independentMap(m Map) Map {
	out := Map{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ---- the laws. Each takes the merge under test.

type law struct {
	name  string
	check func(t *rapid.T, merge mergeFn)
}

var laws = []law{
	{"commutative", func(t *rapid.T, merge mergeFn) {
		a, b := genMap().Draw(t, "a"), genMap().Draw(t, "b")
		if x, y := merge(a, b), merge(b, a); !Equal(x, y) {
			t.Fatalf("merge(a,b) != merge(b,a)\n  merge(a,b) = %v\n  merge(b,a) = %v", x, y)
		}
	}},
	{"associative", func(t *rapid.T, merge mergeFn) {
		a, b, c := genMap().Draw(t, "a"), genMap().Draw(t, "b"), genMap().Draw(t, "c")
		if x, y := merge(merge(a, b), c), merge(a, merge(b, c)); !Equal(x, y) {
			t.Fatalf("merge(merge(a,b),c) != merge(a,merge(b,c))\n  left  = %v\n  right = %v", x, y)
		}
	}},
	{"idempotent", func(t *rapid.T, merge mergeFn) {
		a := genMap().Draw(t, "a")
		if x := merge(a, a); !Equal(x, a) {
			t.Fatalf("merge(a,a) != a: %v", x)
		}
		b := genMap().Draw(t, "b")
		ab := merge(a, b)
		if x := merge(ab, b); !Equal(x, ab) { // receiving the same data twice changes nothing
			t.Fatalf("merging b in a second time changed the result: %v vs %v", x, ab)
		}
	}},
	{"the empty map is the identity", func(t *rapid.T, merge mergeFn) {
		a := genMap().Draw(t, "a")
		if !Equal(merge(a, nil), a) || !Equal(merge(nil, a), a) || !Equal(merge(a, Map{}), a) {
			t.Fatalf("merging with the empty map changed %v", a)
		}
	}},
	{"keeps every key and never goes backwards", func(t *rapid.T, merge mergeFn) {
		a, b := genMap().Draw(t, "a"), genMap().Draw(t, "b")
		m := merge(a, b)
		for _, in := range []Map{a, b} {
			for k, e := range in {
				got, ok := m[k]
				if !ok {
					t.Fatalf("key %q was lost", k)
				}
				if got.Stamp.Compare(e.Stamp) < 0 { // by stamp only: how ties are broken is a free choice
					t.Fatalf("key %q went backwards: %v became %v", k, e, got)
				}
			}
		}
		if len(m) > len(a)+len(b) {
			t.Fatalf("merge invented keys: %v", m)
		}
	}},
	{"the larger stamp wins (the LWW rule itself)", func(t *rapid.T, merge mergeFn) {
		a, b := genMap().Draw(t, "a"), genMap().Draw(t, "b")
		m := merge(a, b)
		for _, k := range propKeys {
			ea, okA := a[k]
			eb, okB := b[k]
			switch {
			case okA && okB && ea.Stamp.Compare(eb.Stamp) != 0:
				want := ea
				if eb.Stamp.After(ea.Stamp) {
					want = eb
				}
				if m[k] != want {
					t.Fatalf("key %q: %v (stamp %v) vs %v (stamp %v): the later stamp must win, got %v", k, ea.Value, ea.Stamp, eb.Value, eb.Stamp, m[k])
				}
			case okA && !okB && m[k] != ea, !okA && okB && m[k] != eb:
				t.Fatalf("key %q present on one side only must be kept as is: %v", k, m[k])
			}
		}
	}},
	{"does not change its inputs and returns a map of its own", func(t *rapid.T, merge mergeFn) {
		a, b := genMap().Draw(t, "a"), genMap().Draw(t, "b")
		ac, bc := independentMap(a), independentMap(b)
		m := merge(a, b)
		m["scribble"] = Entry{Value: "x"}
		for k := range m {
			m[k] = Entry{Value: "scribbled", Stamp: at(99, 99)}
		}
		if !Equal(a, ac) || !Equal(b, bc) {
			t.Fatalf("an input was changed:\n a %v -> %v\n b %v -> %v", ac, a, bc, b)
		}
	}},
	{"replicas that exchange everything converge", func(t *rapid.T, merge mergeFn) {
		const n = 3
		reps := make([]Map, n)
		steps := rapid.IntRange(0, 12).Draw(t, "steps")
		for s := 0; s < steps; s++ {
			i := rapid.IntRange(0, n-1).Draw(t, "replica")
			if rapid.Bool().Draw(t, "is write") {
				w := genWrites(1).Draw(t, "write")
				for _, x := range w {
					reps[i] = reps[i].Put(x.Key, x.Entry)
				}
			} else {
				j := rapid.IntRange(0, n-1).Draw(t, "from")
				reps[i] = merge(reps[i], reps[j]) // a one-way, partial sync
			}
		}
		for round := 0; round < 2; round++ {
			for i := 0; i < n; i++ {
				for j := 0; j < n; j++ {
					reps[j] = merge(reps[j], reps[i])
				}
			}
		}
		for i := 1; i < n; i++ {
			if !Equal(reps[0], reps[i]) {
				t.Fatalf("replica 0 and %d differ after everyone sent to everyone:\n  %v\n  %v", i, reps[0], reps[i])
			}
		}
	}},
}

func TestMergeLawsHoldForGeneratedMaps(t *testing.T) {
	for _, l := range laws {
		l := l
		t.Run(l.name, rapid.MakeCheck(func(rt *rapid.T) { l.check(rt, Merge) }))
	}
}

// ---- properties of the pieces around Merge (these use the real code only)

func TestWritesGiveTheSameMapInAnyOrderAndWithDuplicates(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ws := genWrites(12).Draw(t, "writes")
		apply := func(order []write) Map {
			m := Map{}
			for _, w := range order {
				m = m.Put(w.Key, w.Entry)
			}
			return m
		}
		want := apply(ws)
		perm := rapid.Permutation(ws).Draw(t, "permutation")
		if got := apply(perm); !Equal(got, want) {
			t.Fatalf("a different delivery order gave a different map:\n  in order: %v\n  permuted: %v", want, got)
		}
		dups := append([]write{}, perm...)
		if len(ws) > 0 { // deliver some of them a second time
			for _, i := range rapid.SliceOfN(rapid.IntRange(0, len(ws)-1), 0, 5).Draw(t, "again") {
				dups = append(dups, ws[i])
			}
		}
		if got := apply(dups); !Equal(got, want) {
			t.Fatalf("delivering some writes twice changed the map:\n  %v\n  %v", want, got)
		}
	})
}

func TestPutIsMergeWithASingleEntry(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		a := genMap().Draw(t, "a")
		k, e := rapid.SampledFrom(propKeys).Draw(t, "key"), genEntry().Draw(t, "entry")
		if x, y := a.Put(k, e), Merge(a, Map{k: e}); !Equal(x, y) {
			t.Fatalf("Put and Merge disagree: %v vs %v", x, y)
		}
	})
}

// The strongest check: whatever the replicas do and however they sync, the end
// state equals the oracle, which is simply "for each key, the largest entry
// anyone ever wrote", computed without Merge at all.
func TestConvergedStateEqualsTheLargestEntryEverWritten(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		const n = 3
		reps := make([]Map, n)
		oracle := map[string]Entry{}
		for s := 0; s < rapid.IntRange(0, 16).Draw(t, "steps"); s++ {
			i := rapid.IntRange(0, n-1).Draw(t, "replica")
			if rapid.Bool().Draw(t, "write?") {
				k, e := rapid.SampledFrom(propKeys).Draw(t, "key"), genEntry().Draw(t, "entry")
				reps[i] = reps[i].Put(k, e)
				if cur, ok := oracle[k]; !ok || e.Compare(cur) > 0 {
					oracle[k] = e
				}
			} else {
				reps[i] = Merge(reps[i], reps[rapid.IntRange(0, n-1).Draw(t, "from")])
			}
		}
		for round := 0; round < 2; round++ {
			for _, i := range rapid.Permutation([]int{0, 1, 2}).Draw(t, "order") {
				for j := 0; j < n; j++ {
					reps[j] = Merge(reps[j], reps[i])
				}
			}
		}
		want := Map(oracle)
		for i := range reps {
			if !Equal(reps[i], want) {
				t.Fatalf("replica %d = %v, the oracle says %v", i, reps[i], want)
			}
		}
	})
}

func TestCompareIsATotalOrderOnGeneratedEntries(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		x, y, z := genEntry().Draw(t, "x"), genEntry().Draw(t, "y"), genEntry().Draw(t, "z")
		if x.Compare(y) != -y.Compare(x) {
			t.Fatalf("not antisymmetric: %v %v", x, y)
		}
		if (x.Compare(y) == 0) != (x == y) {
			t.Fatalf("Compare is 0 for different entries (or not 0 for equal ones): %v %v", x, y)
		}
		if x.Compare(y) <= 0 && y.Compare(z) <= 0 && x.Compare(z) > 0 {
			t.Fatalf("not transitive: %v <= %v <= %v but %v > %v", x, y, z, x, z)
		}
		sorted := []Entry{x, y, z}
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].Compare(sorted[j]) < 0 })
		for i := 1; i < len(sorted); i++ {
			if sorted[i-1].Compare(sorted[i]) > 0 {
				t.Fatalf("sorting by Compare did not sort: %v", sorted)
			}
		}
	})
}

func TestTaskKeysRoundTripForGeneratedIDsAndFields(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		id := rapid.StringMatching(`[A-Za-z0-9._-]{1,12}`).Draw(t, "id")
		field := rapid.SampledFrom([]string{FieldTitle, FieldSite, FieldPriority, FieldStatus}).Draw(t, "field")
		gid, gf, ok := SplitTaskKey(TaskKey(id, field))
		if !ok || gid != id || gf != field {
			t.Fatalf("%q/%q came back as %q/%q (%v)", id, field, gid, gf, ok)
		}
	})
}
