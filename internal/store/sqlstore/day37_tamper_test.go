package sqlstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

// TestDay37CheckpointTamperDetectionOnSignedOps verifies that altering any field
// (payload, seq, kind, or previous hash) invalidates the signature and is rejected.
func TestDay37CheckpointTamperDetectionOnSignedOps(t *testing.T) {
	ctx := context.Background()
	devKey := newKey(t)
	orig := signedReplica(t, "node-1", devKey)

	for i := 0; i < 3; i++ {
		if _, _, err := orig.Create(ctx, newTask(fmt.Sprintf("Task %d", i+1))); err != nil {
			t.Fatal(err)
		}
	}
	validOps := opsOf(t, orig)

	receiver := signedReplica(t, "receiver", newKey(t))

	// 1. Tampered payload
	tamperedPayload := make([]Op, len(validOps))
	copy(tamperedPayload, validOps)
	tamperedPayload[1].Payload = json.RawMessage(`{"intent":"create_task","title":"HACKED"}`)
	if _, err := receiver.ApplyRemote(ctx, tamperedPayload); !errors.Is(err, ErrBadSignature) {
		t.Errorf("tampered payload: want ErrBadSignature, got %v", err)
	}

	// 2. Tampered sequence number
	tamperedSeq := make([]Op, len(validOps))
	copy(tamperedSeq, validOps)
	tamperedSeq[1].Seq = 99
	if _, err := receiver.ApplyRemote(ctx, tamperedSeq); err == nil {
		t.Errorf("tampered seq: want error, got nil")
	}

	// 3. Tampered signature bytes
	tamperedSig := make([]Op, len(validOps))
	copy(tamperedSig, validOps)
	badSig := *tamperedSig[1].Sig
	badSig.Sig = make([]byte, 64) // zeroed out
	tamperedSig[1].Sig = &badSig
	if _, err := receiver.ApplyRemote(ctx, tamperedSig); !errors.Is(err, ErrBadSignature) {
		t.Errorf("tampered signature bytes: want ErrBadSignature, got %v", err)
	}
}
