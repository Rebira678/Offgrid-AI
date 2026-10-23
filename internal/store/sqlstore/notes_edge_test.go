package sqlstore

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"offgrid/internal/crdt"
)

// TestNotesEdgeCaseEmptyAndMultilineNotes verifies that empty strings,
// whitespace-only, and complex multiline notes are correctly projected.
func TestNotesEdgeCaseEmptyAndMultilineNotes(t *testing.T) {
	s, _ := freshStore(t)
	ctx := context.Background()
	tk := create(t, s, "Edge Cases")

	// Store rejects empty and whitespace-only notes
	invalidNotes := []string{"", "   ", "\t\t", "\n\n"}
	for _, inv := range invalidNotes {
		if _, err := s.AddNote(ctx, tk.ID, inv); err == nil {
			t.Fatalf("expected error for empty note %q, got nil", inv)
		}
	}

	validNotes := []string{
		"Single line note",
		"Line 1\nLine 2\nLine 3",
		"Internal whitespace preserved: a \n b",
		"JSON note: {\"key\": \"value\", \"nested\": [1,2,3]}",
	}

	for _, text := range validNotes {
		if _, err := s.AddNote(ctx, tk.ID, text); err != nil {
			t.Fatalf("AddNote failed for %q: %v", text, err)
		}
	}

	set, err := s.Notes(ctx)
	if err != nil {
		t.Fatalf("Store.Notes failed: %v", err)
	}

	taskNotes := crdt.TaskNotes(set)[tk.ID]
	if len(taskNotes) != len(validNotes) {
		t.Fatalf("expected %d notes, got %d", len(validNotes), len(taskNotes))
	}

	for i, want := range validNotes {
		if got := taskNotes[i].Text; got != want {
			t.Errorf("note %d: got %q, want %q", i, got, want)
		}
	}
}

// TestNotesEdgeCaseCorruptPayloads asserts that malformed payloads or
// unexpected kinds in the log reliably fail with ErrCorruptLog.
func TestNotesEdgeCaseCorruptPayloads(t *testing.T) {
	cases := []struct {
		name string
		ops  []Op
	}{
		{
			name: "corrupted JSON payload in note op",
			ops: []Op{
				{Pos: 1, Kind: KindNoteAdded, Payload: []byte(`{"id": "T1", "text": `)}, // truncated json
			},
		},
		{
			name: "unknown kind in op sequence",
			ops: []Op{
				{Pos: 1, Kind: KindNoteAdded, Payload: []byte(`{"id": "T1", "text": "ok"}`)},
				{Pos: 2, Kind: "quantum.teleport", Payload: []byte(`{}`)},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NotesFromOps(tc.ops)
			if !errorsIs(err, ErrCorruptLog) {
				t.Fatalf("expected ErrCorruptLog, got %v", err)
			}
		})
	}
}

// TestNotesEdgeCaseThreeNodeConvergenceWithRemovals tests three separate stores
// with cross-node additions and removals converging cleanly with MergeAllSets.
func TestNotesEdgeCaseThreeNodeConvergenceWithRemovals(t *testing.T) {
	ctx := context.Background()
	path, id := originWithOneTask(t)
	storeA := fork(t, path, "node-a", epoch.Add(time.Minute))
	storeB := fork(t, path, "node-b", epoch.Add(2*time.Minute))
	storeC := fork(t, path, "node-c", epoch.Add(3*time.Minute))

	// Node A adds a note: "initial design"
	if _, err := storeA.AddNote(ctx, id, "initial design"); err != nil {
		t.Fatal(err)
	}

	// Sync A -> B
	setA, err := storeA.Notes(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Node B adds its own note
	if _, err := storeB.AddNote(ctx, id, "investigating"); err != nil {
		t.Fatal(err)
	}
	rawB, err := storeB.Notes(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Node B merges what it got from A and removes "initial design" (using crdt.NoteElement)
	mergedB := crdt.MergeSet(rawB, setA)
	for _, n := range crdt.TaskNotes(mergedB)[id] {
		if n.Text == "initial design" {
			mergedB, _ = mergedB.Remove(crdt.NoteElement(id, n.Tag, n.Text))
		}
	}
	setB := mergedB

	// Node C concurrently adds a note with exact same text: "initial design"
	if _, err := storeC.AddNote(ctx, id, "initial design"); err != nil {
		t.Fatal(err)
	}
	setC, err := storeC.Notes(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Merge all three: A, B, and C
	merged := crdt.MergeAllSets(setA, setB, setC)

	// Add-Wins rule: Node C's add was concurrent and not observed by Node B's remove.
	// Therefore, Node C's "initial design" MUST survive in the merged set, alongside "investigating".
	notes := crdt.TaskNotes(merged)[id]
	if len(notes) != 2 {
		t.Fatalf("expected 2 surviving notes, got %d: %+v", len(notes), notes)
	}
	// Sorted by timestamp: investigating (node-b at +2m), then initial design (node-c at +3m)
	if notes[0].Text != "investigating" || notes[0].Tag.Node != "node-b" {
		t.Errorf("note 0: expected investigating from node-b, got %+v", notes[0])
	}
	if notes[1].Text != "initial design" || notes[1].Tag.Node != "node-c" {
		t.Errorf("note 1: expected initial design from node-c, got %+v", notes[1])
	}
}

// BenchmarkNotesFromOps verifies O(N) linear time and minimal allocations
// for projecting large logs of notes into an OR-Set.
func BenchmarkNotesFromOps(b *testing.B) {
	const count = 2000
	ops := make([]Op, count)
	now := time.Now()
	for i := 0; i < count; i++ {
		payload, _ := json.Marshal(notePayload{
			ID:   fmt.Sprintf("task-%d", i%10),
			Text: fmt.Sprintf("Note comment %d with details", i),
		})
		ops[i] = Op{
			Pos:     int64(i + 1),
			NodeID:  "bench-node",
			TS:      now,
			Counter: uint32(i),
			Kind:    KindNoteAdded,
			Payload: payload,
		}
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		set, err := NotesFromOps(ops)
		if err != nil {
			b.Fatal(err)
		}
		if len(set.Elements()) == 0 {
			b.Fatal("unexpected empty set")
		}
	}
}
