package syncer

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"offgrid/internal/identity"
	"offgrid/internal/store/sqlstore"
)

// tlsEnv is an admin node serving mutual TLS, with one enrolled member (b) and nothing else.
type tlsEnv struct {
	admin  *sqlstore.Store
	ida    *identity.Identity
	b      *sqlstore.Store
	idb    *identity.Identity
	clk    *fakeClock
	addr   string // host:port
	url    string
	hits   atomic.Int64 // requests that reached a handler
	conns  atomic.Int64 // connections the server accepted
	authz  Authorizer
	server *http.Server
}

func startSecure(t *testing.T, h http.Handler, cfg *tls.Config, to Timeouts, prep ...func(*http.Server)) (*http.Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewHTTPServer(h, cfg, to)
	for _, p := range prep {
		p(srv)
	}
	go srv.ServeTLS(ln, "", "")
	t.Cleanup(func() { srv.Close() })
	return srv, ln.Addr().String()
}

func newTLSEnv(t *testing.T) *tlsEnv {
	t.Helper()
	ctx := context.Background()
	clk := &fakeClock{t0}
	e := &tlsEnv{clk: clk}
	e.admin, e.ida = newSignedStore(t, "laptop", clk)
	e.b, e.idb = newSignedStore(t, "pi", clk)
	if err := e.admin.Found(ctx, "laptop"); err != nil {
		t.Fatal(err)
	}
	if err := e.admin.AddMember(ctx, e.idb.PublicKey(), sqlstore.RoleMember, "pi"); err != nil {
		t.Fatal(err)
	}
	e.authz = MembershipAuthorizer(e.admin, clk.now)
	cfg, err := ServerTLSConfig(e.ida, e.authz, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	inner := Handler(e.admin, ServerOptions{MutualTLS: true, Authorizer: e.authz, Now: clk.now})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { e.hits.Add(1); inner.ServeHTTP(w, r) })
	e.server, e.addr = startSecure(t, h, cfg, DefaultTimeouts, func(srv *http.Server) {
		srv.ConnState = func(_ net.Conn, s http.ConnState) {
			if s == http.StateNew {
				e.conns.Add(1)
			}
		}
	})
	e.url = "https://" + e.addr
	return e
}

// client is a syncing client for device id, trusting only the server device `server`.
func (e *tlsEnv) client(id *identity.Identity, server string, node string) *Client {
	cfg := ClientTLSConfig(id, PinDevice(server), e.clk.now)
	return &Client{Peer: e.url, Node: node, Mutual: true, HTTP: NewTLSClient(cfg), Now: e.clk.now}
}

func TestEnrolledDevicesSyncOverMutualTLS13WithNoSharedKey(t *testing.T) {
	ctx := context.Background()
	e := newTLSEnv(t)
	add(t, e.admin, "from-admin")
	cl := e.client(e.idb, e.ida.ID(), "pi")
	if _, err := cl.Once(ctx, e.b); err != nil {
		t.Fatal(err)
	}
	if hash(t, e.admin) != hash(t, e.b) {
		t.Fatal("the replicas did not converge")
	}
	// The protocol that was actually negotiated is TLS 1.3 over HTTP/2.
	var proto int
	var ver uint16
	req, _ := http.NewRequest("GET", e.url+PathSummary, nil)
	signSecure(req, e.clk.now())
	resp, err := cl.HTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	proto, ver = resp.ProtoMajor, resp.TLS.Version
	if ver != tls.VersionTLS13 || proto != 2 {
		t.Fatalf("version %x, HTTP/%d: want TLS 1.3 and HTTP/2", ver, proto)
	}
}

func TestOneConnectionServesManyRequests(t *testing.T) {
	ctx := context.Background()
	e := newTLSEnv(t)
	for i := 0; i < 30; i++ {
		add(t, e.admin, fmt.Sprintf("t%d", i))
	}
	cl := e.client(e.idb, e.ida.ID(), "pi")
	cl.Batch = 5 // several requests in one round
	rep, err := cl.Once(ctx, e.b)
	if err != nil || rep.Requests < 4 {
		t.Fatalf("%+v %v", rep, err)
	}
	if n := e.conns.Load(); n != 1 {
		t.Fatalf("%d connections for %d requests: the connection must be reused", n, rep.Requests)
	}
	// And a reused connection is reported as such.
	var reused bool
	req, _ := http.NewRequest("GET", e.url+PathSummary, nil)
	signSecure(req, e.clk.now())
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) { reused = i.Reused }}))
	resp, err := cl.HTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !reused {
		t.Fatal("not reused")
	}
}

func TestAnUnenrolledOrCertlessClientCannotFinishTheHandshake(t *testing.T) {
	e := newTLSEnv(t)
	stranger, _ := identity.Generate()
	cl := e.client(stranger, e.ida.ID(), "mallory")
	_, err := cl.PeerSummary(context.Background())
	if err == nil {
		t.Fatal("an unenrolled device got an answer")
	}
	if e.hits.Load() != 0 {
		t.Fatalf("a request from an unenrolled device reached a handler (%d)", e.hits.Load())
	}
	// No certificate at all.
	nocert := ClientTLSConfig(nil, PinDevice(e.ida.ID()), e.clk.now)
	c2 := &Client{Peer: e.url, Node: "x", Mutual: true, HTTP: NewTLSClient(nocert), Now: e.clk.now}
	if _, err := c2.PeerSummary(context.Background()); err == nil {
		t.Fatal("a client with no certificate got an answer")
	}
	if e.hits.Load() != 0 {
		t.Fatal("reached a handler")
	}
}

func TestOnlyTLS13IsSpoken(t *testing.T) {
	e := newTLSEnv(t)
	for _, v := range []uint16{tls.VersionTLS12, tls.VersionTLS11} {
		// An enrolled device with a perfectly good certificate, but speaking an old protocol.
		cfg := ClientTLSConfig(e.idb, PinDevice(e.ida.ID()), e.clk.now)
		cfg.MinVersion, cfg.MaxVersion = v, v
		conn, err := tls.Dial("tcp", e.addr, cfg)
		if err == nil {
			_, err = conn.Read(make([]byte, 1)) // in TLS 1.2 the client sees a failure on the first read at the latest
			conn.Close()
		}
		if err == nil {
			t.Fatalf("TLS %x was accepted", v)
		}
	}
	conn, err := tls.Dial("tcp", e.addr, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatalf("TLS 1.3 must be spoken: %v", err)
	}
	conn.Close()
}

func TestARevokedDeviceIsCutOffEvenOnAnOpenConnection(t *testing.T) {
	ctx := context.Background()
	e := newTLSEnv(t)
	cl := e.client(e.idb, e.ida.ID(), "pi")
	if _, err := cl.PeerSummary(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.admin.Revoke(ctx, e.idb.ID()); err != nil {
		t.Fatal(err)
	}
	e.clk.t = e.clk.t.Add(AuthorizerTTL + time.Second)
	_, err := cl.PeerSummary(ctx) // same pooled connection
	var re *RemoteError
	if err == nil || (!errors.As(err, &re) || re.Status != http.StatusForbidden) {
		t.Fatalf("a revoked device kept its open connection: %v", err)
	}
	// A new handshake fails outright.
	cl2 := e.client(e.idb, e.ida.ID(), "pi")
	if _, err := cl2.PeerSummary(ctx); err == nil {
		t.Fatal("a revoked device completed a handshake")
	}
}

func TestAClientRefusesAServerThatIsNotTheOneItPinned(t *testing.T) {
	e := newTLSEnv(t)
	other, _ := identity.Generate()
	cl := e.client(e.idb, other.ID(), "pi") // expects `other`, reaches the admin
	_, err := cl.PeerSummary(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not the device") {
		t.Fatalf("err = %v", err)
	}
	if e.hits.Load() != 0 {
		t.Fatal("the client sent a request to a server it did not trust")
	}
	// Pinning by the membership log: the server must be an enrolled, unrevoked member.
	cfg := ClientTLSConfig(e.idb, PinMembers(e.b, e.clk.now), e.clk.now)
	c2 := &Client{Peer: e.url, Node: "pi", Mutual: true, HTTP: NewTLSClient(cfg), Now: e.clk.now}
	pull(t, e.b, e.admin) // the Pi learns the membership
	if _, err := c2.PeerSummary(context.Background()); err != nil {
		t.Fatalf("a member server must be trusted by the log: %v", err)
	}
}

func pull(t *testing.T, to, from *sqlstore.Store) {
	t.Helper()
	ops, _, err := from.OpsSince(context.Background(), sqlstoreSummary(t, to), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := to.ApplyRemote(context.Background(), ops); err != nil {
		t.Fatal(err)
	}
}

func sqlstoreSummary(t *testing.T, s *sqlstore.Store) sqlstore.Summary {
	sum, err := s.Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

// ---- request IDs and replay ------------------------------------------------------------------------------------

func rawSecure(t *testing.T, e *tlsEnv, build func(*http.Request)) (int, string) {
	t.Helper()
	cfg := ClientTLSConfig(e.idb, PinDevice(e.ida.ID()), e.clk.now)
	hc := NewTLSClient(cfg)
	req, _ := http.NewRequest("GET", e.url+PathSummary, nil)
	req.Header.Set(HeaderProto, fmt.Sprint(ProtocolVersion))
	build(req)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var er ErrorResponse
	decodeJSON(resp.Body, &er)
	return resp.StatusCode, er.Code
}

func TestARepeatedRequestIDIsRefusedAndAMissingOrStaleOneToo(t *testing.T) {
	e := newTLSEnv(t)
	req := func(id string, at time.Time) func(*http.Request) {
		return func(r *http.Request) {
			if id != "" {
				r.Header.Set(HeaderRequest, id)
			}
			r.Header.Set(HeaderTime, fmt.Sprint(at.Unix()))
		}
	}
	id := "0123456789abcdef0123456789abcdef"
	if st, _ := rawSecure(t, e, req(id, e.clk.now())); st != http.StatusOK {
		t.Fatalf("first use: %d", st)
	}
	if st, code := rawSecure(t, e, req(id, e.clk.now())); st != http.StatusConflict || code != CodeReplay {
		t.Fatalf("a replayed request: %d %s, want 409 %s", st, code, CodeReplay)
	}
	if st, code := rawSecure(t, e, req("", e.clk.now())); st != http.StatusBadRequest || code != CodeBadRequest {
		t.Fatalf("no request ID: %d %s", st, code)
	}
	for _, bad := range []string{"short", "abcd", strings.Repeat("a", 64), strings.Repeat("z", 32)} {
		if st, _ := rawSecure(t, e, req(bad, e.clk.now())); st != http.StatusBadRequest {
			t.Fatalf("a malformed request ID %q: %d", bad, st)
		}
	}
	if st, code := rawSecure(t, e, req("ffffffffffffffffffffffffffffffff", e.clk.now().Add(-time.Hour))); st != http.StatusUnauthorized || code != CodeClockSkew {
		t.Fatalf("a stale request: %d %s", st, code)
	}
	// An ID may be reused once it is older than the window, because by then the timestamp check refuses it anyway.
	e.clk.t = e.clk.t.Add(DefaultWindow*2 + time.Second)
	if st, _ := rawSecure(t, e, req(id, e.clk.now())); st != http.StatusOK {
		t.Fatalf("after the window: %d", st)
	}
}

func TestTheReplayCacheIsBounded(t *testing.T) {
	c := newReplayCache(3, time.Minute)
	now := t0
	for i := 0; i < 3; i++ {
		if err := c.see(fmt.Sprintf("id%d", i), now); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.see("id0", now); !errors.Is(err, errReplay) {
		t.Fatalf("a repeat: %v", err)
	}
	if err := c.see("id9", now); !errors.Is(err, errReplayFull) {
		t.Fatalf("a full cache must refuse new IDs, not forget old ones: %v", err)
	}
	if err := c.see("id9", now.Add(2*time.Minute)); err != nil {
		t.Fatalf("after the window the old entries are gone: %v", err)
	}
}

// ---- timeouts ---------------------------------------------------------------------------------------------------

func TestTheServerHasTimeoutsAndDropsASlowClient(t *testing.T) {
	srv := NewHTTPServer(http.NotFoundHandler(), &tls.Config{}, DefaultTimeouts)
	if srv.ReadHeaderTimeout <= 0 || srv.ReadTimeout <= 0 || srv.WriteTimeout <= 0 || srv.IdleTimeout <= 0 || srv.MaxHeaderBytes <= 0 {
		t.Fatalf("a server with no timeout can be held open for ever: %+v", srv)
	}
	e := newTLSEnv(t)
	_, addr := startSecure(t, http.NotFoundHandler(), mustServerTLS(t, e), Timeouts{ReadHeader: 200 * time.Millisecond, Read: time.Second, Write: time.Second, Idle: time.Second})
	// A slowloris: a valid TLS client that starts a request and never finishes the headers.
	cfg := ClientTLSConfig(e.idb, PinDevice(e.ida.ID()), e.clk.now)
	cfg.NextProtos = []string{"http/1.1"}
	conn, err := tls.Dial("tcp", addr, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: x\r\nX-Slow: ")
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	_, err = bufio.NewReader(conn).ReadByte()
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("the server did not drop a slow client in time (err=%v after %s)", err, time.Since(start))
	}
}

func mustServerTLS(t *testing.T, e *tlsEnv) *tls.Config {
	cfg, err := ServerTLSConfig(e.ida, e.authz, e.clk.now)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// ---- enrollment listener ---------------------------------------------------------------------------------------

func TestTheEnrollmentListenerNeedsNoClientCertificateButTheClientPinsTheServer(t *testing.T) {
	ctx := context.Background()
	e := newEnrollEnv(t)
	cfg, err := EnrollServerTLSConfig(e.ida, e.clk.now)
	if err != nil {
		t.Fatal(err)
	}
	_, addr := startSecure(t, Handler(e.admin, ServerOptions{Enroller: e.admin, Now: e.clk.now, EnrollOnly: true}), cfg, DefaultTimeouts)
	url := "https://" + addr
	inv, link := e.invite(t, sqlstore.RoleMember)
	fp, _ := identity.Fingerprint(e.ida.PublicKey())
	idb := newKeyT(t)

	// The wrong pin: the client refuses before sending the invite.
	other := newKeyT(t)
	bad := NewTLSClient(ClientTLSConfig(nil, PinDevice(other.ID()), e.clk.now))
	if _, err := Enroll(ctx, bad, url, request(t, inv.Token, idb, "x")); err == nil || !strings.Contains(err.Error(), "not the device") {
		t.Fatalf("wrong device pin: %v", err)
	}
	badFP := NewTLSClient(ClientTLSConfig(nil, AllOf(PinDevice(e.ida.ID()), PinFingerprint(strings.Repeat("0", 64))), e.clk.now))
	if _, err := Enroll(ctx, badFP, url, request(t, inv.Token, idb, "x")); err == nil {
		t.Fatal("a wrong fingerprint was accepted")
	}
	// The right pins work, and the invite was not burned by the refused attempts.
	good := NewTLSClient(ClientTLSConfig(nil, AllOf(PinDevice(link.Admin), PinFingerprint(fp)), e.clk.now))
	resp, err := Enroll(ctx, good, url, request(t, inv.Token, idb, "x"))
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckEnrollment(resp, link, idb, "x"); err != nil {
		t.Fatal(err)
	}
	// This listener serves enrollment only.
	hc := NewTLSClient(ClientTLSConfig(nil, PinDevice(link.Admin), e.clk.now))
	r, err := hc.Get(url + PathSummary)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("the enrollment listener answered a sync request: %d", r.StatusCode)
	}
}

func TestTheSyncListenerDoesNotServeEnrollmentInMutualTLSMode(t *testing.T) {
	e := newTLSEnv(t)
	cl := e.client(e.idb, e.ida.ID(), "pi")
	req, _ := http.NewRequest("POST", e.url+PathEnroll, strings.NewReader("{}"))
	signSecure(req, e.clk.now())
	resp, err := cl.HTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func decodeJSON(r io.Reader, v any) { _ = json.NewDecoder(r).Decode(v) }
