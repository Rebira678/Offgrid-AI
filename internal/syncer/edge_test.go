package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
)

// TestSyncThreeReplicasMeshConvergence tests full mesh convergence among three nodes:
// Lidia, Beka, and Chala. Each creates tasks offline, syncs with peers in an arbitrary
// topology (Lidia <-> Beka, Beka <-> Chala, Chala <-> Lidia), and proves that all three
// replicas converge to the exact same state hash and task set.
func TestSyncThreeReplicasMeshConvergence(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{t0}

	lidia := newStore(t, "node-lidia", clk)
	beka := newStore(t, "node-beka", clk)
	chala := newStore(t, "node-chala", clk)

	// Step 1: Lidia seeds the initial task T1
	clk.t = clk.t.Add(time.Second)
	t1 := add(t, lidia, "Pump inspection (Lidia)")

	// Step 2: Beka and Chala pull from Lidia so their databases hold foreign ops
	clLidia, _ := serve(t, lidia, clk, key)
	if _, err := clLidia.Once(ctx, beka); err != nil {
		t.Fatalf("seed sync beka <- lidia: %v", err)
	}
	if _, err := clLidia.Once(ctx, chala); err != nil {
		t.Fatalf("seed sync chala <- lidia: %v", err)
	}

	// Step 3: All three nodes go offline and create concurrent tasks.
	// Because Beka and Chala hold foreign ops, their new task IDs will be scoped:
	// e.g. T2.node-beka and T2.node-chala.
	clk.t = clk.t.Add(time.Minute)
	t2 := add(t, beka, "Filter cleaning (Beka)")
	clk.t = clk.t.Add(time.Minute)
	t3 := add(t, chala, "Valve grease (Chala)")

	// Lidia also updates T1 while offline
	done := task.StatusDone
	if _, _, err := lidia.Update(ctx, t1.ID, task.Changes{Status: &done}); err != nil {
		t.Fatalf("lidia update: %v", err)
	}

	// Step 4: Three-way sync mesh propagation
	clBeka, _ := serve(t, beka, clk, key)
	if _, err := clBeka.Once(ctx, lidia); err != nil {
		t.Fatalf("sync lidia <-> beka: %v", err)
	}

	clChala, _ := serve(t, chala, clk, key)
	if _, err := clChala.Once(ctx, beka); err != nil {
		t.Fatalf("sync beka <-> chala: %v", err)
	}

	if _, err := clLidia.Once(ctx, chala); err != nil {
		t.Fatalf("sync chala <-> lidia: %v", err)
	}

	// Propagation round so all 3 nodes hold all ops
	if _, err := clBeka.Once(ctx, lidia); err != nil {
		t.Fatalf("final sync lidia <-> beka: %v", err)
	}
	if _, err := clChala.Once(ctx, beka); err != nil {
		t.Fatalf("final sync beka <-> chala: %v", err)
	}

	hLidia := hash(t, lidia)
	hBeka := hash(t, beka)
	hChala := hash(t, chala)

	if hLidia != hBeka || hBeka != hChala {
		t.Fatalf("hashes diverged! Lidia=%s, Beka=%s, Chala=%s", hLidia, hBeka, hChala)
	}

	// Verify all 3 tasks exist on all 3 nodes
	for name, st := range map[string]*sqlstore.Store{"Lidia": lidia, "Beka": beka, "Chala": chala} {
		list, err := st.List(ctx, task.Filter{})
		if err != nil {
			t.Fatalf("%s: list failed: %v", name, err)
		}
		if len(list) != 3 {
			t.Fatalf("%s has %d tasks, want 3", name, len(list))
		}
		for _, wantID := range []string{t1.ID, t2.ID, t3.ID} {
			if _, err := st.Get(ctx, wantID); err != nil {
				t.Fatalf("%s missing task %s: %v", name, wantID, err)
			}
		}
	}
}

// TestSyncConcurrentClientsStress tests multiple concurrent sync clients hitting the same
// sync server simultaneously, ensuring no data races or lock contentions cause corruption.
func TestSyncConcurrentClientsStress(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{t0}

	serverStore := newStore(t, "node-server", clk)
	for i := 0; i < 5; i++ {
		clk.t = clk.t.Add(time.Second)
		add(t, serverStore, fmt.Sprintf("Server task %d", i))
	}

	cl, _ := serve(t, serverStore, clk, key)

	const numWorkers = 6
	var wg sync.WaitGroup
	errCh := make(chan error, numWorkers)

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			wStore, err := sqlstore.Open(ctx, filepath.Join(t.TempDir(), fmt.Sprintf("worker-%d.db", workerID)),
				sqlstore.Options{NodeID: fmt.Sprintf("node-w%d", workerID), Now: clk.now})
			if err != nil {
				errCh <- err
				return
			}
			defer wStore.Close()

			wClient := &Client{Peer: cl.Peer, Key: key, Node: fmt.Sprintf("node-w%d", workerID), Now: clk.now}

			// Seed sync: pull initial tasks from server so worker holds foreign ops
			if _, err := wClient.Once(ctx, wStore); err != nil {
				errCh <- fmt.Errorf("worker %d initial seed sync failed: %w", workerID, err)
				return
			}

			// Now create worker-specific task with scoped ID (e.g. T6.node-w0)
			if _, _, err := wStore.Create(ctx, task.NewTask{Title: fmt.Sprintf("Worker %d task", workerID), Priority: task.PriorityLow}); err != nil {
				errCh <- err
				return
			}

			// Push worker's new task back to server
			if _, err := wClient.Once(ctx, wStore); err != nil {
				errCh <- fmt.Errorf("worker %d sync failed: %w", workerID, err)
				return
			}
		}(w)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}

	// Server should now have 5 original tasks + numWorkers tasks
	list, err := serverStore.List(ctx, task.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 5+numWorkers {
		t.Fatalf("server has %d tasks, want %d", len(list), 5+numWorkers)
	}
}

// TestSyncAuthHeaderEdgeCases verifies exact boundary checks and edge cases on request headers.
func TestSyncAuthHeaderEdgeCases(t *testing.T) {
	clk := &fakeClock{t0}
	st := newStore(t, "node-test", clk)
	_, srv := serve(t, st, clk, key)

	cases := []struct {
		name       string
		method     string
		path       string
		headers    map[string]string
		body       string
		wantStatus int
	}{
		{
			name:       "Missing X-Offgrid-Node header",
			method:     http.MethodGet,
			path:       PathSummary,
			headers:    map[string]string{HeaderTime: fmt.Sprint(t0.Unix()), HeaderSig: "dummy"},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "Missing X-Offgrid-Time header",
			method:     http.MethodGet,
			path:       PathSummary,
			headers:    map[string]string{HeaderNode: "n", HeaderSig: "dummy"},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "Invalid non-integer X-Offgrid-Time",
			method:     http.MethodGet,
			path:       PathSummary,
			headers:    map[string]string{HeaderNode: "n", HeaderTime: "yesterday", HeaderSig: "dummy"},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "Truncated hex signature",
			method:     http.MethodGet,
			path:       PathSummary,
			headers:    map[string]string{HeaderNode: "n", HeaderTime: fmt.Sprint(t0.Unix()), HeaderSig: "1234abcd"},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:   "Signature for POST /sync/ops sent on POST /sync/push",
			method: http.MethodPost,
			path:   PathPush,
			headers: map[string]string{
				HeaderNode: "n",
				HeaderTime: fmt.Sprint(t0.Unix()),
				HeaderSig:  SignRequest(key, http.MethodPost, PathOps, "n", t0.Unix(), []byte("{}")),
			},
			body:       "{}",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:   "Exact window boundary: exactly at window (5m)",
			method: http.MethodGet,
			path:   PathSummary,
			headers: map[string]string{
				HeaderNode: "n",
				HeaderTime: strconv.FormatInt(t0.Add(5*time.Minute).Unix(), 10),
				HeaderSig:  SignRequest(key, http.MethodGet, PathSummary, "n", t0.Add(5*time.Minute).Unix(), nil),
			},
			wantStatus: http.StatusOK,
		},
		{
			name:   "Outside window boundary: 5m + 1s ahead",
			method: http.MethodGet,
			path:   PathSummary,
			headers: map[string]string{
				HeaderNode: "n",
				HeaderTime: strconv.FormatInt(t0.Add(5*time.Minute+time.Second).Unix(), 10),
				HeaderSig:  SignRequest(key, http.MethodGet, PathSummary, "n", t0.Add(5*time.Minute+time.Second).Unix(), nil),
			},
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, _ := raw(t, srv, tc.method, tc.path, tc.headers, tc.body)
			if status != tc.wantStatus {
				t.Errorf("%s: got status %d, want %d", tc.name, status, tc.wantStatus)
			}
		})
	}
}

// TestSyncContextCancellation verifies that canceling client context aborts sync cleanly.
func TestSyncContextCancellation(t *testing.T) {
	clk := &fakeClock{t0}
	stA := newStore(t, "node-a", clk)
	stB := newStore(t, "node-b", clk)

	add(t, stA, "Task before cancel")

	cl, _ := serve(t, stB, clk, key)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := cl.Once(ctx, stA)
	if err == nil {
		t.Fatal("expected error on cancelled context, got nil")
	}
	if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("expected context.Canceled error, got: %v", err)
	}
}

// TestSyncSelfSyncRefusal verifies that a replica refuses to sync against itself.
func TestSyncSelfSyncRefusal(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{t0}
	st := newStore(t, "node-same", clk)

	cl, _ := serve(t, st, clk, key)
	// Client and server have the same NodeID
	cl.Node = "node-same"

	_, err := cl.Once(ctx, st)
	if err == nil || !strings.Contains(err.Error(), "the peer is this replica") {
		t.Fatalf("expected self-sync error, got %v", err)
	}
}

func mockServer(t *testing.T, key []byte, h func(r *http.Request) (status int, body any)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqSig := r.Header.Get(HeaderSig)
		status, repBody := h(r)
		b, _ := json.Marshal(repBody)
		w.Header().Set("Content-Type", "application/json")
		if reqSig != "" {
			w.Header().Set(HeaderSig, SignResponse(key, reqSig, status, b))
			w.Header().Set(HeaderNode, "mock-peer")
		}
		w.WriteHeader(status)
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestSyncPullEmptyBatchWithMore guards against infinite loop if a peer sends empty ops with more=true.
func TestSyncPullEmptyBatchWithMore(t *testing.T) {
	clk := &fakeClock{t0}
	st := newStore(t, "node-local", clk)
	srv := mockServer(t, key, func(r *http.Request) (int, any) {
		switch r.URL.Path {
		case PathSummary:
			return http.StatusOK, SummaryResponse{Node: "mock-peer", Protocol: ProtocolVersion, Summary: sqlstore.Summary{}}
		case PathOps:
			return http.StatusOK, OpsResponse{Ops: []WireOp{}, More: true}
		default:
			return http.StatusNotFound, nil
		}
	})
	cl := &Client{Peer: srv.URL, Key: key, Now: clk.now}
	_, err := cl.Once(context.Background(), st)
	if err == nil || !strings.Contains(err.Error(), "the peer reported more ops but sent an empty batch; stopping") {
		t.Fatalf("expected empty batch infinite loop guard error, got: %v", err)
	}
}

type stallReplica struct {
	Replica
}

func (s *stallReplica) OpsSince(ctx context.Context, have sqlstore.Summary, limit int) ([]sqlstore.Op, bool, error) {
	op := sqlstore.Op{
		NodeID:  "node-stall",
		Seq:     1,
		TS:      t0,
		Counter: 0,
		Kind:    sqlstore.KindTaskCreated,
		Payload: []byte(`{"id":"T1","title":"Stall","priority":"low"}`),
	}
	return []sqlstore.Op{op}, true, nil
}

// TestSyncPushStallGuard guards against infinite loop if push loop cannot advance the have cursor.
func TestSyncPushStallGuard(t *testing.T) {
	clk := &fakeClock{t0}
	st := newStore(t, "node-peer", clk)
	cl, _ := serve(t, st, clk, key)

	fake := &stallReplica{Replica: newStore(t, "node-local", clk)}
	_, err := cl.Once(context.Background(), fake)
	if err == nil || !strings.Contains(err.Error(), "cannot advance push cursor; stopping") {
		t.Fatalf("expected push cursor stall error, got: %v", err)
	}
}

// TestSyncNilPeerSummary tests that nil peer summary is handled gracefully without panicking.
func TestSyncNilPeerSummary(t *testing.T) {
	clk := &fakeClock{t0}
	st := newStore(t, "node-local", clk)
	add(t, st, "Task for nil summary test")

	srv := mockServer(t, key, func(r *http.Request) (int, any) {
		switch r.URL.Path {
		case PathSummary:
			return http.StatusOK, SummaryResponse{Node: "mock-peer", Protocol: ProtocolVersion, Summary: nil}
		case PathOps:
			return http.StatusOK, OpsResponse{Ops: []WireOp{}, More: false}
		case PathPush:
			return http.StatusOK, PushResponse{Applied: 1, Duplicates: 0, Rebuilt: false}
		default:
			return http.StatusNotFound, nil
		}
	})
	cl := &Client{Peer: srv.URL, Key: key, Now: clk.now}
	rep, err := cl.Once(context.Background(), st)
	if err != nil {
		t.Fatalf("unexpected error with nil peer summary: %v", err)
	}
	if rep.Pushed != 1 {
		t.Fatalf("expected 1 op pushed, got %d", rep.Pushed)
	}
}
