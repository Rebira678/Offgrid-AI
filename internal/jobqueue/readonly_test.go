package jobqueue

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"offgrid/internal/store/sqlstore"
)

func TestReadOnlyViewSeesTheQueueAndCannotChangeIt(t *testing.T) {
	q, _, path := newQueue(t, nil)
	enqueue(t, q, "a")
	enqueue(t, q, "b")
	if _, _, err := q.Claim(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}

	db, err := sqlstore.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ro, err := NewReadOnly(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ro.Stats(ctx)
	if err != nil || s.Queued != 1 || s.Running != 1 {
		t.Fatalf("a read-only view must see the live queue: %+v %v", s, err)
	}
	// it follows later changes made by the owner
	enqueue(t, q, "c")
	if s, _ := ro.Stats(ctx); s.Queued != 2 {
		t.Errorf("did not see a new job: %+v", s)
	}
	// and every write is refused by SQLite itself, not just by our good manners
	if _, err := db.Exec(`DELETE FROM jobs`); err == nil {
		t.Error("the read-only connection could delete jobs")
	}
	if _, _, err := ro.Enqueue(ctx, "x", "k", []byte(`{}`), EnqueueOptions{}); err == nil {
		t.Error("the read-only queue accepted a job")
	}
	if s, _ := q.Stats(ctx); s.Total() != 3 {
		t.Errorf("the owner's data changed: %+v", s)
	}
}

func TestReadOnlyViewDoesNotMigrateAnOldFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sqlstore.OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	// a Day 9 (version 1) queue: only the first migration applied
	if _, err := raw.Exec(migrations[0]); err != nil {
		t.Fatal(err)
	}
	raw.Exec(`PRAGMA user_version = 1`)
	raw.Exec(`INSERT INTO jobs(key,kind,payload,status,max_attempts,run_at,created_at,updated_at) VALUES('k','x','{}','queued',5,0,0,0)`)
	raw.Close()

	db, err := sqlstore.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ro, err := NewReadOnly(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if s, err := ro.Stats(ctx); err != nil || s.Queued != 1 {
		t.Errorf("an old file can still be shown: %+v %v", s, err)
	}
	var v int
	db.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v != 1 {
		t.Errorf("a viewer must not upgrade someone else's file: version %d", v)
	}
}

func TestReadOnlyViewRefusesWhatIsNotAQueue(t *testing.T) {
	// a database with nothing in it
	path := filepath.Join(t.TempDir(), "empty.db")
	raw, _ := sqlstore.OpenDB(path)
	raw.Close()
	db, err := sqlstore.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := NewReadOnly(ctx, db); err == nil || !strings.Contains(err.Error(), "no queue yet") {
		t.Errorf("%v", err)
	}
	// one from a newer program
	q, _, p2 := newQueue(t, nil)
	_ = q
	raw2, _ := sql.Open("sqlite3", "file:"+p2)
	raw2.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, len(migrations)+1))
	raw2.Close()
	db2, err := sqlstore.OpenReadOnly(p2)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if _, err := NewReadOnly(ctx, db2); !errors.Is(err, ErrNewerSchema) {
		t.Errorf("%v", err)
	}
}

func TestOpenReadOnlyNeverCreatesAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	if _, err := sqlstore.OpenReadOnly(path); err == nil {
		t.Fatal("a missing file must be an error")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("OpenReadOnly created the file")
	}
	if _, err := sqlstore.OpenReadOnly(""); err == nil {
		t.Error("empty path")
	}
}

func TestReadOnlyConcurrentWithWriters(t *testing.T) {
	q, _, path := newQueue(t, nil)
	db, err := sqlstore.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ro, err := NewReadOnly(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Reader continuously polls Stats
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = ro.Stats(ctx)
				time.Sleep(time.Millisecond)
			}
		}
	}()

	// Writers concurrently enqueue and claim
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				key := fmt.Sprintf("concurrent-%d-%d", workerID, j)
				_, _, _ = q.Enqueue(ctx, key, "compute", []byte(`{}`), EnqueueOptions{})
				_, _, _ = q.Claim(ctx, time.Second)
			}
		}(i)
	}

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()

	finalStats, err := ro.Stats(ctx)
	if err != nil {
		t.Fatalf("final Stats failed: %v", err)
	}
	if finalStats.Total() == 0 {
		t.Errorf("expected jobs to have been enqueued, got total=0")
	}
}
