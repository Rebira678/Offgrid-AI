package drain

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"offgrid/internal/jobqueue"
	"offgrid/internal/store/sqlstore"
)

// This file is Day 12's "done when": kill -9 in the middle of a drain,
// restart, and find no lost jobs and no duplicates.
//
// The "worker" is this very test binary, started again as a child process with
// an environment variable that makes TestHelperWorker run a real Worker on a
// real database file. The parent kills the child with SIGKILL, which the
// process cannot catch or clean up after: exactly what a power cut or the
// out-of-memory killer does. Then it starts a new child and finally checks the
// database.

const stepsPerJob = 3

// TestHelperWorker is not a test. It is the child process of the kill test.
func TestHelperWorker(t *testing.T) {
	if os.Getenv("OFFGRID_DRAIN_HELPER") != "1" {
		t.Skip("helper process for the kill -9 test; not run directly")
	}
	db, err := sqlstore.OpenDB(os.Getenv("OFFGRID_TEST_DB"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper:", err)
		os.Exit(3)
	}
	q, err := jobqueue.New(context.Background(), db, jobqueue.Config{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper:", err)
		os.Exit(3)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	work := func(ctx context.Context, j jobqueue.Job, p Progress) (string, error) {
		// Resume where the last attempt got to.
		start := 0
		if after, ok := strings.CutPrefix(j.Progress, "step="); ok {
			start, _ = strconv.Atoi(after)
		}
		for i := start; i < stepsPerJob; i++ {
			// The "work". step_runs counts every execution; step_effects has a
			// primary key, so repeating a step cannot create a second effect.
			if _, err := db.Exec(`INSERT INTO step_runs (job_id, step) VALUES (?, ?)`, j.ID, i); err != nil {
				return "", err
			}
			if _, err := db.Exec(`INSERT OR IGNORE INTO step_effects (job_id, step) VALUES (?, ?)`, j.ID, i); err != nil {
				return "", err
			}
			select {
			case <-time.After(8 * time.Millisecond):
			case <-ctx.Done():
				return "", ctx.Err()
			}
			if err := p.Save(ctx, fmt.Sprintf("step=%d", i+1)); err != nil {
				return "", err
			}
		}
		return fmt.Sprintf("done:%d", j.ID), nil
	}
	w, err := New(Config{
		Queue: q, Specs: map[string]Spec{"work": {Handle: work}},
		Concurrency: 4, Lease: 400 * time.Millisecond, Poll: 10 * time.Millisecond,
		ExitWhenIdle: os.Getenv("OFFGRID_TEST_EXIT_IDLE") == "1",
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper:", err)
		os.Exit(3)
	}
	w.Run(ctx)
	s := w.Summary()
	fmt.Printf("helper finished: done=%d released=%d lost=%d\n", s.Done, s.Released, s.LostClaims)
	os.Exit(0)
}

type child struct {
	cmd *exec.Cmd
	out *bytes.Buffer
}

func startChild(t *testing.T, dbPath string, exitIdle bool) *child {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperWorker$", "-test.timeout=2m")
	cmd.Env = append(os.Environ(), "OFFGRID_DRAIN_HELPER=1", "OFFGRID_TEST_DB="+dbPath)
	if exitIdle {
		cmd.Env = append(cmd.Env, "OFFGRID_TEST_EXIT_IDLE=1")
	}
	out := &bytes.Buffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &child{cmd: cmd, out: out}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return c
}

func count(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func TestKill9MidDrainLosesNothingAndDuplicatesNothing(t *testing.T) {
	for _, seed := range []int64{1, 2, 3} {
		seed := seed
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			const jobs = 60
			const kills = 3
			rnd := rand.New(rand.NewSource(seed))
			path := t.TempDir() + "/queue.db"

			db, err := sqlstore.OpenDB(path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			q, err := jobqueue.New(bg, db, jobqueue.Config{})
			if err != nil {
				t.Fatal(err)
			}
			for _, ddl := range []string{
				`CREATE TABLE step_runs (id INTEGER PRIMARY KEY AUTOINCREMENT, job_id INTEGER NOT NULL, step INTEGER NOT NULL)`,
				`CREATE TABLE step_effects (job_id INTEGER NOT NULL, step INTEGER NOT NULL, PRIMARY KEY (job_id, step))`,
			} {
				if _, err := db.Exec(ddl); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < jobs; i++ {
				if _, _, err := q.Enqueue(bg, fmt.Sprint("job-", i), "work", []byte(`{}`), jobqueue.EnqueueOptions{MaxAttempts: 10}); err != nil {
					t.Fatal(err)
				}
			}

			killedWhileRunning := 0
			for k := 1; k <= kills; k++ {
				c := startChild(t, path, false)
				target := k * jobs / (kills + 1) // 15, 30, 45 done: kill at different stages
				deadline := time.Now().Add(30 * time.Second)
				for {
					done := count(t, db, `SELECT COUNT(*) FROM jobs WHERE status='done'`)
					running := count(t, db, `SELECT COUNT(*) FROM jobs WHERE status='running'`)
					if done >= target && running >= 2 {
						killedWhileRunning += running
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("round %d: never reached %d done with jobs running (done=%d running=%d)\n%s", k, target, done, running, c.out.String())
					}
					time.Sleep(time.Millisecond)
				}
				time.Sleep(time.Duration(rnd.Intn(20)) * time.Millisecond) // a different moment each time
				if err := c.cmd.Process.Kill(); err != nil {               // SIGKILL
					t.Fatal(err)
				}
				c.cmd.Wait()
			}

			// Restart one last time and let it finish everything, then exit.
			final := startChild(t, path, true)
			waited := make(chan error, 1)
			go func() { waited <- final.cmd.Wait() }()
			select {
			case err := <-waited:
				if err != nil {
					t.Fatalf("the final worker failed: %v\n%s", err, final.out.String())
				}
			case <-time.After(60 * time.Second):
				t.Fatalf("the final worker never finished\n%s", final.out.String())
			}

			// ---- the verdict ----
			if n := count(t, db, `SELECT COUNT(*) FROM jobs`); n != jobs {
				t.Errorf("%d jobs exist, want %d", n, jobs)
			}
			for _, st := range []string{"queued", "running", "dead"} {
				if n := count(t, db, `SELECT COUNT(*) FROM jobs WHERE status=?`, st); n != 0 {
					t.Errorf("%d job(s) ended %s", n, st)
				}
			}
			if n := count(t, db, `SELECT COUNT(*) FROM jobs WHERE status='done'`); n != jobs {
				t.Errorf("%d jobs done, want %d: jobs were lost", n, jobs)
			}
			if n, d := count(t, db, `SELECT COUNT(*) FROM results`), count(t, db, `SELECT COUNT(DISTINCT job_id) FROM results`); n != jobs || d != jobs {
				t.Errorf("results rows=%d distinct jobs=%d, want %d each: a job has no result or two", n, d, jobs)
			}
			rows, err := db.Query(`SELECT job_id, text FROM results`)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var id int64
				var text string
				rows.Scan(&id, &text)
				if text != fmt.Sprint("done:", id) {
					t.Errorf("job %d has result %q", id, text)
				}
			}
			rows.Close()

			// Work: every step of every job has exactly one effect...
			if n := count(t, db, `SELECT COUNT(*) FROM step_effects`); n != jobs*stepsPerJob {
				t.Errorf("%d step effects, want exactly %d", n, jobs*stepsPerJob)
			}
			// ...but the killed attempts did repeat a little work, and only a little:
			// the checkpoint means a restarted job redoes at most the step it was in.
			runs := count(t, db, `SELECT COUNT(*) FROM step_runs`)
			extra := runs - jobs*stepsPerJob
			retried := count(t, db, `SELECT COUNT(*) FROM jobs WHERE attempts > 1`)
			totalRetries := count(t, db, `SELECT COALESCE(SUM(attempts - 1), 0) FROM jobs WHERE attempts > 1`)
			if retried == 0 {
				t.Errorf("no job was interrupted: the kill landed on nothing, so this run proves nothing")
			}
			// Each interrupted attempt can redo at most one step (the step it was in the middle of).
			// Without checkpointing, restarting from step 0 would redo all previous steps.
			if extra < 0 || extra > totalRetries {
				t.Errorf("steps executed %d times for %d needed (extra %d): resuming from checkpoint should redo at most one step per interrupted attempt (at most %d)", runs, jobs*stepsPerJob, extra, totalRetries)
			}
			if maxAllowedRetries := kills * 6; totalRetries > maxAllowedRetries {
				t.Errorf("total retried attempts %d exceeded reasonable bound %d for %d kills", totalRetries, maxAllowedRetries, kills)
			}
			t.Logf("seed %d: %d kills, %d running rows seen at the moments of the kills; %d jobs needed a second attempt (%d total retries); %d extra step executions (repeated work, all harmless); %d results, %d effects",
				seed, kills, killedWhileRunning, retried, totalRetries, extra, jobs, jobs*stepsPerJob)

			if res := func() string {
				var s string
				db.QueryRow(`PRAGMA integrity_check`).Scan(&s)
				return s
			}(); res != "ok" {
				t.Errorf("integrity_check: %s", res)
			}
		})
	}
}
