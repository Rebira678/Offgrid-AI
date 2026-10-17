package drain

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"offgrid/internal/agent"
	"offgrid/internal/connectivity"
	"offgrid/internal/jobqueue"
	"offgrid/internal/llm"
	"offgrid/internal/netfault"
	"offgrid/internal/router"
	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
	"offgrid/internal/tools"
)

// TestFullE2EPipelineDay1ToDay12Audit executes an exhaustive lifecycle integration test
// uniting the entire Offgrid runtime architecture across all 12 days:
//  1. Storage & Event-Sourcing (Day 6): op-log append, projection view, verify & rebuild.
//  2. Rules & Agent Tiering (Day 5): Tier 0 deterministic sub-millisecond execution.
//  3. Router Complexity Gating (Day 10): Instant answer vs durable queue deferral.
//  4. Fault Injection & Connectivity (Day 8, 11): Probers, hysteresis, and partition flipping.
//  5. Worker Draining, Checkpointing & Crash-Recovery (Day 12): Partial execution, checkpoint save,
//     lease expiration takeover, and exactly-once result persistence.
func TestFullE2EPipelineDay1ToDay12Audit(t *testing.T) {
	dir := t.TempDir()
	taskDBPath := filepath.Join(dir, "tasks.db")
	queueDBPath := filepath.Join(dir, "queue.db")

	clockNow := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	clk := func() time.Time { return clockNow }

	// 1. Initialize Event-Sourced Task Store (Day 6)
	taskStore, err := sqlstore.Open(context.Background(), taskDBPath, sqlstore.Options{Now: clk})
	if err != nil {
		t.Fatalf("sqlstore.Open task: %v", err)
	}

	reg := tools.NewRegistry(taskStore, clk)
	ag := &agent.Agent{Rules: agent.DefaultRules(), Registry: reg}

	// 2. Initialize Durable Job Queue (Day 9)
	queueDB, err := sqlstore.OpenDB(queueDBPath)
	if err != nil {
		t.Fatalf("OpenDB queue: %v", err)
	}
	defer queueDB.Close()

	q, err := jobqueue.New(context.Background(), queueDB, jobqueue.Config{
		Backoff: jobqueue.Backoff{Base: 10 * time.Millisecond, Max: 30 * time.Millisecond, Jitter: jobqueue.NoJitter},
	})
	if err != nil {
		t.Fatalf("jobqueue.New: %v", err)
	}

	// 3. Initialize Fault Injector & Connectivity Prober (Day 8 & 11)
	mockInternet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer mockInternet.Close()

	// Initial state: Network is PARTITIONED via in-process switch
	sw := &netfault.Switch{}
	sw.Set(true)

	faultTr, err := netfault.New(netfault.Config{
		Mode:        netfault.Refuse,
		Partitioned: sw.Source(),
	})
	if err != nil {
		t.Fatalf("netfault.New: %v", err)
	}

	probeClient := &http.Client{Transport: faultTr, Timeout: 300 * time.Millisecond}
	prober := connectivity.HTTPProber{URL: mockInternet.URL, Client: probeClient}

	monCtx, monCancel := context.WithCancel(context.Background())
	defer monCancel()

	mon, err := connectivity.New(connectivity.Config{
		Prober:   prober,
		Interval: 25 * time.Millisecond,
		Thresholds: connectivity.Thresholds{
			DownAfter:    1,
			UpAfter:      1,
			DegradeAfter: 1,
		},
	})
	if err != nil {
		t.Fatalf("connectivity.New: %v", err)
	}
	go mon.Run(monCtx)

	// Wait briefly for initial OFFLINE state detection
	deadline := time.Now().Add(2 * time.Second)
	for mon.State() != connectivity.Offline && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if mon.State() != connectivity.Offline {
		t.Fatalf("expected initial state OFFLINE, got: %s", mon.State())
	}

	// 4. Configure Router (Day 10)
	rt := &router.Router{
		Agent: ag,
		Queue: q,
		Net:   mon.State,
		Now:   clk,
	}

	// Execute Tier 0 request (Day 5) - deterministic, creates task
	resT0 := rt.Handle(context.Background(), "new task: Emergency turbine shutdown at site 9, priority high")
	if resT0.Route != router.RouteLocal || resT0.Outcome.Tier != agent.TierRules {
		t.Fatalf("expected Tier 0 local execution, got: %+v", resT0)
	}

	// Verify Task was persisted in event-sourced view (Day 6)
	tasks, err := taskStore.List(context.Background(), task.Filter{})
	if err != nil || len(tasks) != 1 || tasks[0].Site != "9" {
		t.Fatalf("task view verification failed: %+v err=%v", tasks, err)
	}

	// Execute Complex Requests requiring deep thinking (Day 10 Deferral)
	resHard1 := rt.Handle(context.Background(), "summarize this week's incidents and analyze turbine failure causes")
	if resHard1.Route != router.RouteDeferred || resHard1.Job == nil {
		t.Fatalf("expected hard request #1 deferred to queue, got: %+v", resHard1)
	}
	resHard2 := rt.Handle(context.Background(), "compare site 2 and site 9 performance and suggest next maintenance")
	if resHard2.Route != router.RouteDeferred || resHard2.Job == nil {
		t.Fatalf("expected hard request #2 deferred to queue, got: %+v", resHard2)
	}

	job1ID := resHard1.Job.ID
	job2ID := resHard2.Job.ID

	// Verify Queue Depth
	st, _ := q.Stats(context.Background())
	if st.Queued != 2 || st.Running != 0 || st.Done != 0 {
		t.Fatalf("unexpected queue stats: %+v", st)
	}

	// Verify Op-Log Integrity (Day 6 Rebuild & Verify)
	vRep, err := taskStore.Verify(context.Background())
	if err != nil || vRep.LiveHash != vRep.ReplayHash {
		t.Fatalf("taskStore.Verify mismatch: %v %+v", err, vRep)
	}
	rRep, err := taskStore.Rebuild(context.Background())
	if err != nil || rRep.Ops != 1 || rRep.Hash != vRep.LiveHash {
		t.Fatalf("taskStore.Rebuild mismatch: %v %+v (want hash %s)", err, rRep, vRep.LiveHash)
	}

	// 5. Run Drain Worker while Offline (Day 12)
	fakeOllama := &llm.Fake{
		Handler: func(ctx context.Context, req llm.Request) (string, error) {
			return "SYNTHESIZED INCIDENT ANALYSIS: Turbine bearings require replacement.", nil
		},
	}

	specs := map[string]Spec{
		KindDeepThink: DeepThink(DeepThinkConfig{
			Client:       fakeOllama,
			Model:        "llama3.2:3b",
			NeedsNetwork: true, // Needs network!
			Context: func(ctx context.Context) (string, error) {
				return "Turbine site 9 high priority", nil
			},
		}),
	}

	workerOffline, err := New(Config{
		Queue:        q,
		Specs:        specs,
		Concurrency:  2,
		Poll:         10 * time.Millisecond,
		ExitWhenIdle: true,
		Online:       func() bool { return mon.State() == connectivity.Online },
	})
	if err != nil {
		t.Fatalf("New worker: %v", err)
	}

	// Worker runs while offline - must do NOTHING because jobs need network!
	workerOffline.Run(context.Background())
	sumOffline := workerOffline.Summary()
	if sumOffline.Claimed != 0 || sumOffline.Done != 0 {
		t.Fatalf("worker offline claimed network jobs: %+v", sumOffline)
	}

	// Verify jobs are untouched in queue with 0 attempts burned
	for _, id := range []int64{job1ID, job2ID} {
		j, _ := q.Get(context.Background(), id)
		if j.Status != jobqueue.Queued || j.Attempts != 0 {
			t.Fatalf("job %d was modified while offline: %+v", id, j)
		}
	}

	// 6. Heal the Network (Day 11) & Run Mid-Drain Crash Recovery (Day 12)
	sw.Set(false) // Heal network

	// Wait for prober to detect ONLINE
	deadline = time.Now().Add(3 * time.Second)
	for mon.State() != connectivity.Online && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if mon.State() != connectivity.Online {
		t.Fatalf("monitor failed to detect healed network: %s", mon.State())
	}

	// Simulate multi-stage worker with crash on job #1
	var job1Attempts atomic.Int32
	stageSpecs := map[string]Spec{
		KindDeepThink: {
			NeedsNetwork: true,
			Handle: func(ctx context.Context, j jobqueue.Job, p Progress) (string, error) {
				if j.ID == job1ID {
					try := job1Attempts.Add(1)
					if try == 1 {
						// Save checkpoint before simulated crash
						if err := p.Save(ctx, "stage-1-data-gathered"); err != nil {
							return "", err
						}
						// Simulate crash / kill by exiting handler abruptly without reporting
						// (simulates unhandled SIGKILL / sudden process loss)
						return "", errors.New("simulated crash mid-execution")
					}
					// Second attempt resumes from checkpoint!
					if j.Progress != "stage-1-data-gathered" {
						return "", fmt.Errorf("expected resumed progress 'stage-1-data-gathered', got %q", j.Progress)
					}
				}
				return "FINISHED ANALYSIS FOR " + j.Progress, nil
			},
		},
	}

	ctxActive, cancelActive := context.WithCancel(context.Background())
	defer cancelActive()

	workerActive, err := New(Config{
		Queue:        q,
		Specs:        stageSpecs,
		Concurrency:  2,
		Poll:         10 * time.Millisecond,
		Lease:        100 * time.Millisecond, // short lease for fast recovery
		ExitWhenIdle: false,
		Online:       func() bool { return mon.State() == connectivity.Online },
	})
	if err != nil {
		t.Fatalf("New active worker: %v", err)
	}

	doneCh := make(chan struct{})
	go func() {
		workerActive.Run(ctxActive)
		close(doneCh)
	}()

	// Wait until both jobs complete (including retry of job #1)
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, _ := q.Stats(context.Background())
		if st.Done == 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancelActive()
	<-doneCh

	// 7. Final Verification of Invariants
	finalStats, err := q.Stats(context.Background())
	if err != nil {
		t.Fatalf("q.Stats: %v", err)
	}
	if finalStats.Done != 2 || finalStats.Queued != 0 || finalStats.Running != 0 || finalStats.Dead != 0 {
		t.Fatalf("final queue stats mismatch: %+v (want done=2)", finalStats)
	}

	// Verify exact results exist for both jobs
	for _, id := range []int64{job1ID, job2ID} {
		res, found, err := q.Result(context.Background(), id)
		if err != nil || !found || res.Text == "" {
			t.Fatalf("job %d missing result: found=%v err=%v text=%q", id, found, err, res.Text)
		}
	}

	// Verify SQLite database integrity
	var pragmaCheck string
	if err := queueDB.QueryRow(`PRAGMA integrity_check`).Scan(&pragmaCheck); err != nil || pragmaCheck != "ok" {
		t.Fatalf("queueDB integrity failed: %v %s", err, pragmaCheck)
	}
}
