package syncer

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"offgrid/internal/netfault"
	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
)

// lossy is a transport that loses requests AND responses. Losing a response is
// the cruel case: the server did the work, the client believes it did not, and
// will send the same ops again.
type lossy struct {
	base     http.RoundTripper
	rng      *rand.Rand
	reqLoss  float64
	respLoss float64

	lostReq, lostResp atomic.Int64
	limit             int64 // if > 0, only this many requests get through at all (a cable pulled mid-round)
	seen              atomic.Int64
}

var errLost = errors.New("lost in the network")

func (l *lossy) RoundTrip(r *http.Request) (*http.Response, error) {
	n := l.seen.Add(1)
	if l.limit > 0 && n > l.limit {
		l.lostReq.Add(1)
		return nil, errLost
	}
	if l.rng != nil && l.rng.Float64() < l.reqLoss {
		l.lostReq.Add(1)
		return nil, errLost
	}
	resp, err := l.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	if l.rng != nil && l.rng.Float64() < l.respLoss {
		resp.Body.Close()
		l.lostResp.Add(1)
		return nil, errLost
	}
	return resp, nil
}

func verify(t *testing.T, s *sqlstore.Store) {
	t.Helper()
	if _, err := s.Verify(context.Background()); err != nil {
		t.Fatalf("%s: Verify: %v", s.NodeID(), err)
	}
}

func inStep(t *testing.T, a, b *sqlstore.Store) bool {
	t.Helper()
	sa, _ := a.Summary(context.Background())
	sb, _ := b.Summary(context.Background())
	return fmt.Sprint(sa) == fmt.Sprint(sb) && hash(t, a) == hash(t, b)
}

func shown(t *testing.T, s *sqlstore.Store) map[string]task.Task {
	t.Helper()
	l, err := s.List(context.Background(), task.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]task.Task{}
	for _, tk := range l {
		m[tk.ID] = tk
	}
	return m
}

// TestCheckpoint2 is the Day 20 scenario through the real fault-injection
// package: both replicas edit while the network is off, then it comes back.
func TestCheckpoint2TwoReplicasEditOfflineAndConverge(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{t0}
	a, b := newStore(t, "node-a", clk), newStore(t, "node-b", clk)

	// A has work; B is empty. The link between them goes through the chaos switch.
	for _, title := range []string{"Leaking pump", "Replace filter", "Check valve"} {
		add(t, a, title)
	}
	if _, err := a.AddNote(ctx, "T1", "seal cracked"); err != nil {
		t.Fatal(err)
	}
	var sw netfault.Switch
	tr, err := netfault.New(netfault.Config{Partitioned: sw.Source()})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(Handler(a, ServerOptions{Key: key, Now: clk.now}))
	defer srv.Close()
	cl := &Client{Peer: srv.URL, Key: key, Now: clk.now, HTTP: &http.Client{Transport: tr, Timeout: 5 * time.Second}}
	if _, err := cl.Once(ctx, b); err != nil {
		t.Fatal(err)
	}
	if !inStep(t, a, b) {
		t.Fatal("not in step after the first sync")
	}

	// WiFi off. A sync attempt fails the way a real one would, and changes nothing.
	sw.Set(true)
	before := hash(t, b)
	if _, err := cl.Once(ctx, b); !errors.Is(err, netfault.ErrPartitioned) {
		t.Fatalf("err = %v, want an injected partition", err)
	}
	if hash(t, b) != before {
		t.Fatal("a failed round changed B")
	}

	// A edits.
	clk.t = clk.t.Add(time.Minute)
	setStatus := func(s *sqlstore.Store, id string, st task.Status) {
		t.Helper()
		if _, _, err := s.Update(ctx, id, task.Changes{Status: &st}); err != nil {
			t.Fatal(err)
		}
	}
	setPrio := func(s *sqlstore.Store, id string, p task.Priority) {
		t.Helper()
		if _, _, err := s.Update(ctx, id, task.Changes{Priority: &p}); err != nil {
			t.Fatal(err)
		}
	}
	setStatus(a, "T1", task.StatusDone)
	a.AddNote(ctx, "T1", "replaced the seal")
	if err := a.DeleteTask(ctx, "T2"); err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteTask(ctx, "T3"); err != nil {
		t.Fatal(err)
	}
	fuel := add(t, a, "Fuel line")

	// B edits the same task, later in time, and more.
	clk.t = clk.t.Add(time.Minute)
	setStatus(b, "T1", task.StatusInProgress)
	setPrio(b, "T1", task.PriorityLow)
	b.AddNote(ctx, "T1", "waiting for parts")
	if _, err := b.RemoveNote(ctx, "T1", 1); err != nil { // "seal cracked"
		t.Fatal(err)
	}
	setPrio(b, "T2", task.PriorityHigh) // A deleted T2 and has not seen this
	gasket := add(t, b, "Order gasket")

	if inStep(t, a, b) || hash(t, a) == hash(t, b) {
		t.Fatal("the replicas should differ while apart")
	}
	if fuel.ID == gasket.ID {
		t.Fatalf("both new tasks got the ID %s", fuel.ID)
	}

	// WiFi on: one round and they agree.
	sw.Set(false)
	rep, err := cl.Once(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Pulled == 0 || rep.Pushed == 0 {
		t.Fatalf("report %+v: both directions should have carried ops", rep)
	}
	if hash(t, a) != hash(t, b) || !inStep(t, a, b) {
		t.Fatal("state hashes differ after reconnecting: Checkpoint 2 fails")
	}
	verify(t, a)
	verify(t, b)

	got := shown(t, b)
	t1 := got["T1"]
	if t1.Status != task.StatusInProgress || t1.Priority != task.PriorityLow {
		t.Errorf("T1 = %s/%s, want in_progress/low (B's later edits, each field on its own)", t1.Status, t1.Priority)
	}
	var texts []string
	for _, n := range t1.Notes {
		texts = append(texts, n.Text)
	}
	if strings.Join(texts, "|") != "replaced the seal|waiting for parts" {
		t.Errorf("T1 notes = %q, want both new notes and not the removed one", texts)
	}
	if t2, ok := got["T2"]; !ok || t2.Priority != task.PriorityHigh {
		t.Errorf("T2 = %+v, %v: A deleted it but B edited it unseen, so it must stay with B's edit", t2, ok)
	}
	if _, ok := got["T3"]; ok {
		t.Error("T3 was deleted by A and touched by nobody: it must be gone")
	}
	if _, ok := got[fuel.ID]; !ok {
		t.Error("the task A created offline is missing")
	}
	if _, ok := got[gasket.ID]; !ok {
		t.Error("the task B created offline is missing")
	}
	if len(got) != 4 {
		t.Errorf("%d tasks, want 4 (T1, T2, %s, %s)", len(got), fuel.ID, gasket.ID)
	}
	st := tr.Stats()
	if st.Partitioned == 0 || st.Passed == 0 {
		t.Errorf("the switch did nothing: %+v", st)
	}
}

func TestSyncSurvivesALossyNetworkThatEatsRequestsAndResponses(t *testing.T) {
	ctx := context.Background()
	var lostReq, lostResp, failedRounds int64
	for seed := int64(1); seed <= 15; seed++ {
		rng := rand.New(rand.NewSource(seed))
		clk := &fakeClock{t0}
		a, b := newStore(t, "node-a", clk), newStore(t, "node-b", clk)
		add(t, a, "seed task")
		srv := httptest.NewServer(Handler(a, ServerOptions{Key: key, Now: clk.now}))
		lt := &lossy{base: http.DefaultTransport, rng: rand.New(rand.NewSource(seed * 77)), reqLoss: 0.2, respLoss: 0.2}
		cl := &Client{Peer: srv.URL, Key: key, Now: clk.now, Batch: 1 + rng.Intn(4), HTTP: &http.Client{Transport: lt, Timeout: 5 * time.Second}}
		// B gets the first task through a clean network, so both hold foreign ops afterwards
		clean := &Client{Peer: srv.URL, Key: key, Now: clk.now}
		if _, err := clean.Once(ctx, b); err != nil {
			t.Fatal(err)
		}
		if _, err := clean.Once(ctx, a); err == nil {
			// a syncing with its own server is refused (same node ID): that is the point of this line
			t.Fatal("a replica synced with itself")
		}

		stores := []*sqlstore.Store{a, b}
		for step := 0; step < 60; step++ {
			clk.t = clk.t.Add(time.Duration(1+rng.Intn(3)) * time.Second)
			s := stores[rng.Intn(2)]
			all, _ := s.List(ctx, task.Filter{})
			pick := func() (task.Task, bool) {
				if len(all) == 0 {
					return task.Task{}, false
				}
				return all[rng.Intn(len(all))], true
			}
			switch rng.Intn(8) {
			case 0:
				add(t, s, fmt.Sprintf("t%d-%s", step, s.NodeID()))
			case 1:
				if tk, ok := pick(); ok {
					st := []task.Status{task.StatusOpen, task.StatusInProgress, task.StatusDone}[rng.Intn(3)]
					s.Update(ctx, tk.ID, task.Changes{Status: &st})
				}
			case 2, 3:
				if tk, ok := pick(); ok {
					s.AddNote(ctx, tk.ID, fmt.Sprintf("n%d", step))
				}
			case 4:
				if tk, ok := pick(); ok && len(tk.Notes) > 0 {
					s.RemoveNote(ctx, tk.ID, 1+rng.Intn(len(tk.Notes)))
				}
			case 5:
				if tk, ok := pick(); ok && rng.Intn(3) == 0 {
					s.DeleteTask(ctx, tk.ID)
				}
			default:
				// B tries to sync, over the bad network. Failures are normal.
				if _, err := cl.Once(ctx, b); err != nil {
					failedRounds++
				}
				verify(t, a)
				verify(t, b) // a half-finished round must leave a consistent replica
			}
		}
		// the network heals; a few clean rounds settle everything
		for i := 0; i < 3 && !inStep(t, a, b); i++ {
			if _, err := clean.Once(ctx, b); err != nil {
				t.Fatalf("seed %d: clean round: %v", seed, err)
			}
		}
		if !inStep(t, a, b) {
			t.Fatalf("seed %d: replicas still differ after the network healed", seed)
		}
		verify(t, a)
		verify(t, b)
		lostReq += lt.lostReq.Load()
		lostResp += lt.lostResp.Load()
		srv.Close()
	}
	if lostReq < 20 || lostResp < 20 || failedRounds < 10 {
		t.Fatalf("the chaos was too gentle to prove anything: %d lost requests, %d lost responses, %d failed rounds", lostReq, lostResp, failedRounds)
	}
}

func TestACableCutAtAnyPointInARoundLeavesBothReplicasConsistent(t *testing.T) {
	ctx := context.Background()
	for cut := int64(1); cut <= 12; cut++ {
		clk := &fakeClock{t0}
		a, b := newStore(t, "node-a", clk), newStore(t, "node-b", clk)
		for i := 0; i < 6; i++ {
			clk.t = clk.t.Add(time.Second)
			add(t, a, fmt.Sprintf("a%d", i))
		}
		srv := httptest.NewServer(Handler(a, ServerOptions{Key: key, Now: clk.now}))
		clean := &Client{Peer: srv.URL, Key: key, Now: clk.now}
		if _, err := clean.Once(ctx, b); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 5; i++ {
			clk.t = clk.t.Add(time.Second)
			add(t, a, fmt.Sprintf("a-more%d", i))
			add(t, b, fmt.Sprintf("b%d", i))
		}
		cutter := &lossy{base: http.DefaultTransport, limit: cut}
		cl := &Client{Peer: srv.URL, Key: key, Now: clk.now, Batch: 2, HTTP: &http.Client{Transport: cutter, Timeout: 5 * time.Second}}
		_, err := cl.Once(ctx, b)
		if cut < 7 && err == nil {
			// a full round here is 7 requests: 1 summary, 3 pulls, 3 pushes (5 ops, 2 at a time)
			t.Fatalf("cut after %d requests but the round succeeded", cut)
		}
		verify(t, a)
		verify(t, b)
		if _, err := clean.Once(ctx, b); err != nil {
			t.Fatalf("cut %d: the next round failed: %v", cut, err)
		}
		if !inStep(t, a, b) {
			t.Fatalf("cut %d: not in step after the next round", cut)
		}
		srv.Close()
	}
}
