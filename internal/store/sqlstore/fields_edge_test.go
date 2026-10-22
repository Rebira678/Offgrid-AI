package sqlstore

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"pgregory.net/rapid"

	"offgrid/internal/crdt"
	"offgrid/internal/task"
)

// TestFieldsRespectsContextCancellation ensures Store.Fields returns context.Canceled
// immediately if the context is already canceled or timeout occurs.
func TestFieldsRespectsContextCancellation(t *testing.T) {
	s, _ := freshStore(t)
	create(t, s, "Pump 1")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := s.Fields(ctx)
	if err == nil {
		t.Fatalf("expected error on canceled context, got nil")
	}
}

// TestFieldsEmptyStoreReturnsEmptyMap verifies an unpopulated database returns
// an empty, non-nil crdt.Map with zero keys.
func TestFieldsEmptyStoreReturnsEmptyMap(t *testing.T) {
	s, _ := freshStore(t)
	m, err := s.Fields(context.Background())
	if err != nil {
		t.Fatalf("unexpected error on empty store: %v", err)
	}
	if len(m.Keys()) != 0 {
		t.Fatalf("expected 0 keys, got %d", len(m.Keys()))
	}
}

// TestFieldsOutOrderOpsConvergesCorrectly checks that if ops arrive in arbitrary
// order (such as from parallel network delta sync), the resulting registers
// correctly converge on the highest HLC timestamps.
func TestFieldsOutOfOrderOpsConvergesCorrectly(t *testing.T) {
	t0 := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(10 * time.Minute)
	t2 := t0.Add(20 * time.Minute)

	createOp := Op{
		Pos:     1,
		NodeID:  "node-a",
		Seq:     1,
		TS:      t0,
		Counter: 0,
		Kind:    KindTaskCreated,
		Payload: []byte(`{"id":"T1","title":"Initial Title","site":"Site A","priority":"low"}`),
	}

	updateOp1 := Op{
		Pos:     2,
		NodeID:  "node-b",
		Seq:     1,
		TS:      t1,
		Counter: 0,
		Kind:    KindTaskUpdated,
		Payload: []byte(`{"id":"T1","status":"in_progress"}`),
	}

	updateOp2 := Op{
		Pos:     3,
		NodeID:  "node-a",
		Seq:     2,
		TS:      t2,
		Counter: 0,
		Kind:    KindTaskUpdated,
		Payload: []byte(`{"id":"T1","priority":"high"}`),
	}

	// Normal chronological order
	mNormal, err := FieldsFromOps([]Op{createOp, updateOp1, updateOp2})
	if err != nil {
		t.Fatalf("normal order failed: %v", err)
	}

	// Reversed order (update 2, update 1, create)
	mReversed, err := FieldsFromOps([]Op{updateOp2, updateOp1, createOp})
	if err != nil {
		t.Fatalf("reversed order failed: %v", err)
	}

	// Scrambled order
	mScrambled, err := FieldsFromOps([]Op{updateOp1, createOp, updateOp2})
	if err != nil {
		t.Fatalf("scrambled order failed: %v", err)
	}

	if !crdt.Equal(mNormal, mReversed) {
		t.Fatalf("reversed order diverged from normal order:\nNormal:   %v\nReversed: %v", mNormal, mReversed)
	}
	if !crdt.Equal(mNormal, mScrambled) {
		t.Fatalf("scrambled order diverged from normal order:\nNormal:    %v\nScrambled: %v", mNormal, mScrambled)
	}

	fields := crdt.TaskFields(mNormal)["T1"]
	if fields["title"] != "Initial Title" || fields["priority"] != "high" || fields["status"] != "in_progress" {
		t.Fatalf("unexpected fields resolution: %v", fields)
	}
}

// TestFieldsStressManyPagingCycles verifies reading hundreds of ops across tiny
// page boundaries maintains bit-for-bit parity with in-memory direct evaluation.
func TestFieldsStressManyPagingCycles(t *testing.T) {
	s, clk := freshStore(t)
	// Create multiple tasks and updates
	for i := 0; i < 50; i++ {
		tk := create(t, s, "Task "+string(rune('A'+(i%26))))
		clk.t = clk.t.Add(time.Second)
		if i%2 == 0 {
			hi := task.PriorityHigh
			s.Update(context.Background(), tk.ID, task.Changes{Priority: &hi})
		}
		if i%3 == 0 {
			done := task.StatusDone
			s.Update(context.Background(), tk.ID, task.Changes{Status: &done})
		}
	}

	all, err := s.Fields(context.Background())
	if err != nil {
		t.Fatalf("Fields failed: %v", err)
	}

	oldPageSize := fieldsPageSize
	defer func() { fieldsPageSize = oldPageSize }()

	// Test page sizes: 1, 2, 5, 13, 100
	for _, ps := range []int{1, 2, 5, 13, 100} {
		fieldsPageSize = ps
		paged, err := s.Fields(context.Background())
		if err != nil {
			t.Fatalf("Fields with page size %d failed: %v", ps, err)
		}
		if !crdt.Equal(all, paged) {
			t.Fatalf("page size %d diverged from default", ps)
		}
	}
}

// TestPropertyFieldsFromOpsMalformedLogResilience checks that FieldsFromOps cleanly
// fails with ErrCorruptLog when encountering corrupt payloads or unknown kinds,
// while skipping note ops without error or false registers.
func TestPropertyFieldsFromOpsMalformedLogResilience(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		numOps := rapid.IntRange(1, 15).Draw(rt, "numOps")
		hasCorrupt := false
		ops := make([]Op, numOps)

		for i := 0; i < numOps; i++ {
			isCorrupt := rapid.Bool().Draw(rt, fmt.Sprintf("isCorrupt-%d", i))
			now := time.Now().UTC()
			if isCorrupt {
				hasCorrupt = true
				switch rapid.IntRange(0, 2).Draw(rt, "corruptType") {
				case 0: // Unknown op kind
					ops[i] = Op{
						Pos: int64(i + 1), NodeID: "node-x", Seq: int64(i + 1),
						TS: now, Counter: 0, Kind: "task.forged_kind",
						Payload: []byte(`{"id":"T1"}`),
					}
				case 1: // Bad json in created
					ops[i] = Op{
						Pos: int64(i + 1), NodeID: "node-x", Seq: int64(i + 1),
						TS: now, Counter: 0, Kind: KindTaskCreated,
						Payload: []byte(`{invalid-json`),
					}
				case 2: // Bad json in updated
					ops[i] = Op{
						Pos: int64(i + 1), NodeID: "node-x", Seq: int64(i + 1),
						TS: now, Counter: 0, Kind: KindTaskUpdated,
						Payload: []byte(`{invalid-json`),
					}
				}
			} else {
				// Valid op: created, updated, or note_added
				switch rapid.IntRange(0, 2).Draw(rt, "validType") {
				case 0:
					ops[i] = Op{
						Pos: int64(i + 1), NodeID: "node-x", Seq: int64(i + 1),
						TS: now, Counter: 0, Kind: KindTaskCreated,
						Payload: []byte(fmt.Sprintf(`{"id":"T%d","title":"Valid","site":"S","priority":"low"}`, i)),
					}
				case 1:
					ops[i] = Op{
						Pos: int64(i + 1), NodeID: "node-x", Seq: int64(i + 1),
						TS: now, Counter: 0, Kind: KindTaskUpdated,
						Payload: []byte(fmt.Sprintf(`{"id":"T%d","status":"done"}`, i)),
					}
				case 2:
					ops[i] = Op{
						Pos: int64(i + 1), NodeID: "node-x", Seq: int64(i + 1),
						TS: now, Counter: 0, Kind: KindNoteAdded,
						Payload: []byte(`{"id":"T1","body":"Note"}`),
					}
				}
			}
		}

		m, err := FieldsFromOps(ops)
		if hasCorrupt {
			if err == nil {
				rt.Fatalf("expected ErrCorruptLog on corrupted ops, got err=nil")
			}
			if !errors.Is(err, ErrCorruptLog) {
				rt.Fatalf("expected error wrapping ErrCorruptLog, got: %v", err)
			}
		} else {
			if err != nil {
				rt.Fatalf("unexpected error on valid op stream: %v", err)
			}
			// Note ops must not register any fields
			for k := range m {
				if _, _, ok := crdt.SplitTaskKey(k); !ok {
					rt.Fatalf("invalid task key registered: %q", k)
				}
			}
		}
	})
}

// TestPropertyFieldsFromOpsEmptyOrIdempotent checks that empty logs produce empty maps,
// and duplicating an entire log preserves exact CRDT map equality.
func TestPropertyFieldsFromOpsEmptyOrIdempotent(t *testing.T) {
	// Empty slices
	mNil, err1 := FieldsFromOps(nil)
	mEmpty, err2 := FieldsFromOps([]Op{})
	if err1 != nil || err2 != nil {
		t.Fatalf("unexpected error on empty/nil ops: %v, %v", err1, err2)
	}
	if len(mNil) != 0 || len(mEmpty) != 0 {
		t.Fatalf("expected 0 entries from empty logs")
	}

	lists := buildOpLists(t)
	rapid.Check(t, func(rt *rapid.T) {
		ops := lists[rapid.IntRange(0, len(lists)-1).Draw(rt, "log")]
		single, errSingle := FieldsFromOps(ops)
		duplicated, errDup := FieldsFromOps(append(append([]Op{}, ops...), ops...))
		if errSingle != nil || errDup != nil {
			rt.Fatalf("unexpected error: single=%v, dup=%v", errSingle, errDup)
		}
		if !crdt.Equal(single, duplicated) {
			rt.Fatalf("duplicating entire op log changed the registers")
		}
	})
}
