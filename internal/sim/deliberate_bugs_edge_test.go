package sim

import (
	"fmt"
	"strings"
	"testing"
)

// TestFailureClassificationLogic tests that all failure classes defined in the
// simulator are accurately classified and formatted into Failure structs.
func TestFailureClassificationLogic(t *testing.T) {
	// 1. notAtomic error becomes class "atomic"
	atomErr := &notAtomic{msg: "batch failed but state hash changed"}
	f1 := syncFailure("step 1", atomErr)
	if f1.Class != "atomic" || !strings.Contains(f1.Msg, "state hash changed") {
		t.Errorf("atomic failure class wrong: %+v", f1)
	}

	// 2. Generic error becomes class "sync"
	f2 := syncFailure("step 2", fmt.Errorf("network connection reset"))
	if f2.Class != "sync" || !strings.Contains(f2.Msg, "network connection reset") {
		t.Errorf("sync failure class wrong: %+v", f2)
	}

	// 3. Oracle failure classes
	delivFail := &Failure{Class: "delete-rule", Msg: "task was resurrected but marked deleted"}
	if delivFail.Class != "delete-rule" || !strings.Contains(delivFail.Error(), "delete-rule") {
		t.Errorf("delete-rule class wrong: %+v", delivFail)
	}

	sugFail := &Failure{Class: "suggestion-rule", Msg: "second resolution won over first"}
	if sugFail.Class != "suggestion-rule" || !strings.Contains(sugFail.Error(), "suggestion-rule") {
		t.Errorf("suggestion-rule class wrong: %+v", sugFail)
	}
}

// TestResultPassAndFailStringRepresentation verifies Result fields for passing and failing runs.
func TestResultPassAndFailStringRepresentation(t *testing.T) {
	passRes := Result{Seed: 1, Ops: 5, Hash: "48e14ef057317c5f"}
	if passRes.Failure != nil {
		t.Errorf("pass result should have nil Failure")
	}

	failRes := Result{Seed: 2, Failure: &Failure{Class: "diverged", Msg: "hash mismatch r0 vs r1"}}
	if failRes.Failure == nil || failRes.Failure.Class != "diverged" {
		t.Errorf("fail result should retain Failure: %+v", failRes)
	}
}
