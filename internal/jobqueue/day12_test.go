package jobqueue

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"offgrid/internal/store/sqlstore"
)

func enqueueKind(t *testing.T, q *Queue, key, kind string) Job {
	t.Helper()
	j, created, err := q.Enqueue(ctx, key, kind, json.RawMessage(`{}`), EnqueueOptions{})
	if err != nil || !created {
		t.Fatalf("Enqueue(%s): created=%v err=%v", key, created, err)
	}
	return j
}

// ---- ClaimKinds -------------------------------------------------------------

func TestClaimKindsTakesOnlyTheRequestedKinds(t *testing.T) {
	q, _, _ := newQueue(t, nil)
	a := enqueueKind(t, q, "a", "deep_think")
	b := enqueueKind(t, q, "b", "interpret")
	c := enqueueKind(t, q, "c", "deep_think")

	j, ok, err := q.ClaimKinds(ctx, time.Minute, []string{"interpret"})
	if err != nil || !ok || j.ID != b.ID {
		t.Fatalf("got %+v ok=%v err=%v, want job %d", j, ok, err, b.ID)
	}
	if _, ok, _ := q.ClaimKinds(ctx, time.Minute, []string{"interpret"}); ok {
		t.Error("claimed a second interpret job that does not exist")
	}
	// The jobs of the other kind were not touched: no attempt burned.
	for _, id := range []int64{a.ID, c.ID} {
		if got := mustGet(t, q, id); got.Attempts != 0 || got.Status != Queued {
			t.Errorf("job %d was touched while filtering: %+v", id, got)
		}
	}
}

func TestClaimKindsKeepsOrderWithinTheKinds(t *testing.T) {
	q, _, _ := newQueue(t, nil)
	enqueueKind(t, q, "1", "x")
	second := enqueueKind(t, q, "2", "y")
	third := enqueueKind(t, q, "3", "y")
	got1, _, _ := q.ClaimKinds(ctx, time.Minute, []string{"y", "z"})
	got2, _, _ := q.ClaimKinds(ctx, time.Minute, []string{"y", "z"})
	if got1.ID != second.ID || got2.ID != third.ID {
		t.Errorf("order %d,%d want %d,%d", got1.ID, got2.ID, second.ID, third.ID)
	}
}

func TestClaimKindsEmptyListClaimsNothing(t *testing.T) {
	q, _, _ := newQueue(t, nil)
	enqueueKind(t, q, "a", "x")
	for _, kinds := range [][]string{nil, {}} {
		if _, ok, err := q.ClaimKinds(ctx, time.Minute, kinds); ok || err != nil {
			t.Errorf("kinds=%v: ok=%v err=%v", kinds, ok, err)
		}
	}
	if got := mustGet(t, q, 1); got.Attempts != 0 {
		t.Error("an empty filter touched a job")
	}
}

func TestClaimKindsReclaimsExpiredLeasesOfThatKindOnly(t *testing.T) {
	q, clk, _ := newQueue(t, nil)
	enqueueKind(t, q, "a", "net")
	enqueueKind(t, q, "b", "local")
	q.Claim(ctx, time.Minute) // a, running
	q.Claim(ctx, time.Minute) // b, running
	clk.advance(2 * time.Minute)
	j, ok, _ := q.ClaimKinds(ctx, time.Minute, []string{"local"})
	if !ok || j.Kind != "local" || j.Attempts != 2 {
		t.Errorf("got %+v ok=%v", j, ok)
	}
	if got := mustGet(t, q, 1); got.Attempts != 1 {
		t.Errorf("the net job's expired lease was reclaimed by a local-only claim: %+v", got)
	}
}

func TestClaimKindsIsSafeAgainstOddKindText(t *testing.T) {
	q, _, _ := newQueue(t, nil)
	enqueueKind(t, q, "a", "x")
	// Kinds go in as arguments, never into the SQL text.
	if _, ok, err := q.ClaimKinds(ctx, time.Minute, []string{"x') OR 1=1 --"}); ok || err != nil {
		t.Errorf("ok=%v err=%v", ok, err)
	}
}

// ---- CompleteWith / Result ----------------------------------------------------

func TestCompleteWithStoresTheResultAtomically(t *testing.T) {
	q, _, _ := newQueue(t, nil)
	enqueue(t, q, "a")
	j := claim(t, q)
	if _, found, _ := q.Result(ctx, j.ID); found {
		t.Fatal("a result before completion")
	}
	if err := q.CompleteWith(ctx, j.ID, j.Attempts, "the answer"); err != nil {
		t.Fatal(err)
	}
	r, found, err := q.Result(ctx, j.ID)
	if err != nil || !found || r.Text != "the answer" || r.Attempt != j.Attempts || r.At.IsZero() {
		t.Errorf("result = %+v found=%v err=%v", r, found, err)
	}
	if got := mustGet(t, q, j.ID); got.Status != Done {
		t.Errorf("status %s", got.Status)
	}
}

// The heart of "no duplicates": the worker that lost its claim reports late,
// and neither the status nor the stored result changes.
func TestStaleWorkerCannotOverwriteTheResult(t *testing.T) {
	q, clk, _ := newQueue(t, nil)
	enqueue(t, q, "a")
	old := claim(t, q)
	clk.advance(2 * time.Minute)
	fresh := claim(t, q) // the lease expired; a second worker owns it now
	if fresh.Attempts != 2 {
		t.Fatal("setup")
	}
	if err := q.CompleteWith(ctx, fresh.ID, fresh.Attempts, "from the new owner"); err != nil {
		t.Fatal(err)
	}
	err := q.CompleteWith(ctx, old.ID, old.Attempts, "from the stale worker")
	if !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("stale CompleteWith: %v", err)
	}
	r, _, _ := q.Result(ctx, old.ID)
	if r.Text != "from the new owner" || r.Attempt != 2 {
		t.Errorf("the stale worker changed the result: %+v", r)
	}
}

func TestStaleCompleteWithLeavesNoResultBehind(t *testing.T) {
	q, clk, _ := newQueue(t, nil)
	enqueue(t, q, "a")
	old := claim(t, q)
	clk.advance(2 * time.Minute)
	claim(t, q) // new owner, still running
	if err := q.CompleteWith(ctx, old.ID, old.Attempts, "late"); !errors.Is(err, ErrStaleClaim) {
		t.Fatal(err)
	}
	if _, found, _ := q.Result(ctx, old.ID); found {
		t.Error("a rejected completion left its result in the table: the two writes are not one transaction")
	}
}

func TestCompleteWithLimitsAndUnknownJob(t *testing.T) {
	q, _, _ := newQueue(t, nil)
	enqueue(t, q, "a")
	j := claim(t, q)
	if err := q.CompleteWith(ctx, j.ID, j.Attempts, strings.Repeat("x", maxPayload+1)); err == nil {
		t.Error("oversized result accepted")
	}
	if got := mustGet(t, q, j.ID); got.Status != Running {
		t.Errorf("a refused result must not complete the job: %s", got.Status)
	}
	if err := q.CompleteWith(ctx, 999, 1, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown job: %v", err)
	}
}

func TestPlainCompleteHasNoResult(t *testing.T) {
	q, _, _ := newQueue(t, nil)
	enqueue(t, q, "a")
	j := claim(t, q)
	q.Complete(ctx, j.ID, j.Attempts)
	if _, found, _ := q.Result(ctx, j.ID); found {
		t.Error("Complete invented a result")
	}
}

// ---- Checkpoint ---------------------------------------------------------------

func TestCheckpointSurvivesRetriesAndLostWorkers(t *testing.T) {
	q, clk, _ := newQueue(t, nil)
	enqueue(t, q, "a")
	j := claim(t, q)
	if err := q.Checkpoint(ctx, j.ID, j.Attempts, "step 3 of 10"); err != nil {
		t.Fatal(err)
	}
	// the worker dies: the lease expires and the next claim sees the progress
	clk.advance(2 * time.Minute)
	j2 := claim(t, q)
	if j2.Progress != "step 3 of 10" {
		t.Errorf("progress after a lost worker = %q", j2.Progress)
	}
	// a failure with retries left keeps it too
	if _, err := q.Fail(ctx, j2.ID, j2.Attempts, errors.New("boom"), false); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Hour)
	j3 := claim(t, q)
	if j3.Progress != "step 3 of 10" {
		t.Errorf("progress after a retry = %q", j3.Progress)
	}
}

func TestCheckpointIsFenced(t *testing.T) {
	q, clk, _ := newQueue(t, nil)
	enqueue(t, q, "a")
	old := claim(t, q)
	clk.advance(2 * time.Minute)
	fresh := claim(t, q)
	q.Checkpoint(ctx, fresh.ID, fresh.Attempts, "new owner's progress")
	if err := q.Checkpoint(ctx, old.ID, old.Attempts, "stale"); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("stale checkpoint: %v", err)
	}
	if got := mustGet(t, q, old.ID); got.Progress != "new owner's progress" {
		t.Errorf("progress = %q", got.Progress)
	}
}

func TestCheckpointSizeLimitAndRetryClearsIt(t *testing.T) {
	q, _, _ := newQueue(t, nil)
	enqueue(t, q, "a")
	j := claim(t, q)
	if err := q.Checkpoint(ctx, j.ID, j.Attempts, strings.Repeat("x", maxPayload+1)); err == nil {
		t.Error("oversized checkpoint accepted")
	}
	q.Checkpoint(ctx, j.ID, j.Attempts, "half way")
	q.Fail(ctx, j.ID, j.Attempts, errors.New("bad"), true) // dead
	if err := q.Retry(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, q, j.ID); got.Progress != "" {
		t.Errorf("an operator's Retry starts fresh, but progress = %q", got.Progress)
	}
}

// ---- Release ------------------------------------------------------------------

func TestReleaseGivesTheAttemptBackAndKeepsTheQueuePosition(t *testing.T) {
	q, _, _ := newQueue(t, nil)
	first := enqueue(t, q, "first")
	enqueue(t, q, "second")
	j := claim(t, q)
	if j.ID != first.ID {
		t.Fatal("setup")
	}
	if err := q.Release(ctx, j.ID, j.Attempts); err != nil {
		t.Fatal(err)
	}
	got := mustGet(t, q, j.ID)
	if got.Status != Queued || got.Attempts != 0 || !got.LeaseUntil.IsZero() || got.LastError != "" {
		t.Errorf("released job = %+v", got)
	}
	if again := claim(t, q); again.ID != first.ID || again.Attempts != 1 {
		t.Errorf("the released job should be next in line, got %+v", again)
	}
}

func TestReleaseIsFencedAndOnlyForRunningJobs(t *testing.T) {
	q, clk, _ := newQueue(t, nil)
	enqueue(t, q, "a")
	old := claim(t, q)
	clk.advance(2 * time.Minute)
	claim(t, q)
	if err := q.Release(ctx, old.ID, old.Attempts); !errors.Is(err, ErrStaleClaim) {
		t.Errorf("stale release: %v", err)
	}
	if got := mustGet(t, q, old.ID); got.Status != Running || got.Attempts != 2 {
		t.Errorf("a stale release changed the job: %+v", got)
	}
	if err := q.Release(ctx, 999, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown job: %v", err)
	}
}

func TestReleaseDoesNotLetRestartsKillAJob(t *testing.T) {
	q, _, _ := newQueue(t, func(c *Config) { c.DefaultMaxAttempts = 2 })
	enqueue(t, q, "a")
	for i := 0; i < 20; i++ { // twenty stop/start cycles, far more than max_attempts
		j := claim(t, q)
		if err := q.Release(ctx, j.ID, j.Attempts); err != nil {
			t.Fatal(err)
		}
	}
	if got := mustGet(t, q, 1); got.Status != Queued || got.Attempts != 0 {
		t.Errorf("after 20 releases: %+v", got)
	}
}

// ---- migration from Day 9 ---------------------------------------------------------

// A queue file made before Day 12 (schema version 1, with jobs in it) must
// open, keep every job and gain the new column and table.
func TestUpgradeFromVersion1KeepsJobs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sqlstore.OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(migrations[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO jobs (key,kind,payload,status,attempts,max_attempts,run_at,created_at,updated_at)
		VALUES ('old-1','deep_think','{"text":"hello"}','queued',0,5,1,1,1),
		       ('old-2','interpret','{}','dead',5,5,1,1,1)`); err != nil {
		t.Fatal(err)
	}

	q, err := New(ctx, db, Config{})
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	var v int
	db.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v != len(migrations) || v < 2 {
		t.Errorf("version %d", v)
	}
	j, err := q.Get(ctx, 1)
	if err != nil || j.Key != "old-1" || j.Progress != "" || string(j.Payload) != `{"text":"hello"}` {
		t.Errorf("old job after upgrade: %+v %v", j, err)
	}
	if st, _ := q.Stats(ctx); st.Queued != 1 || st.Dead != 1 {
		t.Errorf("stats %+v", st)
	}
	// and the new features work on the upgraded file
	got, ok, _ := q.Claim(ctx, time.Minute)
	if !ok || got.Key != "old-1" {
		t.Fatalf("claim: %+v %v", got, ok)
	}
	if err := q.CompleteWith(ctx, got.ID, got.Attempts, "fine"); err != nil {
		t.Errorf("CompleteWith on an upgraded file: %v", err)
	}
}

func TestResultsTableRefusesOrphans(t *testing.T) {
	// foreign_keys=on in the DSN: a result for a job that does not exist is rejected.
	q, _, _ := newQueue(t, nil)
	_, err := q.db.Exec(`INSERT INTO results (job_id, attempt, text, created_at) VALUES (42, 1, 'x', 0)`)
	if err == nil {
		t.Error("database accepted a result for a missing job")
	}
	var _ *sql.DB = q.db
}
