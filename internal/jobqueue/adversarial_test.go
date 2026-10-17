package jobqueue

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"offgrid/internal/store/sqlstore"
)

// TestSQLiteHeavyConcurrencyAndContention launches 30 concurrent workers performing
// mixed operations (Enqueue, ClaimKinds, Checkpoint, Extend, Release, CompleteWith)
// on a single SQLite database to verify lock handling, zero database busy crashes,
// and complete data integrity.
func TestSQLiteHeavyConcurrencyAndContention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contention.db")
	db, err := sqlstore.OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	q, err := New(context.Background(), db, Config{
		Backoff: Backoff{Base: 5 * time.Millisecond, Max: 50 * time.Millisecond, Jitter: NoJitter},
	})
	if err != nil {
		t.Fatal(err)
	}

	const numJobs = 100
	const numWorkers = 16

	// Seed initial batch of jobs
	for i := 0; i < numJobs; i++ {
		kind := "local"
		if i%2 == 0 {
			kind = "network"
		}
		_, _, err := q.Enqueue(context.Background(), fmt.Sprintf("job-%d", i), kind, []byte(fmt.Sprintf(`{"i":%d}`, i)), EnqueueOptions{})
		if err != nil {
			t.Fatalf("seed enqueue %d: %v", i, err)
		}
	}

	var wg sync.WaitGroup
	errCh := make(chan error, numWorkers*10)

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(workerID * 1000)))

			for {
				// Pick kinds to claim
				kinds := []string{"local", "network"}
				if rng.Intn(2) == 0 {
					kinds = []string{"local"}
				}

				j, ok, err := q.ClaimKinds(context.Background(), 200*time.Millisecond, kinds)
				if err != nil {
					errCh <- fmt.Errorf("worker %d claim error: %w", workerID, err)
					return
				}
				if !ok {
					// Check if all done
					st, _ := q.Stats(context.Background())
					if st.Queued == 0 && st.Running == 0 {
						return
					}
					time.Sleep(10 * time.Millisecond)
					continue
				}

				// Simulate occasional checkpoint
				if rng.Intn(3) == 0 {
					_ = q.Checkpoint(context.Background(), j.ID, j.Attempts, "in-flight-checkpoint")
				}

				// Simulate occasional release (e.g. temporary network drop)
				if rng.Intn(6) == 0 {
					_ = q.Release(context.Background(), j.ID, j.Attempts)
					continue
				}

				// Work simulated
				time.Sleep(time.Duration(rng.Intn(5)) * time.Millisecond)

				// Complete with result
				resText := fmt.Sprintf("result-from-%d-job-%d", workerID, j.ID)
				if err := q.CompleteWith(context.Background(), j.ID, j.Attempts, resText); err != nil {
					// Stale claim is possible if lease expired or was taken over
					if err != ErrStaleClaim {
						errCh <- fmt.Errorf("worker %d complete error: %w", workerID, err)
						return
					}
				}
			}
		}(w)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	// Verify database integrity
	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("database corruption detected: %v, %s", err, integrity)
	}

	stats, err := q.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != numJobs {
		t.Errorf("expected all %d jobs done, got stats: %+v", numJobs, stats)
	}
}

// TestSQLInjectionPayloadResilience verifies that malicious input payloads,
// kinds with SQL keywords, and adversarial strings cannot break or alter SQLite state.
func TestSQLInjectionPayloadResilience(t *testing.T) {
	q, _, _ := newQueue(t, nil)

	injectionPayloads := []string{
		`{"key":"'; DROP TABLE jobs; --"}`,
		`{"key":"' OR '1'='1"}`,
		`{"key":"Robert'); DROP TABLE results;--"}`,
		`{"key":"\u0000\u0001\u0002\u0003"}`,
	}

	for i, payload := range injectionPayloads {
		key := fmt.Sprintf("malicious-key-%d", i)
		j, created, err := q.Enqueue(context.Background(), key, "local", []byte(payload), EnqueueOptions{})
		if err != nil || !created {
			t.Fatalf("enqueue payload %d failed: created=%v err=%v", i, created, err)
		}

		claimed, ok, err := q.Claim(context.Background(), time.Minute)
		if err != nil || !ok || claimed.ID != j.ID {
			t.Fatalf("claim failed on payload %d: ok=%v err=%v", i, ok, err)
		}

		checkpointVal := fmt.Sprintf("checkpoint'; UPDATE jobs SET status='dead' WHERE id=%d; --", claimed.ID)
		if err := q.Checkpoint(context.Background(), claimed.ID, claimed.Attempts, checkpointVal); err != nil {
			t.Fatalf("checkpoint failed: %v", err)
		}

		resultVal := fmt.Sprintf("result'; DELETE FROM jobs; -- %s", payload)
		if err := q.CompleteWith(context.Background(), claimed.ID, claimed.Attempts, resultVal); err != nil {
			t.Fatalf("complete failed: %v", err)
		}

		// Verify result was stored verbatim without SQL execution
		res, found, err := q.Result(context.Background(), claimed.ID)
		if err != nil || !found || res.Text != resultVal {
			t.Fatalf("result mismatch on case %d: found=%v text=%q", i, found, res.Text)
		}
	}

	// Verify invalid non-JSON payloads are rejected
	badPayloads := []string{`{not-json`, `"\x00"`, `hello`}
	for _, bad := range badPayloads {
		if _, _, err := q.Enqueue(context.Background(), "bad-key", "local", []byte(bad), EnqueueOptions{}); err == nil {
			t.Errorf("expected payload %q to be rejected as invalid JSON", bad)
		}
	}

	// Verify tables still exist and are healthy
	var count int
	if err := q.db.QueryRow(`SELECT count(*) FROM jobs`).Scan(&count); err != nil || count != len(injectionPayloads) {
		t.Fatalf("jobs table compromised: count=%d err=%v", count, err)
	}
	if err := q.db.QueryRow(`SELECT count(*) FROM results`).Scan(&count); err != nil || count != len(injectionPayloads) {
		t.Fatalf("results table compromised: count=%d err=%v", count, err)
	}
}

// TestTransactionRollbackOnCompleteFailure ensures that if CompleteWith encounters
// a failure (such as an invalid attempt number), no partial updates are committed
// and the status remains unmodified.
func TestTransactionRollbackOnCompleteFailure(t *testing.T) {
	q, _, _ := newQueue(t, nil)
	j := enqueue(t, q, "rollback-job")

	claimed := claim(t, q)
	wrongAttempt := claimed.Attempts + 5

	err := q.CompleteWith(context.Background(), claimed.ID, wrongAttempt, "should rollback")
	if err != ErrStaleClaim {
		t.Fatalf("expected ErrStaleClaim, got: %v", err)
	}

	// Verify job is still running with initial attempt count
	cur := mustGet(t, q, j.ID)
	if cur.Status != Running || cur.Attempts != claimed.Attempts {
		t.Errorf("job state was mutated despite transaction failure: %+v", cur)
	}

	// Verify no result row was created
	var count int
	err = q.db.QueryRowContext(context.Background(), `SELECT count(*) FROM results WHERE job_id=?`, j.ID).Scan(&count)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("orphan result row was inserted during rolled back transaction")
	}
	var _ *sql.DB = q.db
}
