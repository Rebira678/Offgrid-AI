package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"text/tabwriter"
	"time"

	"offgrid/internal/jobqueue"
	"offgrid/internal/store/sqlstore"
)

const queueHelp = `usage: offgrid queue [-q FILE] SUBCOMMAND

  enqueue -key K -kind KIND [-max N] [PAYLOAD-JSON]   store a job (a repeat of the same key adds nothing)
  list    [-status queued|running|done|dead] [-n N]   show jobs
  stats                                               counts by status
  claim   [-lease 1m]                                 take the next ready job (prints it as JSON)
  done    ID ATTEMPT                                  report success
  fail    [-error MSG] [-permanent] ID ATTEMPT        report failure: retry later, or dead-letter
  retry   ID                                          put a dead job back in the queue
  result  ID                                          print the answer a finished job produced
  jitter  [-clients N] [-attempt A] [-seed S]         print retry delays as CSV (no queue file needed)

The queue file is -q FILE, or $OFFGRID_QUEUE, or queue.db next to $OFFGRID_DB.`

// queuePath finds the queue file: -q, then $OFFGRID_QUEUE, then queue.db in
// the folder of $OFFGRID_DB.
func queuePath(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if p := os.Getenv("OFFGRID_QUEUE"); p != "" {
		return p
	}
	if db := os.Getenv("OFFGRID_DB"); db != "" {
		return filepath.Join(filepath.Dir(db), "queue.db")
	}
	return ""
}

// cmdQueue is the command-line door to the durable job queue. It is how you
// can see, by hand, every state a job goes through.
func cmdQueue(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("queue", flag.ContinueOnError)
	fs.SetOutput(stderr)
	qFlag := fs.String("q", "", "the queue file (default: $OFFGRID_QUEUE, or queue.db next to $OFFGRID_DB)")
	fs.Usage = func() { fmt.Fprintln(stderr, queueHelp) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	sub := fs.Arg(0)
	if sub == "" {
		fs.Usage()
		return 2
	}
	rest := fs.Args()[1:]
	if sub == "jitter" {
		return queueJitter(rest, stdout, stderr)
	}
	switch sub {
	case "enqueue", "list", "stats", "claim", "done", "fail", "retry", "result":
	default:
		fmt.Fprintf(stderr, "error: unknown queue command %q\n\n%s\n", sub, queueHelp)
		return 2
	}

	path := queuePath(*qFlag)
	if path == "" {
		fmt.Fprintln(stderr, "error: no queue file. Use -q FILE (or set OFFGRID_QUEUE)")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if sub != "enqueue" {
		if _, err := os.Stat(path); err != nil {
			fmt.Fprintf(stderr, "error: no queue at %s (create one with: offgrid queue -q %s enqueue ...)\n", path, path)
			return 1
		}
	} else if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	db, err := sqlstore.OpenDB(path)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	defer db.Close()
	q, err := jobqueue.New(ctx, db, jobqueue.Config{})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	code, err := queueRun(ctx, q, sub, rest, stdout, stderr)
	if err != nil {
		return queueError(stderr, err)
	}
	return code
}

func queueError(stderr io.Writer, err error) int {
	switch {
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(stderr, "cancelled")
		return 130
	case errors.Is(err, jobqueue.ErrStaleClaim):
		fmt.Fprintln(stderr, "error: that claim is no longer valid: its lease expired and the job was given to someone else, or it was already finished")
	case errors.Is(err, jobqueue.ErrKeyConflict):
		fmt.Fprintln(stderr, "error: that key already belongs to a different job (same key, different kind or payload). Use a new key")
	case errors.Is(err, jobqueue.ErrNotFound):
		fmt.Fprintln(stderr, "error: no such job")
	case errors.Is(err, jobqueue.ErrNotDead):
		fmt.Fprintln(stderr, "error: only a dead job can be retried")
	default:
		fmt.Fprintf(stderr, "error: %v\n", err)
	}
	return 1
}

func queueRun(ctx context.Context, q *jobqueue.Queue, sub string, args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("queue "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	key := fs.String("key", "", "enqueue: the idempotency key")
	kind := fs.String("kind", "", "enqueue: what kind of job")
	maxAtt := fs.Int("max", 0, "enqueue: attempts before dead-lettering (default 5)")
	status := fs.String("status", "", "list: only this status")
	n := fs.Int("n", 0, "list: at most this many jobs")
	lease := fs.Duration("lease", time.Minute, "claim: how long the claim lasts")
	cause := fs.String("error", "failed", "fail: why")
	permanent := fs.Bool("permanent", false, "fail: do not retry, dead-letter now")
	if err := fs.Parse(args); err != nil {
		return 2, nil
	}
	pos := fs.Args()
	usage := func(msg string) (int, error) {
		fmt.Fprintf(stderr, "error: %s\n", msg)
		return 2, nil
	}
	ids := func(want int) (int64, int, bool) {
		if len(pos) != want {
			return 0, 0, false
		}
		id, err1 := strconv.ParseInt(pos[0], 10, 64)
		att := 0
		var err2 error
		if want == 2 {
			att, err2 = strconv.Atoi(pos[1])
		}
		return id, att, err1 == nil && err2 == nil
	}

	switch sub {
	case "enqueue":
		if *key == "" || *kind == "" || len(pos) > 1 {
			return usage("usage: offgrid queue enqueue -key K -kind KIND [-max N] [PAYLOAD-JSON]")
		}
		var payload json.RawMessage
		if len(pos) == 1 {
			payload = json.RawMessage(pos[0])
		}
		j, created, err := q.Enqueue(ctx, *key, *kind, payload, jobqueue.EnqueueOptions{MaxAttempts: *maxAtt})
		if err != nil {
			return 0, err
		}
		if created {
			fmt.Fprintf(stdout, "queued job %d (key %s, kind %s)\n", j.ID, j.Key, j.Kind)
		} else {
			fmt.Fprintf(stdout, "already queued: job %d (key %s, %s). Nothing added\n", j.ID, j.Key, j.Status)
		}
	case "list":
		jobs, err := q.List(ctx, jobqueue.Status(*status), *n)
		if err != nil {
			return 0, err
		}
		if len(jobs) == 0 {
			fmt.Fprintln(stdout, "no jobs")
			return 0, nil
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tSTATUS\tTRIES\tKIND\tPAYLOAD\tRUN AT\tLAST ERROR")
		for _, j := range jobs {
			msg := j.LastError
			if len(msg) > 40 {
				msg = msg[:37] + "..."
			}
			payload := string(j.Payload)
			if len(payload) > 80 {
				payload = payload[:77] + "..."
			}
			fmt.Fprintf(tw, "%d\t%s\t%d/%d\t%s\t%s\t%s\t%s\n", j.ID, j.Status, j.Attempts, j.MaxAttempts, j.Kind, payload, j.RunAt.Local().Format("15:04:05"), msg)
		}
		tw.Flush()
	case "stats":
		s, err := q.Stats(ctx)
		if err != nil {
			return 0, err
		}
		fmt.Fprintf(stdout, "queued   %d\nrunning  %d\ndone     %d\ndead     %d\ntotal    %d\n", s.Queued, s.Running, s.Done, s.Dead, s.Total())
		if !s.NextReady.IsZero() {
			fmt.Fprintf(stdout, "next job ready at %s\n", s.NextReady.Local().Format("15:04:05"))
		}
	case "claim":
		j, ok, err := q.Claim(ctx, *lease)
		if err != nil {
			return 0, err
		}
		if !ok {
			fmt.Fprintln(stdout, "no job ready")
			return 0, nil
		}
		out, _ := json.Marshal(map[string]any{
			"id": j.ID, "key": j.Key, "kind": j.Kind, "payload": j.Payload,
			"attempt": j.Attempts, "max_attempts": j.MaxAttempts, "lease_until": j.LeaseUntil.Format(time.RFC3339),
		})
		fmt.Fprintln(stdout, string(out))
	case "done":
		id, att, ok := ids(2)
		if !ok {
			return usage("usage: offgrid queue done ID ATTEMPT   (both numbers come from the claim output)")
		}
		if err := q.Complete(ctx, id, att); err != nil {
			return 0, err
		}
		fmt.Fprintf(stdout, "job %d done\n", id)
	case "fail":
		id, att, ok := ids(2)
		if !ok {
			return usage("usage: offgrid queue fail [-error MSG] [-permanent] ID ATTEMPT")
		}
		res, err := q.Fail(ctx, id, att, errors.New(*cause), *permanent)
		if err != nil {
			return 0, err
		}
		if res.Status == jobqueue.Dead {
			fmt.Fprintf(stdout, "job %d is dead (dead-letter). See it with: offgrid queue list -status dead\n", id)
		} else {
			fmt.Fprintf(stdout, "job %d will retry in %s (at %s)\n", id, res.Delay.Round(time.Millisecond), res.RunAt.Local().Format("15:04:05"))
		}
	case "result":
		id, _, ok := ids(1)
		if !ok {
			return usage("usage: offgrid queue result ID")
		}
		r, found, err := q.Result(ctx, id)
		if err != nil {
			return 0, err
		}
		if !found {
			j, err := q.Get(ctx, id)
			if err != nil {
				return 0, err
			}
			fmt.Fprintf(stderr, "job %d has no result yet (it is %s", id, j.Status)
			if j.LastError != "" {
				fmt.Fprintf(stderr, "; last error: %s", j.LastError)
			}
			fmt.Fprintln(stderr, ")")
			return 1, nil
		}
		fmt.Fprintln(stdout, r.Text)
	case "retry":
		id, _, ok := ids(1)
		if !ok {
			return usage("usage: offgrid queue retry ID")
		}
		if err := q.Retry(ctx, id); err != nil {
			return 0, err
		}
		fmt.Fprintf(stdout, "job %d is queued again with a fresh set of attempts\n", id)
	}
	return 0, nil
}

// queueJitter prints, as CSV, the retry delay each of N clients would pick
// after the same failure, for each jitter mode. It uses the real Backoff
// code, so the chart in the Day 9 guide is computed by the code it describes.
// It is a simulation of the algorithm, not a measurement of any network.
func queueJitter(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("queue jitter", flag.ContinueOnError)
	fs.SetOutput(stderr)
	clients := fs.Int("clients", 1000, "how many clients failed at the same moment")
	attempt := fs.Int("attempt", 3, "which failed attempt (1 = first)")
	base := fs.Duration("base", 2*time.Second, "backoff base")
	max := fs.Duration("max", 5*time.Minute, "backoff cap")
	seed := fs.Int64("seed", 1, "random seed, so the output is reproducible")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *clients < 1 || *clients > 1_000_000 || *attempt < 1 {
		fmt.Fprintln(stderr, "error: -clients must be 1 to 1000000 and -attempt at least 1")
		return 2
	}
	fmt.Fprintln(stdout, "mode,client,delay_ms")
	for _, mode := range []jobqueue.Jitter{jobqueue.NoJitter, jobqueue.EqualJitter, jobqueue.FullJitter} {
		b := jobqueue.Backoff{Base: *base, Max: *max, Jitter: mode}
		if err := b.Validate(); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 2
		}
		rng := rand.New(rand.NewSource(*seed))
		for c := 0; c < *clients; c++ {
			fmt.Fprintf(stdout, "%s,%d,%d\n", mode, c, b.Delay(*attempt, rng.Float64()).Milliseconds())
		}
	}
	return 0
}
