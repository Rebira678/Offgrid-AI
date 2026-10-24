package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
)

var (
	t0  = time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	key = []byte("0123456789abcdef-test-key")
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func newStore(t testing.TB, node string, clk *fakeClock) *sqlstore.Store {
	t.Helper()
	s, err := sqlstore.Open(context.Background(), filepath.Join(t.TempDir(), "x.db"), sqlstore.Options{NodeID: node, Now: clk.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func add(t testing.TB, s *sqlstore.Store, title string) task.Task {
	t.Helper()
	tk, _, err := s.Create(context.Background(), task.NewTask{Title: title, Priority: task.PriorityMedium})
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func hash(t testing.TB, s *sqlstore.Store) string {
	t.Helper()
	h, err := s.StateHash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// serve starts a server for rep and returns a client for it.
func serve(t testing.TB, rep Replica, clk *fakeClock, k []byte) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(Handler(rep, ServerOptions{Key: k, Now: clk.now}))
	t.Cleanup(srv.Close)
	return &Client{Peer: srv.URL, Key: key, Now: clk.now}, srv
}

func TestTwoReplicasExchangeOpsOverHTTPAndEndUpIdentical(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{t0}
	a, b := newStore(t, "node-a", clk), newStore(t, "node-b", clk)
	for i := 0; i < 6; i++ {
		clk.t = clk.t.Add(time.Second)
		add(t, a, fmt.Sprintf("a%d", i))
	}
	// b is the server; a is the client, with tiny batches so the loops run
	cl, _ := serve(t, b, clk, key)
	cl.Batch = 2
	rep, err := cl.Once(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Pushed != 6 || rep.Pulled != 0 {
		t.Fatalf("report = %+v, want 6 pushed", rep)
	}
	if hash(t, a) != hash(t, b) {
		t.Fatal("state hashes differ after the push")
	}
	// now b has work of its own; a pulls it
	clk.t = clk.t.Add(time.Minute)
	add(t, b, "from b")
	rep, err = cl.Once(ctx, a)
	if err != nil || rep.Pulled != 1 || rep.Pushed != 0 {
		t.Fatalf("report = %+v err=%v, want 1 pulled", rep, err)
	}
	if hash(t, a) != hash(t, b) {
		t.Fatal("state hashes differ after the pull")
	}
	// a round between replicas that are already in step moves nothing, in two requests
	rep, err = cl.Once(ctx, a)
	if err != nil || rep.Pulled+rep.Pushed != 0 || rep.Requests != 2 {
		t.Fatalf("idle round = %+v err=%v, want nothing moved in 2 requests", rep, err)
	}
}

func TestEditsMadeWhileApartMergeInOneRound(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{t0}
	a, b := newStore(t, "node-a", clk), newStore(t, "node-b", clk)
	add(t, a, "pump")
	cl, _ := serve(t, b, clk, key)
	if _, err := cl.Once(ctx, a); err != nil {
		t.Fatal(err)
	}
	// both go offline and change things
	done, prog := task.StatusDone, task.StatusInProgress
	clk.t = clk.t.Add(time.Minute)
	if _, _, err := a.Update(ctx, "T1", task.Changes{Status: &prog}); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Minute)
	if _, _, err := b.Update(ctx, "T1", task.Changes{Status: &done}); err != nil {
		t.Fatal(err)
	}
	add(t, a, "from a")
	add(t, b, "from b")
	if _, err := cl.Once(ctx, a); err != nil {
		t.Fatal(err)
	}
	if hash(t, a) != hash(t, b) {
		t.Fatal("replicas differ after reconnecting")
	}
	got, _ := a.Get(ctx, "T1")
	if got.Status != task.StatusDone {
		t.Fatalf("status = %s, want the later edit (done) to win", got.Status)
	}
	list, _ := a.List(ctx, task.Filter{})
	if len(list) != 3 {
		t.Fatalf("%d tasks, want 3", len(list))
	}
}

func TestAWrongKeyIsRefusedAndNothingChanges(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{t0}
	a, b := newStore(t, "node-a", clk), newStore(t, "node-b", clk)
	add(t, a, "secret")
	cl, _ := serve(t, b, clk, key)
	cl.Key = []byte("a-completely-different-key")
	_, err := cl.Once(ctx, a)
	var re *RemoteError
	if !errors.As(err, &re) || re.Status != 401 || re.Code != CodeUnauthorized {
		t.Fatalf("err = %v, want a 401 unauthorized", err)
	}
	s, _ := b.Summary(ctx)
	if len(s) != 0 {
		t.Fatal("the server took ops from a caller with the wrong key")
	}
}

func raw(t *testing.T, srv *httptest.Server, method, path string, hdr map[string]string, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func signed(method, path, node string, at time.Time, body string) map[string]string {
	return map[string]string{
		HeaderNode: node,
		HeaderTime: fmt.Sprint(at.Unix()),
		HeaderSig:  SignRequest(key, method, path, node, at.Unix(), []byte(body)),
	}
}

func TestRequestsThatAreUnsignedTamperedOrStaleAreRefused(t *testing.T) {
	clk := &fakeClock{t0}
	b := newStore(t, "node-b", clk)
	_, srv := serve(t, b, clk, key)

	if code, _ := raw(t, srv, "GET", PathSummary, nil, ""); code != 401 {
		t.Errorf("unsigned: %d, want 401", code)
	}
	if code, _ := raw(t, srv, "GET", PathSummary, signed("GET", PathSummary, "n", t0, ""), ""); code != 200 {
		t.Errorf("a correctly signed request: %d, want 200", code)
	}
	// the body is changed after signing
	h := signed("POST", PathOps, "n", t0, `{"have":{},"limit":1}`)
	if code, _ := raw(t, srv, "POST", PathOps, h, `{"have":{},"limit":9}`); code != 401 {
		t.Errorf("tampered body: %d, want 401", code)
	}
	// a signature for one path used on another
	h = signed("GET", PathSummary, "n", t0, "")
	if code, _ := raw(t, srv, "POST", PathOps, h, ""); code != 401 {
		t.Errorf("signature reused on another path: %d, want 401", code)
	}
	// the sender name is part of what is signed
	h = signed("GET", PathSummary, "n", t0, "")
	h[HeaderNode] = "someone-else"
	if code, _ := raw(t, srv, "GET", PathSummary, h, ""); code != 401 {
		t.Errorf("changed node name: %d, want 401", code)
	}
	// stale and future requests, correctly signed
	for name, at := range map[string]time.Time{"stale": t0.Add(-6 * time.Minute), "future": t0.Add(6 * time.Minute)} {
		code, body := raw(t, srv, "GET", PathSummary, signed("GET", PathSummary, "n", at, ""), "")
		var e ErrorResponse
		json.Unmarshal([]byte(body), &e)
		if code != 401 || e.Code != CodeClockSkew || !strings.Contains(e.Error, "6m0s") {
			t.Errorf("%s: %d %q, want 401 clock_skew naming 6m0s", name, code, body)
		}
	}
	// just inside the window is accepted
	if code, _ := raw(t, srv, "GET", PathSummary, signed("GET", PathSummary, "n", t0.Add(-4*time.Minute), ""), ""); code != 200 {
		t.Errorf("4 minutes old: %d, want 200", code)
	}
	// an unsigned or badly signed request must not learn the clock difference
	h = signed("GET", PathSummary, "n", t0.Add(-time.Hour), "")
	h[HeaderSig] = strings.Repeat("0", 64)
	if _, body := raw(t, srv, "GET", PathSummary, h, ""); strings.Contains(body, "clocks differ") {
		t.Error("the server revealed its clock to a caller that could not sign")
	}
	// garbage and unknown fields
	if code, _ := raw(t, srv, "POST", PathOps, signed("POST", PathOps, "n", t0, "{nope"), "{nope"); code != 400 {
		t.Errorf("garbage body: %d, want 400", code)
	}
	bad := `{"have":{},"limit":1,"extra":true}`
	if code, _ := raw(t, srv, "POST", PathOps, signed("POST", PathOps, "n", t0, bad), bad); code != 400 {
		t.Errorf("unknown field: %d, want 400", code)
	}
	// the wrong method
	if code, _ := raw(t, srv, "GET", PathPush, signed("GET", PathPush, "n", t0, ""), ""); code != 405 {
		t.Errorf("GET on push: %d, want 405", code)
	}
}

func TestAServerThatDoesNotKnowTheKeyCannotPassAsThePeer(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{t0}
	a := newStore(t, "node-a", clk)
	// an impostor answers with valid JSON and no signature
	imp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(SummaryResponse{Node: "node-b", Protocol: 1, Summary: sqlstore.Summary{}})
	}))
	defer imp.Close()
	cl := &Client{Peer: imp.URL, Key: key, Now: clk.now}
	if _, err := cl.Once(ctx, a); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("unsigned reply: err = %v, want ErrBadSignature", err)
	}
	// one that signs with another key
	imp2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := json.Marshal(SummaryResponse{Node: "node-b", Protocol: 1, Summary: sqlstore.Summary{}})
		w.Header().Set(HeaderSig, SignResponse([]byte("not-the-shared-key!!"), r.Header.Get(HeaderSig), 200, b))
		w.Write(b)
	}))
	defer imp2.Close()
	cl = &Client{Peer: imp2.URL, Key: key, Now: clk.now}
	if _, err := cl.Once(ctx, a); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("wrongly signed reply: err = %v, want ErrBadSignature", err)
	}
	// a genuine response replayed for a different request does not verify
	b := newStore(t, "node-b", clk)
	h := Handler(b, ServerOptions{Key: key, Now: clk.now})
	var captured http.Header
	var capturedBody []byte
	cap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		captured, capturedBody = rec.Header().Clone(), rec.Body.Bytes()
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.Write(rec.Body.Bytes())
	}))
	defer cap.Close()
	cl = &Client{Peer: cap.URL, Key: key, Now: clk.now}
	if _, err := cl.PeerSummary(ctx); err != nil {
		t.Fatal(err)
	}
	replay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(HeaderSig, captured.Get(HeaderSig))
		w.Write(capturedBody)
	}))
	defer replay.Close()
	clk.t = clk.t.Add(time.Second) // a different request time, so a different signature
	cl = &Client{Peer: replay.URL, Key: key, Now: clk.now}
	if _, err := cl.PeerSummary(ctx); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("replayed response: err = %v, want ErrBadSignature", err)
	}
}

func TestACorruptedResponseBodyIsDetected(t *testing.T) {
	clk := &fakeClock{t0}
	b := newStore(t, "node-b", clk)
	add(t, b, "x")
	h := Handler(b, ServerOptions{Key: key, Now: clk.now})
	mitm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.Write(bytes.Replace(rec.Body.Bytes(), []byte(`"node-b":1`), []byte(`"node-b":9`), 1))
	}))
	defer mitm.Close()
	cl := &Client{Peer: mitm.URL, Key: key, Now: clk.now}
	if _, err := cl.PeerSummary(context.Background()); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("err = %v, want ErrBadSignature", err)
	}
}

func TestSyncingWithYourselfOrAClockFarOffGivesAClearError(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{t0}
	a := newStore(t, "node-a", clk)
	same := newStore(t, "node-a", clk)
	cl, _ := serve(t, same, clk, key)
	if _, err := cl.Once(ctx, a); err == nil || !strings.Contains(err.Error(), "this replica") {
		t.Fatalf("err = %v, want a same-node-ID error", err)
	}
	// the client's clock is 20 minutes off
	b := newStore(t, "node-b", clk)
	cl, _ = serve(t, b, clk, key)
	cl.Now = (&fakeClock{t0.Add(20 * time.Minute)}).now
	_, err := cl.Once(ctx, a)
	var re *RemoteError
	if !errors.As(err, &re) || re.Code != CodeClockSkew || !strings.Contains(re.Msg, "20m0s") {
		t.Fatalf("err = %v, want clock_skew naming 20m0s", err)
	}
}

func TestIDCollisionAndForkAreReportedByCodeAndNothingIsKept(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{t0}
	a, b := newStore(t, "node-a", clk), newStore(t, "node-b", clk)
	add(t, a, "pump")
	add(t, b, "other")
	cl, _ := serve(t, b, clk, key)
	_, err := cl.Once(ctx, a)
	var re *RemoteError
	if !errors.As(err, &re) && !errors.Is(err, sqlstore.ErrIDCollision) {
		t.Fatalf("err = %v, want an ID collision", err)
	}
	// whichever half failed, neither side kept anything half-applied
	for _, s := range []*sqlstore.Store{a, b} {
		if _, err := s.Verify(ctx); err != nil {
			t.Fatalf("Verify after the refused round: %v", err)
		}
	}
	sa, _ := a.Summary(ctx)
	sb, _ := b.Summary(ctx)
	if len(sa) != 1 || len(sb) != 1 {
		t.Fatalf("summaries changed: %v %v", sa, sb)
	}
}

func TestAServerRefusesABatchWithAGapWith409(t *testing.T) {
	clk := &fakeClock{t0}
	a, b := newStore(t, "node-a", clk), newStore(t, "node-b", clk)
	for i := 0; i < 3; i++ {
		add(t, a, fmt.Sprintf("t%d", i))
	}
	ops, _, _ := a.OpsSince(context.Background(), sqlstore.Summary{}, 10)
	_, srv := serve(t, b, clk, key)
	body, _ := json.Marshal(PushRequest{Ops: ToWire(ops[1:])})
	code, resp := raw(t, srv, "POST", PathPush, signed("POST", PathPush, "node-a", t0, string(body)), string(body))
	var e ErrorResponse
	json.Unmarshal([]byte(resp), &e)
	if code != 409 || e.Code != CodeGap {
		t.Fatalf("%d %q, want 409 gap", code, resp)
	}
}

func TestOpsSurviveTheWireUnchanged(t *testing.T) {
	clk := &fakeClock{t0.Add(123456789 * time.Nanosecond)}
	a := newStore(t, "node-a", clk)
	add(t, a, "ünïcode \"quoted\" — ፓምፕ")
	ops, _, _ := a.OpsSince(context.Background(), sqlstore.Summary{}, 10)
	b, _ := json.Marshal(ToWire(ops))
	var w []WireOp
	if err := json.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	back := FromWire(w)
	if len(back) != 1 || !back[0].TS.Equal(ops[0].TS) || back[0].TS.Nanosecond() != 123456789+0 && back[0].TS.Nanosecond() != ops[0].TS.Nanosecond() {
		t.Fatalf("timestamp changed: %v -> %v", ops[0].TS, back[0].TS)
	}
	if back[0].Counter != ops[0].Counter || back[0].Seq != ops[0].Seq || string(back[0].Payload) != string(ops[0].Payload) {
		t.Fatalf("op changed in transit: %+v -> %+v", ops[0], back[0])
	}
}

func TestLoadKey(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "k")
	os.WriteFile(good, []byte("  0123456789abcdef0123  \n"), 0o600)
	k, err := LoadKey(good)
	if err != nil || string(k) != "0123456789abcdef0123" {
		t.Fatalf("got %q, %v", k, err)
	}
	short := filepath.Join(dir, "s")
	os.WriteFile(short, []byte("tooshort"), 0o600)
	if _, err := LoadKey(short); !errors.Is(err, ErrKey) {
		t.Errorf("short key: %v, want ErrKey", err)
	}
	if _, err := LoadKey(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing file was accepted")
	}
	t.Setenv("OFFGRID_SYNC_KEY", "env-key-0123456789abc")
	if k, err := LoadKey(""); err != nil || string(k) != "env-key-0123456789abc" {
		t.Errorf("env key: %q %v", k, err)
	}
	t.Setenv("OFFGRID_SYNC_KEY", "")
	if _, err := LoadKey(""); !errors.Is(err, ErrKey) {
		t.Errorf("no key at all: %v, want ErrKey", err)
	}
}

func TestSignaturesDependOnEveryInput(t *testing.T) {
	base := SignRequest(key, "POST", "/p", "n", 100, []byte("b"))
	variants := []string{
		SignRequest([]byte("another-key-0123456789"), "POST", "/p", "n", 100, []byte("b")),
		SignRequest(key, "GET", "/p", "n", 100, []byte("b")),
		SignRequest(key, "POST", "/q", "n", 100, []byte("b")),
		SignRequest(key, "POST", "/p", "m", 100, []byte("b")),
		SignRequest(key, "POST", "/p", "n", 101, []byte("b")),
		SignRequest(key, "POST", "/p", "n", 100, []byte("c")),
	}
	for i, v := range variants {
		if v == base {
			t.Errorf("variant %d gives the same signature", i)
		}
	}
	rs := SignResponse(key, base, 200, []byte("x"))
	for i, v := range []string{
		SignResponse(key, "other", 200, []byte("x")),
		SignResponse(key, base, 500, []byte("x")),
		SignResponse(key, base, 200, []byte("y")),
		SignResponse([]byte("another-key-0123456789"), base, 200, []byte("x")),
	} {
		if v == rs {
			t.Errorf("response variant %d gives the same signature", i)
		}
	}
}

func TestTheTimeWindowIncludesItsEdgeAndNothingBeyond(t *testing.T) {
	clk := &fakeClock{t0}
	b := newStore(t, "node-b", clk)
	_, srv := serve(t, b, clk, key)
	for name, c := range map[string]struct {
		at   time.Time
		want int
	}{
		"exactly 5m old":   {t0.Add(-5 * time.Minute), 200},
		"exactly 5m ahead": {t0.Add(5 * time.Minute), 200},
		"5m1s old":         {t0.Add(-5*time.Minute - time.Second), 401},
		"5m1s ahead":       {t0.Add(5*time.Minute + time.Second), 401},
	} {
		if code, _ := raw(t, srv, "GET", PathSummary, signed("GET", PathSummary, "n", c.at, ""), ""); code != c.want {
			t.Errorf("%s: %d, want %d", name, code, c.want)
		}
	}
}

func TestTheServerCapsHowManyOpsItSendsAndTakes(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{t0}
	a, b := newStore(t, "node-a", clk), newStore(t, "node-b", clk)
	for i := 0; i < 9; i++ {
		add(t, a, fmt.Sprintf("t%d", i))
	}
	srv := httptest.NewServer(Handler(a, ServerOptions{Key: key, Now: clk.now, MaxBatch: 3}))
	defer srv.Close()
	body := `{"have":{},"limit":1000}`
	code, resp := raw(t, srv, "POST", PathOps, signed("POST", PathOps, "n", t0, body), body)
	var or OpsResponse
	json.Unmarshal([]byte(resp), &or)
	if code != 200 || len(or.Ops) != 3 || !or.More {
		t.Fatalf("a request for 1000 ops got %d ops (more=%v), want the server's cap of 3", len(or.Ops), or.More)
	}
	// a client with a big batch size still converges, in more round trips
	cl := &Client{Peer: srv.URL, Key: key, Batch: 1000, Now: clk.now}
	rep, err := cl.Once(ctx, b)
	if err != nil || rep.Pulled != 9 {
		t.Fatalf("%+v %v", rep, err)
	}
	// pushes beyond four batches are refused outright
	big := PushRequest{Ops: make([]WireOp, 13)}
	pb, _ := json.Marshal(big)
	code, _ = raw(t, srv, "POST", PathPush, signed("POST", PathPush, "n", t0, string(pb)), string(pb))
	if code != 400 {
		t.Fatalf("an oversized push: %d, want 400", code)
	}
}

func TestAPeerOnAnotherProtocolVersionIsRefused(t *testing.T) {
	clk := &fakeClock{t0}
	a := newStore(t, "node-a", clk)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := json.Marshal(SummaryResponse{Node: "node-b", Protocol: ProtocolVersion + 1, Summary: sqlstore.Summary{}})
		w.Header().Set(HeaderSig, SignResponse(key, r.Header.Get(HeaderSig), 200, b))
		w.Write(b)
	}))
	defer peer.Close()
	cl := &Client{Peer: peer.URL, Key: key, Now: clk.now}
	if _, err := cl.Once(context.Background(), a); !errors.Is(err, ErrProtocol) {
		t.Fatalf("err = %v, want ErrProtocol", err)
	}
}
