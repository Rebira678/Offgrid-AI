package jobqueue

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestClaimKindsConcurrentDisjointWorkers verifies that multiple concurrent workers,
// each filtering for a specific kind, only claim their assigned kinds without race
// conditions, lost claims, or kind crosstalk.
func TestClaimKindsConcurrentDisjointWorkers(t *testing.T) {
	q, _, _ := newQueue(t, nil)
	kinds := []string{"compute", "sync", "analytics"}
	const jobsPerKind = 20

	// Interleave enqueueing
	for i := 0; i < jobsPerKind; i++ {
		for _, k := range kinds {
			key := fmt.Sprintf("%s-%d", k, i)
			enqueueKind(t, q, key, k)
		}
	}

	var wg sync.WaitGroup
	errCh := make(chan error, len(kinds)*5)

	// Launch multiple workers for each kind
	for _, targetKind := range kinds {
		for w := 0; w < 3; w++ {
			wg.Add(1)
			go func(kind string) {
				defer wg.Done()
				claimed := 0
				for {
					j, ok, err := q.ClaimKinds(context.Background(), time.Minute, []string{kind})
					if err != nil {
						errCh <- fmt.Errorf("worker for %s error: %w", kind, err)
						return
					}
					if !ok {
						break
					}
					if j.Kind != kind {
						errCh <- fmt.Errorf("worker for %s claimed unexpected kind %s (job %d)", kind, j.Kind, j.ID)
						return
					}
					claimed++
					if err := q.CompleteWith(context.Background(), j.ID, j.Attempts, fmt.Sprintf("processed by %s", kind)); err != nil {
						errCh <- fmt.Errorf("complete error: %w", err)
						return
					}
				}
			}(targetKind)
		}
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	stats, err := q.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantTotal := jobsPerKind * len(kinds)
	if stats.Done != wantTotal || stats.Queued != 0 || stats.Running != 0 || stats.Dead != 0 {
		t.Errorf("stats after disjoint concurrent drain: %+v (want done=%d)", stats, wantTotal)
	}
}

// TestClaimKindsSpecialCharacters ensures kind strings with dots, hyphens,
// underscores, and digits work seamlessly with parameter binding and that invalid
// kinds are rejected.
func TestClaimKindsSpecialCharacters(t *testing.T) {
	q, _, _ := newQueue(t, nil)
	validKinds := []string{
		"sync.v1",
		"ai.eval_0",
		"model-run",
		"task_99",
	}

	for _, k := range validKinds {
		enqueueKind(t, q, "key-"+k, k)
	}

	for _, k := range validKinds {
		j, ok, err := q.ClaimKinds(context.Background(), time.Minute, []string{k})
		if err != nil || !ok {
			t.Fatalf("failed to claim kind %q: ok=%v, err=%v", k, ok, err)
		}
		if j.Kind != k {
			t.Errorf("claimed kind %q, want %q", j.Kind, k)
		}
		if err := q.CompleteWith(context.Background(), j.ID, j.Attempts, "done"); err != nil {
			t.Fatalf("CompleteWith: %v", err)
		}
	}

	// Verify invalid kinds are strictly rejected at enqueue time
	invalidKinds := []string{"Sync", "sync:v1", "task 1", "-bad", "123"}
	for _, bad := range invalidKinds {
		_, _, err := q.Enqueue(context.Background(), "k", bad, []byte(`{}`), EnqueueOptions{})
		if err == nil {
			t.Errorf("expected enqueue of kind %q to fail validation", bad)
		}
	}
}

// TestCompleteWithPayloadEdgeCases checks empty results, max payload size,
// and UTF-8 multi-byte emoji content.
func TestCompleteWithPayloadEdgeCases(t *testing.T) {
	q, _, _ := newQueue(t, nil)

	testCases := []struct {
		name    string
		payload string
	}{
		{"empty string", ""},
		{"unicode and emoji", "Operation succeeded! 🚀🔋⚡️ Temperature: 25°C — OK."},
		{"exact max payload", strings.Repeat("a", maxPayload)},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			j := enqueueKind(t, q, "key-"+tc.name, "edge_case")
			claimed, ok, err := q.Claim(context.Background(), time.Minute)
			if err != nil || !ok || claimed.ID != j.ID {
				t.Fatalf("claim failed: ok=%v err=%v", ok, err)
			}

			if err := q.CompleteWith(context.Background(), claimed.ID, claimed.Attempts, tc.payload); err != nil {
				t.Fatalf("CompleteWith failed on %s: %v", tc.name, err)
			}

			res, found, err := q.Result(context.Background(), claimed.ID)
			if err != nil || !found {
				t.Fatalf("Result failed: found=%v err=%v", found, err)
			}
			if res.Text != tc.payload {
				t.Errorf("result text mismatch: got %d bytes, want %d bytes", len(res.Text), len(tc.payload))
			}
		})
	}
}

// TestCheckpointProgressionAcrossLeaseExpirations simulates a long-running job
// that saves progressive milestones ("init", "downloaded", "transformed"),
// suffers lease expiration after each milestone, and proves each successor worker
// resumes from the latest checkpoint.
func TestCheckpointProgressionAcrossLeaseExpirations(t *testing.T) {
	q, clk, _ := newQueue(t, nil)
	enqueue(t, q, "milestone-job")

	milestones := []string{"milestone-1-init", "milestone-2-fetched", "milestone-3-verified"}

	for i, m := range milestones {
		j := claim(t, q)
		if i > 0 && j.Progress != milestones[i-1] {
			t.Fatalf("expected progress %q upon claim %d, got %q", milestones[i-1], i+1, j.Progress)
		}
		if err := q.Checkpoint(context.Background(), j.ID, j.Attempts, m); err != nil {
			t.Fatalf("Checkpoint %q failed: %v", m, err)
		}
		// Simulate worker crash / lease expiration
		clk.advance(2 * time.Minute)
	}

	// Final worker finishes the job
	finalClaim := claim(t, q)
	if finalClaim.Progress != milestones[len(milestones)-1] {
		t.Errorf("final claim progress = %q, want %q", finalClaim.Progress, milestones[len(milestones)-1])
	}
	if err := q.CompleteWith(context.Background(), finalClaim.ID, finalClaim.Attempts, "all milestones completed"); err != nil {
		t.Fatalf("final CompleteWith failed: %v", err)
	}

	res, found, err := q.Result(context.Background(), finalClaim.ID)
	if err != nil || !found || res.Text != "all milestones completed" {
		t.Fatalf("unexpected result: %+v found=%v err=%v", res, found, err)
	}
}

// TestReleaseRestoresImmediateClaimability verifies that calling Release
// leaves run_at ready for immediate claim without applying exponential backoff delay.
func TestReleaseRestoresImmediateClaimability(t *testing.T) {
	q, _, _ := newQueue(t, nil)
	j := enqueue(t, q, "immediate-release")

	c1 := claim(t, q)
	if c1.ID != j.ID || c1.Attempts != 1 {
		t.Fatalf("claim 1 unexpected: %+v", c1)
	}

	if err := q.Release(context.Background(), c1.ID, c1.Attempts); err != nil {
		t.Fatalf("Release failed: %v", err)
	}

	// Immediately claim again without advancing clock
	c2, ok, err := q.Claim(context.Background(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("re-claim after release failed: ok=%v err=%v", ok, err)
	}
	if c2.ID != j.ID || c2.Attempts != 1 {
		t.Errorf("claim 2 unexpected: %+v (want attempts=1 because attempt was handed back)", c2)
	}
}

// TestReleaseOnNonRunningJobFails verifies that releasing a job that is already
// queued, done, or dead returns ErrStaleClaim or ErrNotFound.
func TestReleaseOnNonRunningJobFails(t *testing.T) {
	q, _, _ := newQueue(t, nil)
	j := enqueue(t, q, "queued-job")

	// Release while status is queued
	if err := q.Release(context.Background(), j.ID, 0); !errors.Is(err, ErrStaleClaim) {
		t.Errorf("releasing queued job: got %v, want ErrStaleClaim", err)
	}

	// Claim and complete it
	claimed := claim(t, q)
	if err := q.CompleteWith(context.Background(), claimed.ID, claimed.Attempts, "done"); err != nil {
		t.Fatal(err)
	}

	// Release while status is done
	if err := q.Release(context.Background(), claimed.ID, claimed.Attempts); !errors.Is(err, ErrStaleClaim) {
		t.Errorf("releasing done job: got %v, want ErrStaleClaim", err)
	}
}
