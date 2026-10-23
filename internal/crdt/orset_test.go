package crdt

import (
	"reflect"
	"testing"
)

func tg(node string, wall int64) Tag { return Tag{Node: node, Stamp: at(wall, 0)} }

// ---- the scenario the roadmap asks for: concurrent add and remove

func TestConcurrentAddAndRemoveOfTheSameItemTheAddWins(t *testing.T) {
	// both replicas start with "pump" added once, at tag t1
	t1 := tg("lidia", 1)
	start := Set{}.Add("pump", t1)
	lidia, removed := start.Remove("pump") // Lidia deletes it
	if len(removed) != 1 || removed[0] != t1 {
		t.Fatalf("Lidia should have observed exactly tag t1: %v", removed)
	}
	beka := start.Add("pump", tg("beka", 2)) // Beka, not having seen the delete, adds it again
	for name, m := range map[string]Set{"lidia+beka": MergeSet(lidia, beka), "beka+lidia": MergeSet(beka, lidia)} {
		if !m.Has("pump") {
			t.Errorf("%s: Beka's concurrent add was destroyed by a remove that never saw it", name)
		}
	}
	if !EqualSet(MergeSet(lidia, beka), MergeSet(beka, lidia)) {
		t.Error("merge order changed the result")
	}
	if lidia.Has("pump") || !beka.Has("pump") {
		t.Error("before merging, each side still sees its own state")
	}
}

func TestARemoveOfWhatItSawStaysRemovedWhenAStaleCopyArrives(t *testing.T) {
	start := Set{}.Add("pump", tg("lidia", 1))
	gone, _ := start.Remove("pump")
	if m := MergeSet(gone, start); m.Has("pump") { // the stale copy still has it live
		t.Error("a replica that never saw the remove brought the element back")
	}
	if m := MergeSet(start, gone); m.Has("pump") {
		t.Error("(other order) the element came back")
	}
}

func TestRemoveThenAddAgainWithANewTagBringsItBack(t *testing.T) {
	s := Set{}.Add("pump", tg("a", 1))
	s, _ = s.Remove("pump")
	s = s.Add("pump", tg("a", 2))
	if !s.Has("pump") {
		t.Error("re-adding after a remove must work")
	}
	if got := s.Observed("pump"); len(got) != 1 || got[0] != tg("a", 2) {
		t.Errorf("only the new tag is live: %v", got)
	}
}

func TestATombstoneThatArrivesBeforeItsAddStillRemovesIt(t *testing.T) {
	t1 := tg("lidia", 1)
	early := Set{}.RemoveTags(t1) // the remove arrives first
	late := early.Add("pump", t1) // then the add it refers to
	if late.Has("pump") {
		t.Error("the add arrived after its tombstone and was resurrected")
	}
	// and the same through merge, in both orders
	addOnly := Set{}.Add("pump", t1)
	if MergeSet(early, addOnly).Has("pump") || MergeSet(addOnly, early).Has("pump") {
		t.Error("order of arrival changed membership")
	}
}

func TestRemovingAnElementThatIsNotThereDoesNothing(t *testing.T) {
	s := Set{}.Add("a", tg("n", 1))
	out, tags := s.Remove("zzz")
	if len(tags) != 0 || out.Tombstones() != 0 || !EqualSet(out, s) {
		t.Errorf("tags %v, tombstones %d", tags, out.Tombstones())
	}
}

func TestSeveralTagsOfOneElementAreAllRemovedByOneRemoveThatSawThem(t *testing.T) {
	s := Set{}.Add("x", tg("a", 1)).Add("x", tg("b", 2)).Add("y", tg("a", 3))
	out, tags := s.Remove("x")
	if len(tags) != 2 || out.Has("x") || !out.Has("y") || out.Tombstones() != 2 {
		t.Errorf("%v %v %d", tags, out.Elements(), out.Tombstones())
	}
	if !reflect.DeepEqual(tags, []Tag{tg("a", 1), tg("b", 2)}) {
		t.Errorf("observed tags must be sorted: %v", tags)
	}
}

func TestAddingTheSameTagTwiceIsOneAdd(t *testing.T) {
	s := Set{}.Add("x", tg("a", 1)).Add("x", tg("a", 1))
	if got := s.Observed("x"); len(got) != 1 {
		t.Errorf("%v", got)
	}
	if s2, _ := s.Remove("x"); s2.Has("x") {
		t.Error("one remove must kill the one tag")
	}
}

func TestElementsAreSortedAndListedOnce(t *testing.T) {
	s := Set{}.Add("b", tg("a", 1)).Add("a", tg("a", 2)).Add("b", tg("c", 3))
	if got := s.Elements(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("%v", got)
	}
	if got := (Set{}).Elements(); len(got) != 0 || (Set{}).Has("x") || (Set{}).Tombstones() != 0 {
		t.Error("the zero Set is the empty set")
	}
}

func TestEqualSetSeesDifferencesInAddsAndInTombstones(t *testing.T) {
	a := Set{}.Add("x", tg("n", 1))
	if EqualSet(a, Set{}) || EqualSet(Set{}, a) {
		t.Error("adds differ")
	}
	if EqualSet(a, Set{}.Add("y", tg("n", 1))) {
		t.Error("same tag, different element")
	}
	if EqualSet(a, a.RemoveTags(tg("n", 1))) || EqualSet(a.RemoveTags(tg("n", 1)), a) {
		t.Error("tombstones differ")
	}
	if EqualSet(a.RemoveTags(tg("n", 1)), a.RemoveTags(tg("m", 1))) {
		t.Error("different tombstones")
	}
	if !EqualSet(a, Set{}.Add("x", tg("n", 1))) {
		t.Error("equal sets reported different")
	}
}

func TestOneTagSeenWithTwoElementsKeepsTheSmallerOnEveryReplica(t *testing.T) {
	// cannot happen with unique tags; the rule exists so a forged input still converges
	a, b := Set{}.Add("m", tg("n", 1)), Set{}.Add("k", tg("n", 1))
	if !EqualSet(MergeSet(a, b), MergeSet(b, a)) || !MergeSet(a, b).Has("k") || MergeSet(a, b).Has("m") {
		t.Error("the smaller element should win, in both orders")
	}
}

func TestSetOperationsNeverChangeTheirInputs(t *testing.T) {
	a := Set{}.Add("x", tg("a", 1))
	b := Set{}.Add("y", tg("b", 2))
	ac, bc := a.clone(), b.clone()
	_ = a.Add("z", tg("c", 3))
	_, _ = a.Remove("x")
	_ = a.RemoveTags(tg("a", 1))
	out := MergeSet(a, b)
	out.adds[tg("q", 9)] = "scribble"
	out.removed[tg("q", 9)] = struct{}{}
	if !EqualSet(a, ac) || !EqualSet(b, bc) {
		t.Error("an input was changed")
	}
}

// ---- the user-facing story: "what happens when I delete a task while you edit it?"

func TestDeletingATaskWhileSomeoneEditsItKeepsTheTask(t *testing.T) {
	// the set of tasks; an edit counts as touching the task, which re-adds it
	start := Set{}.Add("T1", tg("office", 1))
	lidia, _ := start.Remove("T1")         // Lidia deletes T1
	beka := start.Add("T1", tg("beka", 2)) // Beka edits T1 at the same time
	merged := MergeSet(lidia, beka)
	if !merged.Has("T1") {
		t.Error("Beka's edit should keep the task alive")
	}
	// If Beka had seen the deletion first, his later add is a deliberate re-create and also wins;
	// if Beka had NOT edited, the delete holds:
	if MergeSet(lidia, start).Has("T1") {
		t.Error("without a concurrent edit the delete must hold")
	}
}

// ---- notes

func TestNoteElementsRoundTripAndKeepEveryCharacterOfTheText(t *testing.T) {
	cases := []struct {
		task string
		tag  Tag
		text string
	}{
		{"T1", Tag{"node-a", at(1760000000123456789, 7)}, "seal cracked"},
		{"T12", Tag{"n.with.dots_and-dashes", at(0, 0)}, ""},
		{"T3", Tag{"n", at(5, 4294967295)}, "line one\nline two\n\n"},
		{"T4", Tag{"n", at(9223372036854775807, 1)}, "note/T1/x\ny"},
		{"T1/x", Tag{"n", at(3, 3)}, "a task ID with a slash still splits at the last one"},
	}
	for _, c := range cases {
		id, tag, text, ok := SplitNoteElement(NoteElement(c.task, c.tag, c.text))
		if !ok || id != c.task || tag != c.tag || text != c.text {
			t.Errorf("%+v came back as %q %v %q %v", c, id, tag, text, ok)
		}
	}
	for _, bad := range []string{"", "x", "note/T1", "note/T1/short\ntext", "note//00000000000000000001.0000000001.n\nx",
		"note/T1/0000000000000000000a.0000000001.n\nx", "note/T1/00000000000000000001-0000000001.n\nx", "note/T1/00000000000000000001.0000000001\nx",
		"note/T1/00000000000000000001.0000000001.\nx", "note/T1/00000000000000000001.0000000001-n\nx"} {
		if _, _, _, ok := SplitNoteElement(bad); ok {
			t.Errorf("%q should not parse", bad)
		}
	}
}

func TestTwoNotesWithTheSameTextAreTwoNotes(t *testing.T) {
	s := Set{}
	for i, who := range []string{"lidia", "beka", "lidia"} {
		tag := Tag{who, at(int64(10+i), 0)}
		s = s.Add(NoteElement("T1", tag, "ok"), tag)
	}
	if got := TaskNotes(s)["T1"]; len(got) != 3 {
		t.Errorf("three adds of \"ok\" are three notes, got %d", len(got))
	}
}

func TestTaskNotesAreOldestFirstSkipRemovedAndIgnoreOtherElements(t *testing.T) {
	var s Set
	add := func(task, who string, wall int64, text string) Tag {
		tag := Tag{who, at(wall, 0)}
		s = s.Add(NoteElement(task, tag, text), tag)
		return tag
	}
	add("T1", "beka", 30, "third")
	first := add("T1", "lidia", 10, "first")
	add("T1", "carol", 20, "second")
	add("T2", "lidia", 5, "other task")
	gone := add("T2", "lidia", 6, "deleted")
	s = s.Add("not a note", tg("x", 1))
	s, _ = s.Remove(NoteElement("T2", gone, "deleted"))
	got := TaskNotes(s)
	var texts []string
	for _, n := range got["T1"] {
		texts = append(texts, n.Text)
	}
	if !reflect.DeepEqual(texts, []string{"first", "second", "third"}) || got["T1"][0].Tag != first {
		t.Errorf("T1 notes: %v", got["T1"])
	}
	if len(got["T2"]) != 1 || got["T2"][0].Text != "other task" || len(got) != 2 {
		t.Errorf("T2 notes: %v (all: %v)", got["T2"], got)
	}
}

func TestTagString(t *testing.T) {
	if got := (Tag{"lidia", at(0, 3)}).String(); got != "lidia@1970-01-01T00:00:00Z#3" {
		t.Errorf("%q", got)
	}
}

func TestObservedTagsAreSortedWhateverTheMapOrder(t *testing.T) {
	var s Set
	var want []Tag
	for i, node := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		tag := tg(node, int64(100-i)) // node order and wall order disagree on purpose
		s = s.Add("x", tag)
		want = append(want, tag)
	}
	for run := 0; run < 20; run++ { // map iteration order changes between calls
		got := s.Observed("x")
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d: not sorted by node then stamp: %v", run, got)
		}
	}
}

func TestNotesWithTheSameStampAreOrderedByNode(t *testing.T) {
	var s Set
	for _, who := range []string{"carol", "lidia", "beka"} {
		tag := Tag{who, at(5, 0)}
		s = s.Add(NoteElement("T1", tag, "by "+who), tag)
	}
	var got []string
	for _, n := range TaskNotes(s)["T1"] {
		got = append(got, n.Text)
	}
	if !reflect.DeepEqual(got, []string{"by beka", "by carol", "by lidia"}) {
		t.Errorf("%v", got)
	}
}
