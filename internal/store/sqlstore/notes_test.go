package sqlstore

import (
	"context"
	"reflect"
	"testing"
	"time"


	"offgrid/internal/crdt"
	"offgrid/internal/task"
)

func notesOf(t *testing.T, s *Store) crdt.Set {
	t.Helper()
	set, err := s.Notes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func noteTexts(set crdt.Set, id string) []string {
	var out []string
	for _, n := range crdt.TaskNotes(set)[id] {
		out = append(out, n.Text)
	}
	return out
}

// The OR-Set is a second reading of the same log: for every task it must list
// the notes the SQLite view lists, in the same order.
func TestNoteSetAgreesWithTheMaterializedViewOnRandomWorkloads(t *testing.T) {
	for seed := int64(1); seed <= 12; seed++ {
		s, clk := freshStore(t)
		workload(t, s, clk, seed, 120)
		set := notesOf(t, s)
		tasks, err := s.List(context.Background(), task.Filter{})
		if err != nil {
			t.Fatal(err)
		}
		total := 0
		for _, tk := range tasks {
			var want []string
			for _, n := range tk.Notes {
				want = append(want, n.Text)
			}
			total += len(want)
			if got := noteTexts(set, tk.ID); !reflect.DeepEqual(got, want) && !(len(got) == 0 && len(want) == 0) {
				t.Fatalf("seed %d, %s: set %q, view %q", seed, tk.ID, got, want)
			}
		}
		if n := len(crdt.TaskNotes(set)); n > len(tasks) {
			t.Fatalf("seed %d: notes for %d tasks, only %d exist", seed, n, len(tasks))
		}
		_ = total
	}
}

func TestNoteTagsCarryTheWritingNodeAndTheOpStamp(t *testing.T) {
	s, _ := freshStore(t)
	tk := create(t, s, "Pump")
	if _, err := s.AddNote(context.Background(), tk.ID, "seal cracked"); err != nil {
		t.Fatal(err)
	}
	ops := allOps(t, s)
	notes := crdt.TaskNotes(notesOf(t, s))[tk.ID]
	if len(notes) != 1 || notes[0].Tag.Node != "node-a" || notes[0].Tag.Stamp != ops[1].Stamp() || notes[0].Text != "seal cracked" {
		t.Errorf("%+v, add op stamp %v", notes, ops[1].Stamp())
	}
}

func TestTwoIdenticalNotesAreTwoNotesAndTheTextIsKeptExactly(t *testing.T) {
	s, _ := freshStore(t)
	tk := create(t, s, "Pump")
	for _, txt := range []string{"ok", "ok", "line one\nline two"} {
		if _, err := s.AddNote(context.Background(), tk.ID, txt); err != nil {
			t.Fatal(err)
		}
	}
	if got := noteTexts(notesOf(t, s), tk.ID); !reflect.DeepEqual(got, []string{"ok", "ok", "line one\nline two"}) {
		t.Errorf("%q", got)
	}
}

func TestNotesFromOpsRejectsAnUnknownKindAndABadPayloadAndSkipsTaskOps(t *testing.T) {
	if _, err := NotesFromOps([]Op{{Pos: 1, Kind: "task.teleported"}}); !errorsIs(err, ErrCorruptLog) {
		t.Errorf("unknown kind: %v", err)
	}
	if _, err := NotesFromOps([]Op{{Pos: 1, Kind: KindNoteAdded, Payload: []byte("{")}}); !errorsIs(err, ErrCorruptLog) {
		t.Errorf("bad payload: %v", err)
	}
	set, err := NotesFromOps([]Op{{Pos: 1, Kind: KindTaskCreated, Payload: []byte(`{}`)}, {Pos: 2, Kind: KindTaskUpdated, Payload: []byte(`{}`)}})
	if err != nil || len(set.Elements()) != 0 {
		t.Errorf("task ops are not notes: %v %v", set.Elements(), err)
	}
}

func TestNotesReadAcrossPages(t *testing.T) {
	s, clk := freshStore(t)
	workload(t, s, clk, 5, 50)
	want, err := NotesFromOps(allOps(t, s))
	if err != nil {
		t.Fatal(err)
	}
	old := fieldsPageSize
	defer func() { fieldsPageSize = old }()
	for _, size := range []int{1, 4, 1000} {
		fieldsPageSize = size
		if got := notesOf(t, s); !crdt.EqualSet(got, want) {
			t.Errorf("page size %d: sets differ", size)
		}
	}
}

// ---- two replicas add notes to the same task while apart

func TestConcurrentNotesOnTwoReplicasAreAllKeptInStampOrder(t *testing.T) {
	a, b, id := replicas(t, epoch.Add(time.Minute), epoch.Add(2*time.Minute))
	for _, c := range []struct {
		s    *Store
		text string
	}{{a, "lidia: seal cracked"}, {b, "beka: ordered a part"}, {a, "lidia: part arrives Friday"}} {
		if _, err := c.s.AddNote(context.Background(), id, c.text); err != nil {
			t.Fatal(err)
		}
	}
	m := crdt.MergeSet(notesOf(t, a), notesOf(t, b))
	if !crdt.EqualSet(m, crdt.MergeSet(notesOf(t, b), notesOf(t, a))) {
		t.Fatal("merge order changed the result")
	}
	got := noteTexts(m, id)
	// a's two notes are at +1m and +1m#1; b's is at +2m: stamp order, not replica order
	want := []string{"lidia: seal cracked", "lidia: part arrives Friday", "beka: ordered a part"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestTheSameNoteTextAddedOnTwoReplicasIsTwoNotes(t *testing.T) {
	a, b, id := replicas(t, epoch.Add(time.Minute), epoch.Add(time.Minute))
	for _, s := range []*Store{a, b} {
		if _, err := s.AddNote(context.Background(), id, "needs a new seal"); err != nil {
			t.Fatal(err)
		}
	}
	if got := noteTexts(crdt.MergeSet(notesOf(t, a), notesOf(t, b)), id); len(got) != 2 {
		t.Errorf("two people each wrote the note: %q", got)
	}
}

// A note that one replica removes while another is still looking at it: the
// remove carries the tags it saw, so a note added meanwhile elsewhere survives.
func TestRemovingANoteWhileAnotherReplicaAddsOneKeepsTheNewNote(t *testing.T) {
	a, b, id := replicas(t, epoch.Add(time.Minute), epoch.Add(2*time.Minute))
	if _, err := a.AddNote(context.Background(), id, "old note"); err != nil {
		t.Fatal(err)
	}
	both := notesOf(t, a) // b receives a's note
	aSet := both
	for _, n := range crdt.TaskNotes(aSet)[id] { // Lidia removes it
		aSet, _ = aSet.Remove(crdt.NoteElement(id, n.Tag, n.Text))
	}
	if _, err := b.AddNote(context.Background(), id, "new note"); err != nil {
		t.Fatal(err)
	}
	bSet := crdt.MergeSet(both, notesOf(t, b))
	merged := crdt.MergeSet(aSet, bSet)
	if got := noteTexts(merged, id); !reflect.DeepEqual(got, []string{"new note"}) {
		t.Errorf("the old note is removed and the concurrent new one stays: %q", got)
	}
}

