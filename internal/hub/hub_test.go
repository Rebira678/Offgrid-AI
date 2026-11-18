package hub

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"offgrid/internal/identity"
	"offgrid/internal/store/sqlstore"
	"offgrid/internal/syncer"
	"offgrid/internal/task"
)

// safeClock is the one clock everything in a test shares, so a test can move time (revocation caches,
// the rate limit) without sleeping.
type safeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *safeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *safeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type node struct {
	st *sqlstore.Store
	id *identity.Identity
}

type env struct {
	t     *testing.T
	clk   *safeClock
	hub   *Hub
	hubID *identity.Identity
	dir   string
	addr  string
	srv   *http.Server
}

func newID(t *testing.T) *identity.Identity {
	t.Helper()
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *env) node(name string) node {
	e.t.Helper()
	id := newID(e.t)
	st, err := sqlstore.Open(context.Background(), filepath.Join(e.t.TempDir(), name+".db"), sqlstore.Options{NodeID: name, Now: e.clk.now, Signer: id})
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { st.Close() })
	return node{st, id}
}

func newEnv(t *testing.T, mod ...func(*Options)) *env {
	t.Helper()
	e := &env{t: t, clk: &safeClock{t: time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)}, hubID: newID(t), dir: t.TempDir()}
	e.start(mod...)
	return e
}

func (e *env) start(mod ...func(*Options)) {
	e.t.Helper()
	opt := Options{Dir: e.dir, Identity: e.hubID, Now: e.clk.now}
	for _, m := range mod {
		m(&opt)
	}
	h, err := New(context.Background(), opt)
	if err != nil {
		e.t.Fatal(err)
	}
	srv, err := h.Server()
	if err != nil {
		e.t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		e.t.Fatal(err)
	}
	go srv.ServeTLS(ln, "", "")
	e.hub, e.srv, e.addr = h, srv, ln.Addr().String()
	e.t.Cleanup(func() { srv.Close(); h.Close() })
}

func (e *env) stop() { e.srv.Close(); e.hub.Close() }

func (e *env) client(n node, group string, rt http.RoundTripper) *syncer.Client {
	cfg := syncer.ClientTLSConfig(n.id, syncer.PinDevice(e.hubID.ID()), e.clk.now)
	hc := syncer.NewTLSClient(cfg)
	if rt != nil {
		if tr, ok := hc.Transport.(*http.Transport); ok {
			hc.Transport = wrap(tr, rt)
		}
	}
	return &syncer.Client{Peer: "https://" + e.addr + Prefix + group, Node: "n", Mutual: true, HTTP: hc, Now: e.clk.now}
}

// wrapped is a transport that calls rt with the real transport as its next hop.
type wrapped struct {
	next http.RoundTripper
	f    func(next http.RoundTripper, r *http.Request) (*http.Response, error)
}

func (w wrapped) RoundTrip(r *http.Request) (*http.Response, error) { return w.f(w.next, r) }

type hook func(next http.RoundTripper, r *http.Request) (*http.Response, error)

func wrap(tr *http.Transport, rt http.RoundTripper) http.RoundTripper {
	return wrapped{tr, rt.(hookRT).f}
}

type hookRT struct{ f hook }

func (hookRT) RoundTrip(*http.Request) (*http.Response, error) { panic("not used directly") }

func addTask(t *testing.T, s *sqlstore.Store, title string) {
	t.Helper()
	if _, _, err := s.Create(context.Background(), task.NewTask{Title: title, Priority: task.PriorityMedium}); err != nil {
		t.Fatal(err)
	}
}

func titles(t *testing.T, s *sqlstore.Store) map[string]bool {
	t.Helper()
	ts, err := s.List(context.Background(), task.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]bool{}
	for _, k := range ts {
		m[k.Title] = true
	}
	return m
}

// twoSites founds a group on site A, enrols B, and has the hub host it for A's device.
func (e *env) twoSites() (a, b node) {
	e.t.Helper()
	ctx := context.Background()
	a, b = e.node("site-a"), e.node("site-b")
	if err := e.hub.AddGroup(ctx, "crew", a.id.ID()); err != nil {
		e.t.Fatal(err)
	}
	if err := a.st.Found(ctx, "site a"); err != nil {
		e.t.Fatal(err)
	}
	if err := a.st.AddMember(ctx, b.id.PublicKey(), sqlstore.RoleMember, "site b"); err != nil {
		e.t.Fatal(err)
	}
	return a, b
}

// Two sites on different networks (neither listens on anything) sync only through the hub.
func TestTwoSitesMeetOnlyThroughTheHub(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	a, b := e.twoSites()
	addTask(t, a.st, "from site a")

	// B is not known to the hub until the founder's membership ops have arrived: the handshake itself refuses it.
	if _, err := e.client(b, "crew", nil).Once(ctx, b.st); err == nil {
		t.Fatal("a member the hub has not heard of yet was let in")
	}
	if rep, err := e.client(a, "crew", nil).Once(ctx, a.st); err != nil || rep.Pushed == 0 {
		t.Fatalf("founder's first round: %+v, %v", rep, err)
	}
	if _, err := e.client(b, "crew", nil).Once(ctx, b.st); err != nil {
		t.Fatalf("B after the founder pushed: %v", err)
	}
	if !titles(t, b.st)["from site a"] {
		t.Fatal("B did not receive A's task through the hub")
	}
	addTask(t, b.st, "from site b")
	if _, err := e.client(b, "crew", nil).Once(ctx, b.st); err != nil {
		t.Fatal(err)
	}
	if _, err := e.client(a, "crew", nil).Once(ctx, a.st); err != nil {
		t.Fatal(err)
	}
	ha, _ := a.st.StateHash(ctx)
	hb, _ := b.st.StateHash(ctx)
	if ha != hb || !titles(t, a.st)["from site b"] {
		t.Fatal("the two sites do not agree after meeting at the hub")
	}
	gs, _ := e.hub.Groups(ctx)
	if len(gs) != 1 || !gs[0].Founded || gs[0].Members != 2 || gs[0].Ops < 3 {
		t.Fatalf("hub's view of the group: %+v", gs)
	}
}

// The first membership op of a group is accepted only from the founder named when the hub added it.
func TestOnlyTheNamedFounderCanStartAGroup(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	founder, intruder := e.node("founder"), e.node("intruder")
	if err := e.hub.AddGroup(ctx, "crew", founder.id.ID()); err != nil {
		t.Fatal(err)
	}
	if err := intruder.st.Found(ctx, "squatter"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.client(intruder, "crew", nil).Once(ctx, intruder.st); err == nil {
		t.Fatal("a device that is not the named founder founded the group")
	}
	if err := founder.st.Found(ctx, "real"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.client(founder, "crew", nil).Once(ctx, founder.st); err != nil {
		t.Fatalf("the named founder: %v", err)
	}
	gs, _ := e.hub.Groups(ctx)
	if !gs[0].Founded || gs[0].Members != 1 {
		t.Fatalf("group after founding: %+v", gs[0])
	}
}

func rawGet(t *testing.T, e *env, n node, path string) (int, string) {
	t.Helper()
	cl := e.client(n, "x", nil)
	resp, err := cl.HTTP.Get("https://" + e.addr + path)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// Not a member of the group, and the group not existing, look the same; a stranger cannot even connect.
func TestAMemberOfAnotherGroupLearnsNothingAboutThisOne(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	a, _ := e.twoSites() // group "crew"
	other := e.node("other")
	if err := e.hub.AddGroup(ctx, "farm", other.id.ID()); err != nil {
		t.Fatal(err)
	}
	if err := other.st.Found(ctx, "farm"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.client(other, "farm", nil).Once(ctx, other.st); err != nil {
		t.Fatal(err)
	}
	_ = a
	code1, body1 := rawGet(t, e, other, Prefix+"crew"+syncer.PathSummary)
	code2, body2 := rawGet(t, e, other, Prefix+"nonesuch"+syncer.PathSummary)
	if code1 != http.StatusNotFound || code1 != code2 || body1 != body2 || !strings.Contains(body1, CodeNoSuchGroup) {
		t.Fatalf("a group that exists but is not theirs: %d %s; a group that does not exist: %d %s", code1, body1, code2, body2)
	}
	if code, _ := rawGet(t, e, other, "/elsewhere"); code != http.StatusNotFound {
		t.Fatalf("a path outside /g/: %d", code)
	}
	stranger := e.node("stranger")
	if code, body := rawGet(t, e, stranger, Prefix+"crew"+syncer.PathSummary); code != 0 {
		t.Fatalf("a stranger got an answer: %d %s", code, body)
	}
}

// frames builds a pull answer by hand, as a dishonest hub would send it (over mutual TLS the mac is "-").
func frames(ops []syncer.WireOp) string {
	js, _ := json.Marshal(map[string]any{"ops": ops})
	end, _ := json.Marshal(map[string]any{"end": true, "n": len(ops)})
	return "- " + string(js) + "\n- " + string(end) + "\n"
}

// answerWith replaces what the hub says to a pull; everything else passes through. Gzip is switched off for it.
func answerWith(f func(real []string) string) hookRT {
	return hookRT{func(next http.RoundTripper, r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, syncer.PathOps) {
			return next.RoundTrip(r)
		}
		r = r.Clone(r.Context())
		r.Header.Del("Accept-Encoding")
		resp, err := next.RoundTrip(r)
		if err != nil || resp.StatusCode != 200 {
			return resp, err
		}
		all, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		lines := strings.SplitAfter(string(all), "\n")
		resp.Body = io.NopCloser(strings.NewReader(f(lines)))
		resp.ContentLength = -1
		resp.Header.Del("Content-Length")
		return resp, nil
	}}
}

// The hub cannot be trusted to relay honestly, and the receiving node does not need it to be: a tampered op,
// and an op signed by someone who is not a member, are refused by the node that receives them.
func TestAForgedOpIsRefusedByTheReceivingNodeNotJustTheHub(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	a, b := e.twoSites()
	addTask(t, a.st, "genuine")
	if _, err := e.client(a, "crew", nil).Once(ctx, a.st); err != nil {
		t.Fatal(err)
	}
	// B learns the membership first, so it can judge who may write
	if _, err := e.client(b, "crew", nil).Once(ctx, b.st); err != nil {
		t.Fatal(err)
	}
	addTask(t, a.st, "second genuine")
	if _, err := e.client(a, "crew", nil).Once(ctx, a.st); err != nil {
		t.Fatal(err)
	}

	// 1. the hub alters a task's title in what it sends
	tamper := answerWith(func(real []string) string {
		return strings.Replace(strings.Join(real, ""), "second genuine", "forged title!", 1)
	})
	rep, err := e.client(b, "crew", tamper).Once(ctx, b.st)
	if err == nil {
		t.Fatalf("B accepted an op the hub altered (%+v)", rep)
	}
	if titles(t, b.st)["forged title!"] || titles(t, b.st)["second genuine"] {
		t.Fatal("part of the forged answer was applied")
	}

	// 2. the hub makes up an op in the name of a device that was never enrolled
	stranger := e.node("stranger")
	addTask(t, stranger.st, "from nobody")
	ops, _, err := stranger.st.OpsSince(ctx, sqlstore.Summary{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	invent := answerWith(func([]string) string { return frames(syncer.ToWire(ops)) })
	if rep, err := e.client(b, "crew", invent).Once(ctx, b.st); err == nil || rep.Pulled != 0 {
		t.Fatalf("B accepted an op from a device that is not a member: %+v, %v", rep, err)
	}
	if titles(t, b.st)["from nobody"] {
		t.Fatal("an unenrolled device's task reached B's view")
	}

	// the honest hub still works for B afterwards
	if _, err := e.client(b, "crew", nil).Once(ctx, b.st); err != nil {
		t.Fatal(err)
	}
	if !titles(t, b.st)["second genuine"] {
		t.Fatal("B did not recover after the refused answers")
	}
}

// gunzipBody returns a request's body whether or not it was compressed.
func gunzipBody(r *http.Request) ([]byte, error) {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		return io.ReadAll(zr)
	}
	return b, nil
}

// The hub refuses what it can judge: a tampered op, an unsigned op. Nothing of either is stored.
func TestTheHubRefusesAForgedOrUnsignedPush(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	a, _ := e.twoSites()
	addTask(t, a.st, "genuine")

	tamper := hookRT{func(next http.RoundTripper, r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, syncer.PathPush) {
			body, err := gunzipBody(r)
			if err != nil {
				return nil, err
			}
			body = bytes.Replace(body, []byte("genuine"), []byte("forged!"), 1)
			r = r.Clone(r.Context())
			r.Header.Del("Content-Encoding")
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
		}
		return next.RoundTrip(r)
	}}
	if _, err := e.client(a, "crew", tamper).Once(ctx, a.st); err == nil {
		t.Fatal("the hub accepted a push whose op had been altered in flight")
	}
	gs, _ := e.hub.Groups(ctx)
	if gs[0].Ops != 0 {
		t.Fatalf("the hub stored %d ops from a refused push", gs[0].Ops)
	}

	// an unsigned op, from a replica with no device key, pushed by the (genuine) founder's connection
	legacy, err := sqlstore.Open(ctx, filepath.Join(t.TempDir(), "legacy.db"), sqlstore.Options{NodeID: "legacy", Now: e.clk.now})
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	addTask(t, legacy, "unsigned")
	var re *syncer.RemoteError
	if _, err := e.client(a, "crew", nil).Once(ctx, legacy); err == nil || !errors.As(err, &re) {
		t.Fatalf("an unsigned op was not refused by the hub: %v", err)
	}
	gs, _ = e.hub.Groups(ctx)
	if gs[0].Ops != 0 {
		t.Fatalf("the hub stored %d ops, want none", gs[0].Ops)
	}
}

// A group has its own budget: its burst, then 429 with Retry-After; another group is not affected; it refills.
func TestEachGroupHasItsOwnRateLimit(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, func(o *Options) { o.RatePerSecond, o.RateBurst = 1, 3 })
	a, _ := e.twoSites()
	farmer := e.node("farmer")
	if err := e.hub.AddGroup(ctx, "farm", farmer.id.ID()); err != nil {
		t.Fatal(err)
	}
	var codes []int
	var retryAfter string
	for i := 0; i < 5; i++ {
		resp, err := e.client(a, "x", nil).HTTP.Get("https://" + e.addr + Prefix + "crew" + syncer.PathSummary)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			retryAfter = resp.Header.Get("Retry-After")
		}
		resp.Body.Close()
		codes = append(codes, resp.StatusCode)
	}
	// the three that passed the limit reach the sync handler (which says 400/401 for a request with no headers);
	// the rest are 429
	for i, c := range codes {
		if (i < 3) == (c == http.StatusTooManyRequests) {
			t.Fatalf("status codes %v: want three through, then 429", codes)
		}
	}
	if retryAfter != "1" {
		t.Fatalf("Retry-After = %q, want 1", retryAfter)
	}
	if e.hub.Limited("crew") != 2 || e.hub.Limited("farm") != 0 {
		t.Fatalf("limited counts: crew %d, farm %d", e.hub.Limited("crew"), e.hub.Limited("farm"))
	}
	if err := farmer.st.Found(ctx, "farm"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.client(farmer, "farm", nil).Once(ctx, farmer.st); err != nil {
		t.Fatalf("another group was affected by crew's limit: %v", err)
	}
	// the client sees the hint, and the budget comes back with time
	_, err := e.client(a, "crew", nil).Once(ctx, a.st)
	var re *syncer.RemoteError
	if !errors.As(err, &re) || re.Status != http.StatusTooManyRequests || re.RetryAfter != time.Second || !syncer.Transient(err) {
		t.Fatalf("client error = %v, want a transient 429 carrying Retry-After", err)
	}
	e.clk.advance(10 * time.Second)
	if _, err := e.client(a, "crew", nil).Once(ctx, a.st); err != nil {
		t.Fatalf("after the budget refilled: %v", err)
	}
}

// Bodies are bounded as sent, with a declared length and without one.
func TestBodiesOverTheLimitAreRefused(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, func(o *Options) { o.MaxBody = 2048 })
	a, _ := e.twoSites()
	hc := e.client(a, "x", nil).HTTP
	url := "https://" + e.addr + Prefix + "crew" + syncer.PathPush
	big := strings.Repeat("x", 10_000)

	req, _ := http.NewRequest("POST", url, strings.NewReader(big))
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("declared length: %d, want 413", resp.StatusCode)
	}

	pr, pw := io.Pipe() // no Content-Length: sent chunked
	go func() { pw.Write([]byte(big)); pw.Close() }()
	req, _ = http.NewRequest("POST", url, pr)
	req.ContentLength = -1
	resp, err = hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("a chunked body of 10,000 bytes got %d under a 2048-byte limit, want 413", resp.StatusCode)
	}
	gs, _ := e.hub.Groups(ctx)
	if gs[0].Ops != 0 {
		t.Fatalf("ops stored: %d", gs[0].Ops)
	}
}

// A device revoked by the group is cut off at the hub once the hub has the revocation.
func TestARevokedDeviceIsRefusedByTheHub(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	a, b := e.twoSites()
	addTask(t, a.st, "x")
	for _, n := range []node{a, b} {
		if _, err := e.client(a, "crew", nil).Once(ctx, a.st); err != nil {
			t.Fatal(err)
		}
		if _, err := e.client(n, "crew", nil).Once(ctx, n.st); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.st.Revoke(ctx, b.id.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.client(a, "crew", nil).Once(ctx, a.st); err != nil {
		t.Fatal(err)
	}
	e.clk.advance(2 * syncer.AuthorizerTTL)
	if _, err := e.client(b, "crew", nil).Once(ctx, b.st); err == nil {
		t.Fatal("a revoked device was still served")
	}
	if _, err := e.client(a, "crew", nil).Once(ctx, a.st); err != nil {
		t.Fatalf("the admin after revoking: %v", err)
	}
}

// Groups, ops and the founder rule survive a restart.
func TestTheHubRemembersItsGroupsAcrossARestart(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	a, _ := e.twoSites()
	addTask(t, a.st, "kept")
	if _, err := e.client(a, "crew", nil).Once(ctx, a.st); err != nil {
		t.Fatal(err)
	}
	pending := e.node("pending-founder")
	if err := e.hub.AddGroup(ctx, "later", pending.id.ID()); err != nil {
		t.Fatal(err)
	}
	e.stop()
	e.start()
	gs, err := e.hub.Groups(ctx)
	if err != nil || len(gs) != 2 || gs[0].ID != "crew" || gs[0].Ops < 3 || gs[1].Founded {
		t.Fatalf("groups after a restart: %+v, %v", gs, err)
	}
	if _, err := e.client(a, "crew", nil).Once(ctx, a.st); err != nil {
		t.Fatalf("the founder after a restart: %v", err)
	}
	intruder := e.node("intruder")
	if _, err := e.client(intruder, "later", nil).Once(ctx, intruder.st); err == nil {
		t.Fatal("after a restart, a stranger could reach a group that has no members yet")
	}
}

func TestGroupNamesFoundersAndLimitsAreChecked(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, func(o *Options) { o.MaxGroups = 2 })
	id := newID(t).ID()
	for _, bad := range []string{"", "Crew", "-crew", "a/b", "a b", "../x", strings.Repeat("a", 41)} {
		if err := e.hub.AddGroup(ctx, bad, id); err == nil {
			t.Errorf("group name %q was accepted", bad)
		}
	}
	for _, bad := range []string{"", "dev_short", "DEV_" + strings.Repeat("a", 32), "dev_" + strings.Repeat("g", 32)} {
		if err := e.hub.AddGroup(ctx, "ok", bad); err == nil {
			t.Errorf("founder %q was accepted", bad)
		}
	}
	if err := e.hub.AddGroup(ctx, "one", id); err != nil {
		t.Fatal(err)
	}
	if err := e.hub.AddGroup(ctx, "one", id); !errors.Is(err, ErrExists) {
		t.Fatalf("a duplicate: %v", err)
	}
	if err := e.hub.AddGroup(ctx, "two", id); err != nil {
		t.Fatal(err)
	}
	if err := e.hub.AddGroup(ctx, "three", id); err == nil {
		t.Fatal("the group limit was not enforced")
	}
}
