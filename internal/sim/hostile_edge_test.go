package sim

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"testing"

	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
)

// TestMaskDevicesEdgeCases tests device ID normalization in traces and failure messages.
func TestMaskDevicesEdgeCases(t *testing.T) {
	// Case 1: Empty result
	r1 := &Result{}
	maskDevices(r1)
	if len(r1.Trace) != 0 || r1.Failure != nil {
		t.Fatalf("maskDevices on empty result: got %+v", r1)
	}

	// Case 2: Trace with multiple devices, repeats, and failure
	devA := "dev_0123456789abcdef0123456789abcdef"
	devB := "dev_fedcba9876543210fedcba9876543210"
	devC := "dev_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	r2 := &Result{
		Trace: []string{
			fmt.Sprintf("r0 enrolled %s", devA),
			fmt.Sprintf("r1 received op from %s", devB),
			fmt.Sprintf("r0 synced with %s again", devA),
			fmt.Sprintf("r2 revoked %s", devC),
			"unrelated line without device ID",
		},
		Failure: &Failure{
			Class: "test",
			Msg:   fmt.Sprintf("failure involving %s and %s", devB, devA),
		},
	}
	maskDevices(r2)

	// devA was seen first -> dev#1
	// devB was seen second -> dev#2
	// devC was seen third -> dev#3
	wantTrace := []string{
		"r0 enrolled dev#1",
		"r1 received op from dev#2",
		"r0 synced with dev#1 again",
		"r2 revoked dev#3",
		"unrelated line without device ID",
	}
	for i, tr := range r2.Trace {
		if tr != wantTrace[i] {
			t.Errorf("trace[%d] = %q, want %q", i, tr, wantTrace[i])
		}
	}

	wantFailure := "failure involving dev#2 and dev#1"
	if r2.Failure.Msg != wantFailure {
		t.Errorf("failure msg = %q, want %q", r2.Failure.Msg, wantFailure)
	}
}

// TestHostilePreludeInvariants tests securePrelude guarantees across various schedule lengths.
func TestHostilePreludeInvariants(t *testing.T) {
	// Case 1: Short schedule (< 24) is untouched
	shortActs := make([]Action, 10)
	for i := range shortActs {
		shortActs[i] = Action{Kind: "sync", A: 0, B: 1}
	}
	securePrelude(rand.New(rand.NewSource(1)), shortActs, Config{Replicas: 3, Steps: 10, Secure: true})
	for _, a := range shortActs {
		if isHostile(a.Kind) {
			t.Fatalf("short schedule (< 24) was modified with hostile action: %v", a)
		}
	}

	// Case 2: Schedules of length 24, 50, 80, 200 across multiple seeds
	for _, n := range []int{24, 50, 80, 200} {
		for seed := int64(1); seed <= 10; seed++ {
			rng := rand.New(rand.NewSource(seed))
			acts := make([]Action, n)
			for i := range acts {
				acts[i] = Action{Kind: "sync", A: 0, B: 1}
			}
			securePrelude(rng, acts, Config{Replicas: 3, Steps: n, Secure: true})

			var revokes, forgeries, rogues, replays int
			for _, a := range acts {
				switch a.Kind {
				case Revoke:
					revokes++
				case Forge:
					forgeries++
				case Rogue:
					rogues++
				case Replay:
					replays++
				}
			}

			if revokes != 1 {
				t.Fatalf("n=%d seed=%d: revokes=%d, want exactly 1", n, seed, revokes)
			}
			if forgeries < 5 {
				t.Fatalf("n=%d seed=%d: forgeries=%d, want >= 5", n, seed, forgeries)
			}
			if rogues < 3 {
				t.Fatalf("n=%d seed=%d: rogues=%d, want >= 3", n, seed, rogues)
			}
			if replays < 2 {
				t.Fatalf("n=%d seed=%d: replays=%d, want >= 2", n, seed, replays)
			}
		}
	}
}

// TestParseActionHostileEdgeCases validates round-trip and malformed inputs for hostile kinds.
func TestParseActionHostileEdgeCases(t *testing.T) {
	// Valid hostile kinds round-trip
	for _, k := range hostileKinds {
		for _, a := range []Action{
			{Kind: k, A: 0, N: 0},
			{Kind: k, A: 1, N: 42},
			{Kind: k, A: 2, N: 1 << 16},
		} {
			s := a.String()
			parsed, err := ParseAction(s)
			if err != nil {
				t.Fatalf("failed to parse %q: %v", s, err)
			}
			if parsed != a {
				t.Fatalf("round-trip mismatch: got %+v, want %+v", parsed, a)
			}
		}
	}

	// Malformed inputs
	badInputs := []string{
		"",
		"   ",
		"revoke",
		"revoke r0",
		"revoke r0 foo",
		"forge rX 1",
		"rogue r0 -5",
		"replay r9999999999999999999999999999999 1",
		"unknown_hostile_kind r0 1",
	}
	for _, bad := range badInputs {
		if _, err := ParseAction(bad); err == nil {
			t.Errorf("ParseAction(%q) succeeded, want error", bad)
		}
	}
}

// TestRefusedErrorClassification verifies the classification of store errors.
func TestRefusedErrorClassification(t *testing.T) {
	matching := []error{
		sqlstore.ErrNotAuthorized,
		sqlstore.ErrRevoked, // wraps ErrNotAuthorized: a revoked device's write refusal
		sqlstore.ErrBadSignature,
		sqlstore.ErrUnsigned,
		sqlstore.ErrKeyChange,
		sqlstore.ErrBadOp,
		sqlstore.ErrChain,
		sqlstore.ErrFork,
		sqlstore.ErrGap,
		fmt.Errorf("wrapped: %w", sqlstore.ErrNotAuthorized),
		fmt.Errorf("wrapped: %w", sqlstore.ErrBadOp),
	}
	for _, err := range matching {
		if !refused(err) {
			t.Errorf("refused(%v) = false, want true", err)
		}
	}

	nonMatching := []error{
		nil,
		errors.New("generic error"),
		sqlstore.ErrAlreadyRevoked,
		context.Canceled,
		context.DeadlineExceeded,
		task.ErrNotFound,
	}
	for _, err := range nonMatching {
		if refused(err) {
			t.Errorf("refused(%v) = true, want false", err)
		}
	}
}

// TestCaptureBufferBoundedSlidingWindow tests sliding window capacity of w.capture().
func TestCaptureBufferBoundedSlidingWindow(t *testing.T) {
	w := &world{
		cfg: Config{Secure: true},
	}
	// Secure = false drops captures
	wInsecure := &world{
		cfg: Config{Secure: false},
	}
	wInsecure.capture([]sqlstore.Op{{NodeID: "test", Seq: 1}})
	if len(wInsecure.h.captured) != 0 {
		t.Fatal("insecure world captured batches, want 0")
	}

	// Empty slice is dropped
	w.capture(nil)
	w.capture([]sqlstore.Op{})
	if len(w.h.captured) != 0 {
		t.Fatal("empty ops slice captured, want 0")
	}

	// Feed 200 batches to test cap at 128
	for i := 1; i <= 200; i++ {
		w.capture([]sqlstore.Op{{NodeID: "node", Seq: int64(i)}})
	}

	if got := len(w.h.captured); got != 128 {
		t.Fatalf("captured length = %d, want cap of 128", got)
	}
	// The oldest retained should be batch 73 (200 - 128 + 1)
	if first := w.h.captured[0][0].Seq; first != 73 {
		t.Fatalf("first captured seq = %d, want 73", first)
	}
	if last := w.h.captured[127][0].Seq; last != 200 {
		t.Fatalf("last captured seq = %d, want 200", last)
	}
}

// TestCloseHostileIdempotence verifies closeHostile handles nil and repeated calls.
func TestCloseHostileIdempotence(t *testing.T) {
	w := &world{}
	// Should not panic on uninitialized stores
	w.closeHostile()
	w.closeHostile()
}

// TestDoRogueBeforeRevocationIsNoop verifies doRogue exits cleanly when no node is revoked.
func TestDoRogueBeforeRevocationIsNoop(t *testing.T) {
	w := &world{
		cfg: Config{Secure: true},
		h: hostileState{
			revoked: map[int]bool{},
		},
	}
	f := w.doRogue(Action{Kind: Rogue, A: 0, N: 1})
	if f != nil {
		t.Fatalf("doRogue before revocation returned failure: %v", f)
	}
}

// TestDoReplayEmptyCaptureIsNoop verifies doReplay exits cleanly when captured is empty.
func TestDoReplayEmptyCaptureIsNoop(t *testing.T) {
	w := &world{
		cfg: Config{Secure: true},
	}
	f := w.doReplay(Action{Kind: Replay, A: 0, N: 1})
	if f != nil {
		t.Fatalf("doReplay with 0 captures returned failure: %v", f)
	}
}
