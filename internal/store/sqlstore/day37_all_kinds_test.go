package sqlstore

import (
	"context"
	"testing"

	"offgrid/internal/task"
)

// TestDay37CheckpointAllOpKindsSigned verifies that Security Checkpoint 1
// correctly signs every operation kind supported by the engine and stores
// valid ECDSA P-256 signatures in op_sigs.
func TestDay37CheckpointAllOpKindsSigned(t *testing.T) {
	ctx := context.Background()
	devKey := newKey(t)
	clk := &clock{t: epoch}
	s := open(t, newDB(t), Options{NodeID: "rig-a", Now: clk.now, Signer: devKey})

	// 1. Create task
	tk, _, err := s.Create(ctx, newTask("Sensor Calibration"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// 2. Update task
	high := task.PriorityHigh
	prog := task.StatusInProgress
	if _, _, err := s.Update(ctx, tk.ID, task.Changes{Priority: &high, Status: &prog}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// 3. Add note
	if _, err := s.AddNote(ctx, tk.ID, "verified against fluke meter"); err != nil {
		t.Fatalf("AddNote: %v", err)
	}

	// 4. Suggestion
	low := task.PriorityLow
	sug, _, err := s.Suggest(ctx, task.Draft{
		Task:     tk.ID,
		Priority: &low,
		By:       "agent-tier1",
	})
	if err != nil {
		t.Fatalf("Suggest: %v", err)
	}

	// 5. Accept suggestion
	if _, err := s.Resolve(ctx, sug.ID, true); err != nil {
		t.Fatalf("Resolve (accept): %v", err)
	}

	// 6. Another suggestion to dismiss
	sug2, _, err := s.Suggest(ctx, task.Draft{
		Task:     tk.ID,
		Priority: &high,
		By:       "agent-tier1",
	})
	if err != nil {
		t.Fatalf("Suggest 2: %v", err)
	}

	// 7. Dismiss suggestion
	if _, err := s.Resolve(ctx, sug2.ID, false); err != nil {
		t.Fatalf("Resolve (dismiss): %v", err)
	}

	// 8. Record Tier 2 Answer
	if _, err := s.RecordAnswer(ctx, "k-calib-1", "What is sensor voltage?", "3.31V nominal", "qwen2.5:7b", "local"); err != nil {
		t.Fatalf("RecordAnswer: %v", err)
	}

	// 9. Delete task
	if err := s.DeleteTask(ctx, tk.ID); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}

	// Check all ops carry valid signatures
	ops := opsOf(t, s)
	if len(ops) != 9 {
		t.Fatalf("expected 9 ops, got %d", len(ops))
	}

	for i, op := range ops {
		if op.Sig == nil {
			t.Fatalf("op %d (%s/%d, kind %s) is missing signature", i, op.NodeID, op.Seq, op.Kind)
		}
		if op.Sig.Device != devKey.ID() {
			t.Errorf("op %d signed by %s, want %s", i, op.Sig.Device, devKey.ID())
		}
		if op.Sig.Alg != "ecdsa-p256" {
			t.Errorf("op %d alg = %s, want ecdsa-p256", i, op.Sig.Alg)
		}
		if len(op.Sig.Sig) != 64 {
			t.Errorf("op %d sig length = %d, want 64", i, len(op.Sig.Sig))
		}
	}

	// Verify signatures and projection consistency
	rep, err := s.Verify(ctx)
	if err != nil {
		t.Fatalf("Verify failed: %v", err)
	}
	if rep.Sigs.Signed != 9 || rep.Sigs.Unsigned != 0 {
		t.Errorf("Verify report: signed=%d unsigned=%d, want 9/0", rep.Sigs.Signed, rep.Sigs.Unsigned)
	}
	if len(rep.Sigs.Devices) != 1 || rep.Sigs.Devices[0].Device != devKey.ID() {
		t.Errorf("Verify devices: %+v, want 1 device %s", rep.Sigs.Devices, devKey.ID())
	}
}
