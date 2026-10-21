package crdt

import (
	"math/rand"
	"reflect"
	"testing"

	"offgrid/internal/hlc"
)

func at(wall int64, counter uint32) hlc.Timestamp { return hlc.Timestamp{Wall: wall, Counter: counter} }
func e(v string, st hlc.Timestamp, node string) Entry {
	return Entry{Value: v, Stamp: st, Node: node}
}

// ---- the scenarios the roadmap asks for: concurrent edits to the same field

func TestConcurrentEditsToTheSameFieldTheLaterStampWinsOnBothSides(t *testing.T) {
	k := TaskKey("T1", FieldPriority)
	alice := Map{}.Put(k, e("low", at(100, 0), "alice"))
	bob := Map{}.Put(k, e("high", at(200, 0), "bob"))
	ab, ba := Merge(alice, bob), Merge(bob, alice)
	if v, _ := ab.Get(k); v != "high" {
		t.Errorf("alice.Merge(bob) = %q, want high", v)
	}
	if !Equal(ab, ba) {
		t.Errorf("merge order changed the result: %v vs %v", ab, ba)
	}
}

func TestSameWallDifferentCounterTheHigherCounterWins(t *testing.T) {
	k := TaskKey("T1", FieldStatus)
	a := Map{}.Put(k, e("open", at(100, 3), "z-node"))
	b := Map{}.Put(k, e("done", at(100, 4), "a-node"))
	if v, _ := Merge(a, b).Get(k); v != "done" {
		t.Errorf("counter should decide before node: got %q", v)
	}
}

func TestEqualStampsAreDecidedByNodeIDTheSameOnEveryReplica(t *testing.T) {
	k := TaskKey("T1", FieldPriority)
	a := Map{}.Put(k, e("low", at(100, 1), "node-a"))
	b := Map{}.Put(k, e("high", at(100, 1), "node-b"))
	for name, m := range map[string]Map{"a.Merge(b)": Merge(a, b), "b.Merge(a)": Merge(b, a)} {
		if v, _ := m.Get(k); v != "high" {
			t.Errorf("%s = %q, want high (node-b > node-a)", name, v)
		}
	}
}

func TestEditsToDifferentFieldsAreBothKept(t *testing.T) {
	alice := Map{}.Put(TaskKey("T1", FieldPriority), e("low", at(100, 0), "alice"))
	bob := Map{}.Put(TaskKey("T1", FieldStatus), e("done", at(50, 0), "bob")) // even though older
	m := Merge(alice, bob)
	if p, _ := m.Get(TaskKey("T1", FieldPriority)); p != "low" {
		t.Errorf("priority lost: %q", p)
	}
	if s, _ := m.Get(TaskKey("T1", FieldStatus)); s != "done" {
		t.Errorf("status lost: %q", s)
	}
}

func TestAllThreeReplicasAgreeWhateverTheMergeOrder(t *testing.T) {
	k := TaskKey("T1", FieldTitle)
	a := Map{}.Put(k, e("A", at(10, 0), "a"))
	b := Map{}.Put(k, e("B", at(30, 0), "b"))
	c := Map{}.Put(k, e("C", at(20, 0), "c"))
	want := Merge(Merge(a, b), c)
	for _, got := range []Map{Merge(a, Merge(b, c)), Merge(Merge(c, a), b), Merge(c, Merge(b, a)), Merge(Merge(b, c), a)} {
		if !Equal(got, want) {
			t.Errorf("order-dependent merge: %v vs %v", got, want)
		}
	}
	if v, _ := want.Get(k); v != "B" {
		t.Errorf("winner %q, want B", v)
	}
}

func TestMergeIsIdempotentAndEmptyIsTheIdentity(t *testing.T) {
	m := Map{}.Put("k", e("v", at(5, 5), "n"))
	if !Equal(Merge(m, m), m) || !Equal(Merge(m, nil), m) || !Equal(Merge(nil, m), m) || len(Merge(nil, nil)) != 0 {
		t.Error("merge with itself or the empty map changed something")
	}
}

// ---- delete

func TestDeleteBeatsAnOlderWriteAndLosesToANewerOne(t *testing.T) {
	k := TaskKey("T1", FieldSite)
	base := Map{}.Put(k, e("site 4", at(100, 0), "a"))
	del := base.Delete(k, at(200, 0), "a")
	if _, ok := del.Get(k); ok {
		t.Error("a deleted key is still readable")
	}
	late := Map{}.Put(k, e("site 9", at(150, 0), "b")) // written before the delete
	if _, ok := Merge(del, late).Get(k); ok {
		t.Error("an older write resurrected a deleted key")
	}
	newer := Map{}.Put(k, e("site 9", at(300, 0), "b"))
	if v, ok := Merge(del, newer).Get(k); !ok || v != "site 9" {
		t.Errorf("a newer write should win over the tombstone: %q %v", v, ok)
	}
	if got := len(del.Keys()); got != 1 {
		t.Errorf("the tombstone must stay in the map so it can win later: %d keys", got)
	}
}

// ---- Put

func TestPutKeepsTheNewerEntryWhateverTheArrivalOrder(t *testing.T) {
	k := "k"
	old, newer := e("old", at(1, 0), "n"), e("new", at(2, 0), "n")
	if v, _ := (Map{}).Put(k, old).Put(k, newer).Get(k); v != "new" {
		t.Errorf("old then new: %q", v)
	}
	if v, _ := (Map{}).Put(k, newer).Put(k, old).Get(k); v != "new" {
		t.Errorf("new then old: %q", v)
	}
}

// ---- purity: the roadmap's hard requirement

// independentCopy deliberately does not use the package's own clone, so a
// broken clone cannot hide the damage it does.
func independentCopy(m Map) Map {
	out := Map{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

func TestMergeAndPutNeverTouchTheirInputs(t *testing.T) {
	a := Map{"x": e("1", at(1, 0), "a")}
	b := Map{"x": e("2", at(2, 0), "b"), "y": e("3", at(1, 0), "b")}
	aCopy, bCopy := independentCopy(a), independentCopy(b)
	out := Merge(a, b)
	out["x"] = e("mutated", at(9, 9), "z") // the result must be independent
	out["y"] = e("mutated", at(9, 9), "z")
	_ = a.Put("x", e("3", at(3, 0), "c"))
	_ = a.Delete("x", at(9, 0), "c")
	if !Equal(a, aCopy) || !Equal(b, bCopy) {
		t.Errorf("an input changed: a=%v b=%v", a, b)
	}
	// even when nothing is merged in, the result is a fresh map
	same := Merge(a, nil)
	same["x"] = e("changed", at(7, 0), "q")
	if !Equal(a, aCopy) {
		t.Error("Merge(a, nil) returned a map that aliases a")
	}
}

func TestMergeReadsNoClockAndNoState(t *testing.T) {
	a := Map{"x": e("1", at(1, 0), "a")}
	b := Map{"x": e("2", at(2, 0), "b")}
	first := Merge(a, b)
	for i := 0; i < 50; i++ {
		if !Equal(Merge(a, b), first) {
			t.Fatal("the same inputs gave different outputs")
		}
	}
}

// ---- Compare is a total order (this is what the CRDT laws rest on)

func TestCompareIsATotalOrder(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	gen := func() Entry {
		return Entry{Value: string(rune('a' + r.Intn(3))), Deleted: r.Intn(2) == 0,
			Stamp: at(int64(r.Intn(3)), uint32(r.Intn(3))), Node: string(rune('x' + r.Intn(3)))}
	}
	for i := 0; i < 20000; i++ {
		x, y, z := gen(), gen(), gen()
		if x.Compare(y) != -y.Compare(x) {
			t.Fatalf("not antisymmetric: %v %v", x, y)
		}
		if (x.Compare(y) == 0) != (x == y) {
			t.Fatalf("Compare==0 must mean identical: %v %v", x, y)
		}
		if x.Compare(y) <= 0 && y.Compare(z) <= 0 && x.Compare(z) > 0 {
			t.Fatalf("not transitive: %v %v %v", x, y, z)
		}
	}
}

func TestEachPartOfTheOrderMattersInTheRightPriority(t *testing.T) {
	base := Entry{Value: "m", Stamp: at(10, 5), Node: "m"}
	cases := []struct {
		name string
		o    Entry
		want int // base.Compare(o)
	}{
		{"older wall", Entry{Value: "z", Stamp: at(11, 0), Node: "z", Deleted: true}, -1},
		{"newer wall", Entry{Value: "a", Stamp: at(9, 9), Node: "a"}, 1},
		{"counter beats node", Entry{Value: "a", Stamp: at(10, 6), Node: "a"}, -1},
		{"node beats value", Entry{Value: "a", Stamp: at(10, 5), Node: "n"}, -1},
		{"deleted sorts after live", Entry{Value: "a", Stamp: at(10, 5), Node: "m", Deleted: true}, -1},
		{"value is the last resort", Entry{Value: "n", Stamp: at(10, 5), Node: "m"}, -1},
		{"identical", base, 0},
	}
	for _, c := range cases {
		if got := base.Compare(c.o); got != c.want {
			t.Errorf("%s: Compare = %d, want %d", c.name, got, c.want)
		}
	}
}

// ---- a randomized convergence check (Day 17 replaces it with rapid)

func TestRandomReplicasThatExchangeEverythingEndIdentical(t *testing.T) {
	for seed := int64(0); seed < 200; seed++ {
		r := rand.New(rand.NewSource(seed))
		const n = 4
		reps := make([]Map, n)
		for step := 0; step < 60; step++ {
			i := r.Intn(n)
			key := TaskKey("T"+string(rune('1'+r.Intn(2))), []string{FieldTitle, FieldStatus, FieldPriority}[r.Intn(3)])
			ent := Entry{Value: string(rune('a' + r.Intn(5))), Deleted: r.Intn(8) == 0,
				Stamp: at(int64(r.Intn(20)), uint32(r.Intn(3))), Node: string(rune('A' + i))}
			reps[i] = reps[i].Put(key, ent)
			if r.Intn(3) == 0 { // a partial, one-way sync
				j := r.Intn(n)
				reps[j] = Merge(reps[j], reps[i])
			}
		}
		// everyone sends to everyone, in a seed-dependent order
		for _, i := range r.Perm(n) {
			for _, j := range r.Perm(n) {
				reps[j] = Merge(reps[j], reps[i])
			}
		}
		for _, i := range r.Perm(n) {
			for _, j := range r.Perm(n) {
				reps[j] = Merge(reps[j], reps[i])
			}
		}
		for i := 1; i < n; i++ {
			if !Equal(reps[0], reps[i]) {
				t.Fatalf("seed %d: replica 0 and %d differ:\n%v\n%v", seed, i, reps[0], reps[i])
			}
		}
	}
}

// ---- task keys

func TestTaskKeysRoundTripAndRejectOtherKeys(t *testing.T) {
	id, f, ok := SplitTaskKey(TaskKey("T12", FieldPriority))
	if !ok || id != "T12" || f != "priority" {
		t.Errorf("round trip: %q %q %v", id, f, ok)
	}
	// the field is whatever follows the LAST slash
	if id, f, ok := SplitTaskKey("task/a/b/c"); !ok || id != "a/b" || f != "c" {
		t.Errorf("last slash: %q %q %v", id, f, ok)
	}
	for _, bad := range []string{"", "task/", "task/T1", "task//x", "task/T1/", "note/T1/title", "title"} {
		if _, _, ok := SplitTaskKey(bad); ok {
			t.Errorf("%q should not parse", bad)
		}
	}
}

func TestTaskFieldsGroupsByTaskAndSkipsDeletedAndForeignKeys(t *testing.T) {
	m := Map{}.
		Put(TaskKey("T1", FieldTitle), e("Pump", at(1, 0), "a")).
		Put(TaskKey("T1", FieldStatus), e("open", at(1, 1), "a")).
		Put(TaskKey("T2", FieldTitle), e("Filter", at(2, 0), "a")).
		Put("other", e("x", at(1, 0), "a")).
		Delete(TaskKey("T2", FieldTitle), at(3, 0), "a")
	want := map[string]map[string]string{"T1": {"title": "Pump", "status": "open"}}
	if got := TaskFields(m); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestKeysAreSortedAndIncludeTombstones(t *testing.T) {
	m := Map{}.Put("b", e("1", at(1, 0), "n")).Put("a", e("1", at(1, 0), "n")).Delete("c", at(2, 0), "n")
	if got := m.Keys(); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("%v", got)
	}
	if _, ok := Map(nil).Get("x"); ok || len(Map(nil).Keys()) != 0 {
		t.Error("nil map should be empty")
	}
}

func TestEqualDetectsEveryKindOfDifference(t *testing.T) {
	a := Map{"k": e("v", at(1, 1), "n")}
	for name, b := range map[string]Map{
		"extra key": {"k": a["k"], "j": a["k"]}, "missing key": {}, "other key": {"j": a["k"]},
		"value": {"k": e("w", at(1, 1), "n")}, "stamp": {"k": e("v", at(1, 2), "n")},
		"node": {"k": e("v", at(1, 1), "m")}, "deleted": {"k": {Value: "v", Stamp: at(1, 1), Node: "n", Deleted: true}},
	} {
		if Equal(a, b) {
			t.Errorf("%s not detected", name)
		}
	}
}
