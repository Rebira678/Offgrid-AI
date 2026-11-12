package sqlstore

import (
	"errors"
	"fmt"
	"testing"

	"offgrid/internal/identity"
)

// TestDay37CheckpointVerifyPoolParallelStress verifies that verifyPool handles
// 100+ concurrent signatures deterministically, identifying the lowest-indexed
// invalid signature across multiple parallel workers.
func TestDay37CheckpointVerifyPoolParallelStress(t *testing.T) {
	const total = 120
	keys := []*identity.Identity{newKey(t), newKey(t), newKey(t)}
	jobs := make([]sigJob, total)

	for i := 0; i < total; i++ {
		k := keys[i%len(keys)]
		msg := []byte(fmt.Sprintf("op-hash-%04d", i))
		sig, err := k.Sign([]byte(sigDomain + string(msg)))
		if err != nil {
			t.Fatal(err)
		}
		jobs[i] = sigJob{
			idx:    i,
			key:    k.PublicKey(),
			msg:    []byte(sigDomain + string(msg)),
			sig:    sig,
			node:   fmt.Sprintf("node-%d", i%len(keys)),
			seq:    int64(i + 1),
			device: k.ID(),
		}
	}

	// All valid first
	idx, err := verifyPool(jobs)
	if err != nil || idx != -1 {
		t.Fatalf("all valid: verifyPool returned idx=%d, err=%v", idx, err)
	}

	// Corrupt two signatures: index 27 and index 64
	corrupted := make([]sigJob, len(jobs))
	copy(corrupted, jobs)
	corrupted[27].sig = make([]byte, 64)
	corrupted[64].sig = make([]byte, 64)

	// verifyPool must return lowest failing index (27) deterministically
	for run := 0; run < 10; run++ {
		failIdx, failErr := verifyPool(corrupted)
		if failIdx != 27 {
			t.Fatalf("run %d: expected lowest failure index 27, got %d", run, failIdx)
		}
		if !errors.Is(failErr, ErrBadSignature) {
			t.Fatalf("run %d: expected ErrBadSignature, got %v", run, failErr)
		}
	}
}
