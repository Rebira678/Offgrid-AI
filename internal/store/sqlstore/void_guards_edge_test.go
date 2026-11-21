package sqlstore

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"offgrid/internal/task"
)

// TestTrueCorruptionIsNeverTolerated ensures that missing entities that are NOT void
// are strictly refused with ErrCorruptLog across all operation kinds.
func TestTrueCorruptionIsNeverTolerated(t *testing.T) {
	ctx := context.Background()
	e := trio(t)

	badOp := func(kind string, payload string) Op {
		return Op{
			NodeID:  "ghost",
			Seq:     1,
			Kind:    kind,
			Payload: json.RawMessage(payload),
		}
	}

	tests := []struct {
		name string
		op   Op
	}{
		{
			name: "update non-existent task",
			op:   badOp(KindTaskUpdated, `{"id":"T_NEVER_EXISTED","status":"done"}`),
		},
		{
			name: "add note to non-existent task",
			op:   badOp(KindNoteAdded, `{"id":"T_NEVER_EXISTED","text":"lost note"}`),
		},
		{
			name: "remove note from non-existent task",
			op:   badOp(KindNoteRemoved, `{"id":"T_NEVER_EXISTED","node":"ghost","seq":99}`),
		},
		{
			name: "delete non-existent task",
			op:   badOp(KindTaskRemoved, `{"id":"T_NEVER_EXISTED","seen":{"laptop":1}}`),
		},
		{
			name: "suggest on non-existent task",
			op:   badOp(KindSuggestionMade, `{"task":"T_NEVER_EXISTED","status":"done","base_status":"open","base_priority":"medium","by":"ghost"}`),
		},
		{
			name: "resolve non-existent suggestion",
			op:   badOp(KindSuggestionResolved, `{"node":"ghost","seq":9999,"resolution":"accepted"}`),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := e.a.write(ctx, func(tx execer) error {
				return applyOp(ctx, tx, tc.op)
			})
			if err == nil || !errors.Is(err, ErrCorruptLog) {
				t.Fatalf("%s: expected ErrCorruptLog, got %v", tc.name, err)
			}
		})
	}
}

// TestRevokedDeviceAllLocalWriteOperationsRefused asserts that every mutating
// Store operation is rejected with ErrRevoked when invoked on a revoked device.
func TestRevokedDeviceAllLocalWriteOperationsRefused(t *testing.T) {
	ctx := context.Background()
	e := trio(t)

	// Set up tasks and notes on B before revocation so that targets exist
	kept := create(t, e.b, "initial task")
	kept2 := create(t, e.b, "task to delete")
	if _, err := e.b.AddNote(ctx, kept.ID, "initial note"); err != nil {
		t.Fatal(err)
	}
	stOpen := task.StatusOpen
	sug, _, err := e.b.Suggest(ctx, task.Draft{
		Task:   kept.ID,
		Note:   "initial sug",
		By:     "pi",
		Key:    "k-init",
		Status: &stOpen,
	})
	if err != nil {
		t.Fatal(err)
	}

	pull(t, e.a, e.b, 50)

	// Admin revokes B
	e.clkA.tick(time.Minute)
	if _, err := e.a.Revoke(ctx, e.idb.ID()); err != nil {
		t.Fatal(err)
	}

	// B pulls revocation
	pull(t, e.b, e.a, 50)

	// 1. Create
	if _, _, err := e.b.Create(ctx, task.NewTask{Title: "after revoke", Priority: task.PriorityMedium}); !errors.Is(err, ErrRevoked) {
		t.Fatalf("Create: got %v, want ErrRevoked", err)
	}

	// 2. Update
	st := task.StatusDone
	if _, _, err := e.b.Update(ctx, kept.ID, task.Changes{Status: &st}); !errors.Is(err, ErrRevoked) {
		t.Fatalf("Update: got %v, want ErrRevoked", err)
	}

	// 3. AddNote
	if _, err := e.b.AddNote(ctx, kept.ID, "revoked note"); !errors.Is(err, ErrRevoked) {
		t.Fatalf("AddNote: got %v, want ErrRevoked", err)
	}

	// 4. RemoveNote
	if _, err := e.b.RemoveNote(ctx, kept.ID, 1); !errors.Is(err, ErrRevoked) {
		t.Fatalf("RemoveNote: got %v, want ErrRevoked", err)
	}

	// 5. DeleteTask
	if err := e.b.DeleteTask(ctx, kept2.ID); !errors.Is(err, ErrRevoked) {
		t.Fatalf("DeleteTask: got %v, want ErrRevoked", err)
	}

	// 6. Suggest
	if _, _, err := e.b.Suggest(ctx, task.Draft{
		Task:   kept.ID,
		Note:   "revoked sug",
		By:     "pi",
		Key:    "k-rev",
		Status: &st,
	}); !errors.Is(err, ErrRevoked) {
		t.Fatalf("Suggest: got %v, want ErrRevoked", err)
	}

	// 7. Resolve
	if _, err := e.b.Resolve(ctx, sug.ID, false); !errors.Is(err, ErrRevoked) {
		t.Fatalf("Resolve: got %v, want ErrRevoked", err)
	}
}

// TestRefuseIfRevokedUnsignedOrEmptyFastPath tests refuseIfRevoked when signer is nil
// or when no revocation ops exist in the store.
func TestRefuseIfRevokedUnsignedOrEmptyFastPath(t *testing.T) {
	ctx := context.Background()

	// Store with no signer
	s, err := Open(ctx, t.TempDir()+"/unsigned.db", Options{NodeID: "unsigned_node"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	err = s.write(ctx, func(tx execer) error {
		return s.refuseIfRevoked(ctx, tx, KindTaskCreated, 1)
	})
	if err != nil {
		t.Fatalf("refuseIfRevoked on unsigned store: got %v, want nil", err)
	}

	// Store with signer but zero KindMemberRevoke ops
	id := newKey(t)
	s2, err := Open(ctx, t.TempDir()+"/signed.db", Options{NodeID: "signed_node", Signer: id})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	if err := s2.Found(ctx, "signed_node"); err != nil {
		t.Fatal(err)
	}
	err = s2.write(ctx, func(tx execer) error {
		return s2.refuseIfRevoked(ctx, tx, KindTaskCreated, 2)
	})
	if err != nil {
		t.Fatalf("refuseIfRevoked with 0 revokes: got %v, want nil", err)
	}
}

// TestVoidContextCancellation verifies that taskIsVoid, suggestionIsVoid and opIsVoid
// honor context cancellation immediately without panicking.
func TestVoidContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-canceled

	e := trio(t)

	if _, err := taskIsVoid(ctx, e.a.db, "T1.laptop"); err == nil {
		t.Fatal("taskIsVoid with canceled ctx: want error, got nil")
	}

	if _, err := suggestionIsVoid(ctx, e.a.db, "laptop", 1); err == nil {
		t.Fatal("suggestionIsVoid with canceled ctx: want error, got nil")
	}

	if _, err := opIsVoid(ctx, e.a.db, "laptop", 1); err == nil {
		t.Fatal("opIsVoid with canceled ctx: want error, got nil")
	}
}
