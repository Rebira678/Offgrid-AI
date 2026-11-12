package syncer

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"offgrid/internal/identity"
	"offgrid/internal/soak"
	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
)

// TestSoak keeps two signed replicas writing and syncing for as long as you
// ask, and judges memory and goroutines:
//
//	OFFGRID_SOAK=10m go test -run TestSoak -timeout 0 ./internal/syncer
//	OFFGRID_SOAK=1h  OFFGRID_SOAK_OUT=soak.jsonl go test -run TestSoak -timeout 0 ./internal/syncer
//
// It is skipped unless OFFGRID_SOAK is set, because it is meant to take a long
// time. It runs both replicas in one process over a real TCP socket on
// localhost, so it checks leaks and growth in the code; it is NOT the two-device
// soak (scripts/real-device/soak.sh), which is what Checkpoint 1 asks for.
func TestSoak(t *testing.T) {
	spec := os.Getenv("OFFGRID_SOAK")
	if spec == "" {
		t.Skip("set OFFGRID_SOAK=10m (a Go duration) to run the soak")
	}
	dur, err := time.ParseDuration(spec)
	if err != nil || dur < 5*time.Second {
		t.Fatalf("OFFGRID_SOAK=%q: want a duration of at least 5s", spec)
	}
	out := os.Getenv("OFFGRID_SOAK_OUT")
	if out == "" {
		out = filepath.Join(t.TempDir(), "soak.jsonl")
	}
	every := 10 * time.Second
	if dur < 10*time.Minute {
		every = dur / 60
	}

	ctx := context.Background()
	open := func(node string) (*sqlstore.Store, *identity.Identity) {
		id, err := identity.Generate()
		if err != nil {
			t.Fatal(err)
		}
		s, err := sqlstore.Open(ctx, filepath.Join(t.TempDir(), node+".db"), sqlstore.Options{NodeID: node, Signer: id})
		if err != nil {
			t.Fatal(err)
		}
		return s, id
	}
	a, _ := open("laptop")
	b, _ := open("pi")
	defer a.Close()
	defer b.Close()

	srv := httptest.NewServer(Handler(b, ServerOptions{Key: key}))
	cl := &Client{Peer: srv.URL, Key: key, Node: "laptop"}

	f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	var rounds, failures atomic.Int64
	runCtx, stop := context.WithTimeout(ctx, dur)
	defer stop()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		soak.Run(runCtx, f, "inproc", every, func(s *soak.Sample) {
			s.Rounds, s.Failures = rounds.Load(), failures.Load()
			if sum, err := a.Summary(ctx); err == nil {
				for _, n := range sum {
					s.Ops += n
				}
			}
		})
	}()
	writer := func(s *sqlstore.Store, name string, gap time.Duration) {
		defer wg.Done()
		tk := time.NewTicker(gap)
		defer tk.Stop()
		for i := 0; ; i++ {
			select {
			case <-runCtx.Done():
				return
			case <-tk.C:
			}
			created, _, err := s.Create(ctx, task.NewTask{Title: fmt.Sprintf("%s %d", name, i), Priority: task.PriorityMedium})
			if err != nil {
				t.Errorf("%s write: %v", name, err)
				return
			}
			if i%3 == 0 {
				if _, err := s.AddNote(ctx, created.ID, "note"); err != nil {
					t.Errorf("%s note: %v", name, err)
					return
				}
			}
		}
	}
	wg.Add(2)
	go writer(a, "laptop", 40*time.Millisecond)
	go writer(b, "pi", 55*time.Millisecond)
	wg.Add(1)
	go func() {
		defer wg.Done()
		tk := time.NewTicker(100 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-tk.C:
			}
			rctx, cancel := context.WithTimeout(runCtx, 10*time.Second)
			_, err := cl.Once(rctx, a)
			cancel()
			if runCtx.Err() != nil {
				return
			}
			rounds.Add(1)
			if err != nil {
				failures.Add(1)
				t.Logf("round failed: %v", err)
			}
		}
	}()
	wg.Wait()
	f.Close()

	// Everything quiet: one last round, then both sides must agree and every op must verify.
	if _, err := cl.Once(ctx, a); err != nil {
		t.Fatal(err)
	}
	if ha, hb := hash(t, a), hash(t, b); ha != hb {
		t.Fatalf("replicas differ after the soak:\n A %s\n B %s", ha, hb)
	}
	for name, s := range map[string]*sqlstore.Store{"laptop": a, "pi": b} {
		rep, err := s.VerifySignatures(ctx)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if rep.Unsigned != 0 || rep.Signed != rep.Ops || len(rep.Devices) != 2 {
			t.Fatalf("%s: %+v, want every op signed by 2 devices", name, rep)
		}
		if _, err := s.Verify(ctx); err != nil {
			t.Fatalf("%s: store verify: %v", name, err)
		}
	}
	srv.Close()

	in, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	lim := soak.DefaultLimits
	lim.MinDuration = dur / 2
	rep, err := soak.Summarize(in, lim)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("soak %s, samples in %s\n%s", spec, out, rep.Format())
	if rep.Verdict != "PASS" {
		t.Fatalf("soak verdict %s: %v", rep.Verdict, rep.Problems)
	}
}
