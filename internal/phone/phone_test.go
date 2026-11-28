package phone

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"offgrid/internal/identity"
	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
)

var t0 = time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)

type clk struct{ t time.Time }

func (c *clk) now() time.Time          { return c.t }
func (c *clk) advance(d time.Duration) { c.t = c.t.Add(d) }

type rig struct {
	t       *testing.T
	st      *sqlstore.Store
	clk     *clk
	h       http.Handler
	hd      *handler
	secret  []byte
	founder *identity.Identity
}

const host = "192.168.1.20:8443"

func newRig(t *testing.T, mod func(*Config)) *rig {
	t.Helper()
	c := &clk{t0}
	f, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlstore.Open(context.Background(), filepath.Join(t.TempDir(), "n.db"), sqlstore.Options{NodeID: "site", Signer: f, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Found(context.Background(), "site"); err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, st: st, clk: c, secret: bytes.Repeat([]byte{7}, 32), founder: f}
	cfg := Config{Store: st, Secret: r.secret, Now: c.now}
	if mod != nil {
		mod(&cfg)
	}
	hd, h, err := build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.h, r.hd = h, hd
	return r
}

type reply struct {
	code int
	h    http.Header
	body string
}

func (r *rig) do(method, path string, body any, cookie string, hdr map[string]string) reply {
	r.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	req := httptest.NewRequest(method, "https://"+host+path, rd)
	req.Host = host
	req.RemoteAddr = "192.168.1.50:5555"
	if method == "POST" {
		req.Header.Set(ActionHeader, "1")
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: CookieName, Value: cookie})
	}
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	res := w.Result()
	b, _ := io.ReadAll(res.Body)
	return reply{res.StatusCode, res.Header, string(b)}
}

func (rp reply) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(rp.body), v); err != nil {
		t.Fatalf("not JSON: %q", rp.body)
	}
}

func (rp reply) cookie() string {
	for _, c := range rp.h.Values("Set-Cookie") {
		if strings.HasPrefix(c, CookieName+"=") {
			return strings.Split(strings.TrimPrefix(c, CookieName+"="), ";")[0]
		}
	}
	return ""
}

func (rp reply) errCode(t *testing.T) string {
	t.Helper()
	var e apiError
	rp.json(t, &e)
	return e.Code
}

type phoneDev struct {
	id    *identity.Identity
	label string
}

func (r *rig) invite(label string) string {
	r.t.Helper()
	inv, err := r.st.CreateInvite(context.Background(), sqlstore.RoleMember, label, 15*time.Minute)
	if err != nil {
		r.t.Fatal(err)
	}
	return inv.Token
}

func (r *rig) pairBody(token string, p *phoneDev) pairRequest {
	r.t.Helper()
	proof, err := sqlstore.EnrollProof(token, p.id, p.label)
	if err != nil {
		r.t.Fatal(err)
	}
	return pairRequest{Code: token, PublicKey: p.id.PublicKey(), Label: p.label, Proof: base64.RawURLEncoding.EncodeToString(proof)}
}

// pair enrolls a new phone and returns it with its session cookie.
func (r *rig) pair(label string) (*phoneDev, string) {
	r.t.Helper()
	id, _ := identity.Generate()
	p := &phoneDev{id: id, label: label}
	rp := r.do("POST", "/api/pair", r.pairBody(r.invite(label), p), "", nil)
	if rp.code != 200 {
		r.t.Fatalf("pair: %d %s", rp.code, rp.body)
	}
	return p, rp.cookie()
}

func (r *rig) mk(title string) task.Task {
	r.t.Helper()
	got, _, err := r.st.Create(context.Background(), task.NewTask{Title: title, Priority: task.PriorityMedium})
	if err != nil {
		r.t.Fatal(err)
	}
	return got
}

func (r *rig) notes(id string) []string {
	r.t.Helper()
	tk, err := r.st.Get(context.Background(), id)
	if err != nil {
		r.t.Fatal(err)
	}
	var out []string
	for _, n := range tk.Notes {
		out = append(out, n.Text)
	}
	return out
}

func (r *rig) sync(cookie string, cmds ...Command) []Result {
	r.t.Helper()
	rp := r.do("POST", "/api/sync", syncRequest{Commands: cmds}, cookie, nil)
	if rp.code != 200 {
		r.t.Fatalf("sync: %d %s", rp.code, rp.body)
	}
	var out syncReply
	rp.json(r.t, &out)
	if len(out.Results) != len(cmds) {
		r.t.Fatalf("%d results for %d commands", len(out.Results), len(cmds))
	}
	return out.Results
}

// ---------------------------------------------------------------- an unpaired phone gets nothing

func TestAnUnpairedPhoneGetsNothing(t *testing.T) {
	r := newRig(t, nil)
	secretTitle := "the-boiler-code-is-4411"
	r.mk(secretTitle)
	other := signer{key: bytes.Repeat([]byte{9}, 32)}
	expired := signer{key: r.secret}.issue("dev_x", t0.Add(-48*time.Hour), time.Hour)
	forged := other.issue("dev_x", t0, time.Hour)
	routes := []struct{ method, path string }{
		{"GET", "/api/state"}, {"POST", "/api/sync"}, {"POST", "/api/tasks/T1/photos"}, {"GET", "/api/tasks/T1/attachments/abc"},
	}
	for _, ck := range []string{"", "garbage", forged, expired} {
		for _, rt := range routes {
			rp := r.do(rt.method, rt.path, nil, ck, nil)
			if rp.code != 401 || rp.errCode(t) != "unpaired" {
				t.Errorf("%s %s with cookie %.12q: %d %s", rt.method, rt.path, ck, rp.code, rp.body)
			}
			if strings.Contains(rp.body, secretTitle) {
				t.Errorf("%s %s leaked a task", rt.method, rt.path)
			}
		}
	}
	// the app's own files are public and hold no task data
	for _, p := range []string{"/", "/app.js", "/sw.js", "/manifest.webmanifest", "/style.css"} {
		rp := r.do("GET", p, nil, "", nil)
		if rp.code != 200 || strings.Contains(rp.body, secretTitle) {
			t.Errorf("%s: %d", p, rp.code)
		}
	}
}

func TestAnEnrolledPhoneThatIsRevokedIsCutOffEvenWithAValidCookie(t *testing.T) {
	r := newRig(t, nil)
	p, ck := r.pair("Ana")
	if rp := r.do("GET", "/api/state", nil, ck, nil); rp.code != 200 {
		t.Fatalf("%d %s", rp.code, rp.body)
	}
	if _, err := r.st.Revoke(context.Background(), mustDev(t, p)); err != nil {
		t.Fatal(err)
	}
	r.clk.advance(memberTTL + time.Second)
	rp := r.do("GET", "/api/state", nil, ck, nil)
	if rp.code != 401 || !strings.Contains(rp.body, "no longer enrolled") {
		t.Fatalf("%d %s", rp.code, rp.body)
	}
}

func mustDev(t *testing.T, p *phoneDev) string {
	t.Helper()
	d, err := identity.DeviceID(p.id.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestASessionExpires(t *testing.T) {
	r := newRig(t, func(c *Config) { c.SessionTTL = time.Hour })
	_, ck := r.pair("Ana")
	r.clk.advance(59 * time.Minute)
	if rp := r.do("GET", "/api/state", nil, ck, nil); rp.code != 200 {
		t.Fatalf("before expiry %d", rp.code)
	}
	r.clk.advance(2 * time.Minute)
	if rp := r.do("GET", "/api/state", nil, ck, nil); rp.code != 401 {
		t.Fatalf("after expiry %d", rp.code)
	}
}

func TestASessionLongerThanTheMaximumIsRefusedAtStart(t *testing.T) {
	c := &clk{t0}
	st, _ := sqlstore.Open(context.Background(), filepath.Join(t.TempDir(), "n.db"), sqlstore.Options{NodeID: "x", Now: c.now})
	defer st.Close()
	if _, err := Handler(Config{Store: st, Secret: make([]byte, 32), SessionTTL: MaxSessionTTL + time.Hour}); err == nil {
		t.Fatal("accepted")
	}
	if _, err := Handler(Config{Store: st, Secret: make([]byte, 31)}); err == nil {
		t.Fatal("short secret accepted")
	}
	if _, err := Handler(Config{Secret: make([]byte, 32)}); err == nil {
		t.Fatal("no store accepted")
	}
}

// ---------------------------------------------------------------- pairing

func TestPairingEnrollsThePhoneAndSetsAHardenedCookie(t *testing.T) {
	r := newRig(t, nil)
	p, ck := r.pair("Ana's phone")
	if ck == "" {
		t.Fatal("no cookie")
	}
	var sc string
	for _, c := range r.do("POST", "/api/pair", r.pairBody(r.invite("x"), &phoneDev{id: mustID(t), label: "x"}), "", nil).h.Values("Set-Cookie") {
		sc = c
	}
	for _, want := range []string{CookieName + "=", "HttpOnly", "Secure", "SameSite=Strict", "Path=/", "Max-Age=2592000"} {
		if !strings.Contains(sc, want) {
			t.Errorf("cookie %q lacks %s", sc, want)
		}
	}
	if strings.Contains(sc, "Domain=") {
		t.Errorf("a __Host- cookie must not carry Domain: %q", sc)
	}
	rep, _ := r.st.Members(context.Background())
	found := false
	for _, m := range rep.Members {
		if m.Device == mustDev(t, p) && m.Label == "Ana's phone" && m.Role == sqlstore.RoleMember && !m.Revoked {
			found = true
		}
	}
	if !found {
		t.Fatalf("the phone is not a member: %+v", rep.Members)
	}
	var st StateReply
	rp := r.do("GET", "/api/state", nil, ck, nil)
	rp.json(t, &st)
	if st.Device != mustDev(t, p) || st.Label != "Ana's phone" || st.LateAfterMS != DefaultLateAfter.Milliseconds() {
		t.Fatalf("%+v", st)
	}
}

func mustID(t *testing.T) *identity.Identity {
	t.Helper()
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestPairingRefusals(t *testing.T) {
	r := newRig(t, nil)
	id := mustID(t)
	p := &phoneDev{id: id, label: "Ana"}
	tok := r.invite("Ana")
	good := r.pairBody(tok, p)

	bad := good
	bad.Proof = base64.RawURLEncoding.EncodeToString(make([]byte, 64)) // not a signature by this key
	if rp := r.do("POST", "/api/pair", bad, "", nil); rp.code != 400 || rp.errCode(t) != "bad_proof" {
		t.Errorf("bad proof: %d %s", rp.code, rp.body)
	}
	short := good
	short.Proof = "AAAA"
	if rp := r.do("POST", "/api/pair", short, "", nil); rp.code != 400 || rp.errCode(t) != "bad_proof" {
		t.Errorf("short proof: %d %s", rp.code, rp.body)
	}
	wrong := good
	wrong.Code = "AAAA-AAAA-AAAA-AAAA-AAAA"
	if rp := r.do("POST", "/api/pair", wrong, "", nil); rp.code != 403 || rp.errCode(t) != "invalid_code" {
		t.Errorf("wrong code: %d %s", rp.code, rp.body)
	}
	if rp := r.do("POST", "/api/pair", good, "", nil); rp.code != 200 {
		t.Fatalf("good: %d %s", rp.code, rp.body)
	}
	if rp := r.do("POST", "/api/pair", good, "", nil); rp.code != 403 || rp.errCode(t) != "used_code" {
		t.Errorf("reuse: %d %s", rp.code, rp.body)
	}
	// the same phone with a fresh code: already enrolled
	tok2 := r.invite("Ana")
	if rp := r.do("POST", "/api/pair", r.pairBody(tok2, p), "", nil); rp.code != 409 || rp.errCode(t) != "already_member" {
		t.Errorf("already: %d %s", rp.code, rp.body)
	}
	// expired
	tok3 := r.invite("Bob")
	r.clk.advance(16 * time.Minute)
	bob := &phoneDev{id: mustID(t), label: "Bob"}
	if rp := r.do("POST", "/api/pair", r.pairBody(tok3, bob), "", nil); rp.code != 403 || rp.errCode(t) != "expired_code" {
		t.Errorf("expired: %d %s", rp.code, rp.body)
	}
	if rp := r.do("POST", "/api/pair", []byte("{"), "", nil); rp.code != 400 {
		t.Errorf("malformed: %d", rp.code)
	}
	if rp := r.do("POST", "/api/pair", map[string]any{"code": "x", "extra": 1}, "", nil); rp.code != 400 {
		t.Errorf("unknown field: %d", rp.code)
	}
	if rp := r.do("POST", "/api/pair", bytes.Repeat([]byte("a"), 5000), "", nil); rp.code != 413 && rp.code != 400 {
		t.Errorf("oversized: %d", rp.code)
	}
}

func TestAnAdminInviteDoesNotMakeAnAdminPhone(t *testing.T) {
	r := newRig(t, nil)
	inv, err := r.st.CreateInvite(context.Background(), sqlstore.RoleAdmin, "sneaky", 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	p := &phoneDev{id: mustID(t), label: "sneaky"}
	rp := r.do("POST", "/api/pair", r.pairBody(inv.Token, p), "", nil)
	if rp.code != 403 || rp.errCode(t) != "not_a_member_invite" || rp.cookie() != "" {
		t.Fatalf("%d %s", rp.code, rp.body)
	}
	rep, _ := r.st.Members(context.Background())
	for _, m := range rep.Members {
		if m.Device == mustDev(t, p) && !m.Revoked {
			t.Fatalf("the phone stayed enrolled: %+v", m)
		}
	}
}

func TestGuessingThePairingCodeIsSlowedDown(t *testing.T) {
	r := newRig(t, nil)
	p := &phoneDev{id: mustID(t), label: "Ana"}
	tok := r.invite("Ana")
	good := r.pairBody(tok, p)
	wrong := good
	wrong.Code = "AAAA-AAAA-AAAA-AAAA-AAAA"
	for i := 0; i < 5; i++ {
		if rp := r.do("POST", "/api/pair", wrong, "", nil); rp.code != 403 {
			t.Fatalf("attempt %d: %d", i, rp.code)
		}
	}
	if rp := r.do("POST", "/api/pair", good, "", nil); rp.code != 429 || rp.errCode(t) != "slow_down" {
		t.Fatalf("the right code during a lockout: %d %s", rp.code, rp.body)
	}
	r.clk.advance(61 * time.Second)
	if rp := r.do("POST", "/api/pair", good, "", nil); rp.code != 200 {
		t.Fatalf("after the window: %d %s", rp.code, rp.body)
	}
}

func TestTheLimiterCountsTheWholeNodeToo(t *testing.T) {
	l := newLimiter()
	now := t0
	for i := 0; i < 20; i++ {
		l.fail("10.0.0."+string(rune('a'+i)), now) // 20 different addresses, one failure each
	}
	if l.allowed("10.0.0.z", now) {
		t.Fatal("a new address was allowed during a node-wide flood")
	}
	if !l.allowed("10.0.0.z", now.Add(61*time.Second)) {
		t.Fatal("still locked after the window")
	}
}

// ---------------------------------------------------------------- signing in again

func (r *rig) login(p *phoneDev, nonce string, sign bool) reply {
	var sig []byte
	if sign {
		sig, _ = p.id.Sign(LoginMessage(mustDev(r.t, p), nonce))
	} else {
		sig = make([]byte, 64)
	}
	return r.do("POST", "/api/login", loginRequest{Device: mustDev(r.t, p), Nonce: nonce, Sig: base64.RawURLEncoding.EncodeToString(sig)}, "", nil)
}

func (r *rig) nonce() string {
	var c challengeReply
	r.do("POST", "/api/challenge", nil, "", nil).json(r.t, &c)
	return c.Nonce
}

func TestAPairedPhoneSignsInAgainWithItsKeyAndNoCode(t *testing.T) {
	r := newRig(t, nil)
	p, _ := r.pair("Ana")
	rp := r.login(p, r.nonce(), true)
	if rp.code != 200 || rp.cookie() == "" {
		t.Fatalf("%d %s", rp.code, rp.body)
	}
	if st := r.do("GET", "/api/state", nil, rp.cookie(), nil); st.code != 200 {
		t.Fatalf("the new session does not work: %d", st.code)
	}
}

func TestSigningInRefusals(t *testing.T) {
	r := newRig(t, nil)
	p, _ := r.pair("Ana")
	stranger := &phoneDev{id: mustID(t), label: "x"}

	n := r.nonce()
	if rp := r.login(p, n, false); rp.code != 401 {
		t.Errorf("unsigned: %d", rp.code)
	}
	if rp := r.login(stranger, r.nonce(), true); rp.code != 401 {
		t.Errorf("a key that was never enrolled: %d", rp.code)
	}
	// someone else's signature under Ana's device ID
	n2 := r.nonce()
	sig, _ := stranger.id.Sign(LoginMessage(mustDev(t, p), n2))
	rp := r.do("POST", "/api/login", loginRequest{Device: mustDev(t, p), Nonce: n2, Sig: base64.RawURLEncoding.EncodeToString(sig)}, "", nil)
	if rp.code != 401 {
		t.Errorf("a stranger's signature: %d", rp.code)
	}
	if rp := r.login(p, "not-a-nonce", true); rp.code != 401 {
		t.Errorf("forged nonce: %d", rp.code)
	}
	// a signature over a nonce for a different device does not carry over
	n3 := r.nonce()
	sig3, _ := p.id.Sign(LoginMessage("dev_other", n3))
	if rp := r.do("POST", "/api/login", loginRequest{Device: mustDev(t, p), Nonce: n3, Sig: base64.RawURLEncoding.EncodeToString(sig3)}, "", nil); rp.code != 401 {
		t.Errorf("wrong message: %d", rp.code)
	}
}

func TestALoginCannotBeReplayedOrAnsweredLate(t *testing.T) {
	r := newRig(t, nil)
	p, _ := r.pair("Ana")
	n := r.nonce()
	if rp := r.login(p, n, true); rp.code != 200 {
		t.Fatalf("first: %d", rp.code)
	}
	if rp := r.login(p, n, true); rp.code != 401 || !strings.Contains(rp.body, "already used") {
		t.Fatalf("replay: %d %s", rp.code, rp.body)
	}
	old := r.nonce()
	r.clk.advance(ChallengeTTL + time.Second)
	if rp := r.login(p, old, true); rp.code != 401 || !strings.Contains(rp.body, "expired") {
		t.Fatalf("late: %d %s", rp.code, rp.body)
	}
}

func TestARevokedPhoneCannotSignInAgain(t *testing.T) {
	r := newRig(t, nil)
	p, _ := r.pair("Ana")
	if _, err := r.st.Revoke(context.Background(), mustDev(t, p)); err != nil {
		t.Fatal(err)
	}
	if rp := r.login(p, r.nonce(), true); rp.code != 401 {
		t.Fatalf("%d %s", rp.code, rp.body)
	}
}

func TestLogoutClearsTheCookie(t *testing.T) {
	r := newRig(t, nil)
	_, ck := r.pair("Ana")
	rp := r.do("POST", "/api/logout", nil, ck, nil)
	sc := strings.Join(rp.h.Values("Set-Cookie"), ";")
	if rp.code != 200 || !strings.Contains(sc, "Max-Age=0") && !strings.Contains(sc, "Max-Age=-1") && !strings.Contains(sc, "1970") {
		t.Fatalf("%d %q", rp.code, sc)
	}
	if !strings.Contains(sc, "Secure") || !strings.Contains(sc, "HttpOnly") {
		t.Fatalf("the clearing cookie must carry the same flags to replace it: %q", sc)
	}
}

// ---------------------------------------------------------------- the rules every request meets

func TestTheRulesAgainstOtherSitesAndOtherNames(t *testing.T) {
	r := newRig(t, nil)
	_, ck := r.pair("Ana")
	body := syncRequest{}
	cases := []struct {
		name string
		hdr  map[string]string
		host string
		want int
	}{
		{"no action header", map[string]string{ActionHeader: ""}, host, 403},
		{"cross-origin", map[string]string{"Origin": "https://evil.example"}, host, 403},
		{"same origin ok", map[string]string{"Origin": "https://" + host}, host, 200},
		{"cross-site fetch metadata", map[string]string{"Sec-Fetch-Site": "cross-site"}, host, 403},
		{"same-site fetch metadata is still not same-origin", map[string]string{"Sec-Fetch-Site": "same-site"}, host, 403},
		{"same-origin fetch metadata", map[string]string{"Sec-Fetch-Site": "same-origin"}, host, 200},
		{"rebound name", nil, "evil.example", 403},
		{"public ip", nil, "8.8.8.8:8443", 403},
		{"private ip", nil, "10.1.2.3", 200},
		{"mdns name", nil, "farm.local:8443", 200},
		{"offgrid name", nil, "node.offgrid", 200},
	}
	for _, c := range cases {
		req := httptest.NewRequest("POST", "https://x/api/sync", bytes.NewReader([]byte(`{"commands":[]}`)))
		req.Host = c.host
		req.Header.Set(ActionHeader, "1")
		req.AddCookie(&http.Cookie{Name: CookieName, Value: ck})
		for k, v := range c.hdr {
			if v == "" {
				req.Header.Del(k)
			} else {
				req.Header.Set(k, v)
			}
		}
		w := httptest.NewRecorder()
		r.h.ServeHTTP(w, req)
		if w.Code != c.want {
			t.Errorf("%s: %d %s", c.name, w.Code, w.Body.String())
		}
	}
	_ = body
	for _, m := range []string{"PUT", "DELETE", "PATCH", "OPTIONS"} {
		if rp := r.do(m, "/api/state", nil, ck, nil); rp.code != 405 {
			t.Errorf("%s: %d", m, rp.code)
		}
	}
}

func TestConfiguredHostsReplaceTheDefaults(t *testing.T) {
	r := newRig(t, func(c *Config) { c.Hosts = []string{"site.lan", "192.168.1.20"} })
	req := httptest.NewRequest("GET", "https://x/api/state", nil)
	req.Host = "10.9.9.9" // private, but not in the list
	if w := httptest.NewRecorder(); func() int { r.h.ServeHTTP(w, req); return w.Code }() != 403 {
		t.Error("a private address outside the configured list was served")
	}
	for host, want := range map[string]bool{"site.lan:8443": true, "SITE.LAN": true, "192.168.1.20:1": true, "192.168.1.21": false, "other.lan": false, "": false} {
		if got := hostOK([]string{"site.lan", "192.168.1.20"}, host); got != want {
			t.Errorf("%q: %v", host, got)
		}
	}
}

func TestEveryReplyCarriesTheStrictPolicy(t *testing.T) {
	r := newRig(t, nil)
	for _, p := range []string{"/", "/api/state", "/app.js", "/sw.js"} {
		rp := r.do("GET", p, nil, "", nil)
		for k, want := range map[string]string{
			"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer",
		} {
			if rp.h.Get(k) != want {
				t.Errorf("%s %s = %q", p, k, rp.h.Get(k))
			}
		}
		c := rp.h.Get("Content-Security-Policy")
		for _, must := range []string{"default-src 'none'", "script-src 'self'", "connect-src 'self'", "frame-ancestors 'none'", "base-uri 'none'", "form-action 'none'", "manifest-src 'self'", "worker-src 'self'"} {
			if !strings.Contains(c, must) {
				t.Errorf("%s CSP lacks %s: %s", p, must, c)
			}
		}
		if strings.Contains(c, "unsafe") || strings.Contains(c, "http:") {
			t.Errorf("%s CSP is loose: %s", p, c)
		}
		if !strings.Contains(rp.h.Get("Strict-Transport-Security"), "max-age") {
			t.Errorf("%s has no HSTS", p)
		}
	}
}

// ---------------------------------------------------------------- the app's own files

func TestTheAppFilesAreSelfContainedAndInstallable(t *testing.T) {
	r := newRig(t, nil)
	idx := r.do("GET", "/", nil, "", nil).body
	if strings.Contains(idx, "<script>") || strings.Contains(idx, "onclick=") || strings.Contains(idx, "style=") {
		t.Error("the page has inline script or style, which the policy would block")
	}
	for _, ref := range []string{`href="/manifest.webmanifest"`, `src="/app.js"`, `href="/style.css"`, `name="viewport"`} {
		if !strings.Contains(idx, ref) {
			t.Errorf("index lacks %s", ref)
		}
	}
	var m struct {
		Name, ShortName, StartURL, Scope, Display string
		Icons                                     []struct{ Src, Sizes, Type, Purpose string }
	}
	mf := r.do("GET", "/manifest.webmanifest", nil, "", nil).body
	var raw map[string]any
	if err := json.Unmarshal([]byte(mf), &raw); err != nil {
		t.Fatal(err)
	}
	if raw["start_url"] != "/" || raw["display"] != "standalone" || raw["name"] == "" || raw["short_name"] == "" {
		t.Fatalf("%v", raw)
	}
	_ = m
	sizes := map[string]bool{}
	for _, ic := range raw["icons"].([]any) {
		im := ic.(map[string]any)
		sizes[im["sizes"].(string)] = true
		if rp := r.do("GET", im["src"].(string), nil, "", nil); rp.code != 200 || len(rp.body) == 0 {
			t.Errorf("icon %v: %d", im["src"], rp.code)
		}
	}
	if !sizes["192x192"] || !sizes["512x512"] {
		t.Errorf("a phone needs a 192 and a 512 icon to install the app: %v", sizes)
	}
	sw := r.do("GET", "/sw.js", nil, "", nil)
	if sw.code != 200 || !strings.Contains(sw.h.Get("Content-Type"), "javascript") || !strings.Contains(sw.h.Get("Cache-Control"), "no-store") {
		t.Errorf("sw: %d %s %s", sw.code, sw.h.Get("Content-Type"), sw.h.Get("Cache-Control"))
	}
	if !strings.Contains(sw.body, "'/api/'") {
		t.Error("the service worker must stay out of /api/")
	}
	app := r.do("GET", "/app.js", nil, "", nil).body
	for _, bad := range []string{"innerHTML", "eval(", "document.write", "localStorage", "style:", "setAttribute('style'", ".style."} {
		if strings.Contains(app, bad) {
			t.Errorf("app.js uses %s", bad)
		}
	}
	for _, need := range []string{"extractable", "X-Offgrid-Action"} {
		if !strings.Contains(app, need) && !strings.Contains(app, strings.ToLower(need)) {
			t.Errorf("app.js lacks %s", need)
		}
	}
	if !strings.Contains(app, "generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, false,") {
		t.Error("the device key must be created non-extractable")
	}
}

// ---------------------------------------------------------------- reading

func TestTheStateIsTheTaskListAndTheSuggestions(t *testing.T) {
	r := newRig(t, nil)
	_, ck := r.pair("Ana")
	tk := r.mk("fix pump")
	var st StateReply
	r.do("GET", "/api/state", nil, ck, nil).json(t, &st)
	if st.Node != "site" || len(st.Tasks) != 1 || st.Tasks[0].ID != tk.ID || st.TasksTotal != 1 || st.Suggestions == nil {
		t.Fatalf("%+v", st)
	}
	if !st.Now.Equal(t0) {
		t.Fatalf("now %v", st.Now)
	}
}

// ---------------------------------------------------------------- replaying the queue

func cmd(id, op string) Command { return Command{ID: id + "-0000000", Op: op} }

func TestAPromptCommandIsAppliedWithoutAMarker(t *testing.T) {
	r := newRig(t, nil)
	_, ck := r.pair("Ana")
	tk := r.mk("fix pump")
	c := cmd("a1", "status")
	c.Task, c.Status, c.AgeMS = tk.ID, "done", 500
	res := r.sync(ck, c)[0]
	if res.Outcome != "applied" || res.Late || res.Task != tk.ID {
		t.Fatalf("%+v", res)
	}
	got, _ := r.st.Get(context.Background(), tk.ID)
	if got.Status != task.StatusDone || len(got.Notes) != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestALateCommandIsAppliedWhenItArrivesAndMarkedLate(t *testing.T) {
	r := newRig(t, nil)
	_, ck := r.pair("Ana")
	tk := r.mk("fix pump")
	r.clk.advance(3 * time.Hour) // the arrival time is what the new op carries
	c := cmd("a2", "status")
	c.Task, c.Status, c.AgeMS = tk.ID, "done", (2*time.Hour + 5*time.Minute).Milliseconds()
	res := r.sync(ck, c)[0]
	if res.Outcome != "applied" || !res.Late {
		t.Fatalf("%+v", res)
	}
	n := r.notes(tk.ID)
	if len(n) != 1 || !strings.Contains(n[0], `from phone "Ana"`) || !strings.Contains(n[0], "late, made 2h05m before it reached this node") || !strings.Contains(n[0], "[c:a2-0000000]") {
		t.Fatalf("%q", n)
	}
	ops, _ := r.st.LastOps(context.Background(), 5)
	last := ops[len(ops)-1]
	for _, o := range ops {
		if o.TS.Before(r.clk.t.Add(-time.Minute)) && strings.Contains(o.Describe(), "done") {
			t.Fatalf("the status op was stamped with the phone's time, not the arrival: %v", o.TS)
		}
	}
	if last.TS.Before(r.clk.t) {
		t.Fatalf("op stamped %v, arrival %v", last.TS, r.clk.t)
	}
}

func TestTheLateThresholdIsExact(t *testing.T) {
	r := newRig(t, nil)
	_, ck := r.pair("Ana")
	tk := r.mk("fix pump")
	a, b := cmd("edge1", "note"), cmd("edge2", "note")
	a.Task, a.Text, a.AgeMS = tk.ID, "just under", (DefaultLateAfter - time.Millisecond).Milliseconds()
	b.Task, b.Text, b.AgeMS = tk.ID, "exactly", DefaultLateAfter.Milliseconds()
	res := r.sync(ck, a, b)
	if res[0].Late || !res[1].Late {
		t.Fatalf("%+v", res)
	}
	n := r.notes(tk.ID)
	if n[0] != "just under" || !strings.HasPrefix(n[1], "exactly — from phone") {
		t.Fatalf("%q", n)
	}
}

func TestANegativeOrHugeAgeCannotBreakTheMarker(t *testing.T) {
	r := newRig(t, nil)
	_, ck := r.pair("Ana")
	tk := r.mk("fix pump")
	a, b := cmd("neg", "note"), cmd("huge", "note")
	a.Task, a.Text, a.AgeMS = tk.ID, "negative", -5000
	b.Task, b.Text, b.AgeMS = tk.ID, "huge", 1<<62
	res := r.sync(ck, a, b)
	if res[0].Late || !res[1].Late {
		t.Fatalf("%+v", res)
	}
	if n := r.notes(tk.ID); !strings.Contains(n[1], "90d00h") {
		t.Fatalf("an absurd age is shown as the longest a session can last: %q", n)
	}
}

func TestTheSameCommandTwiceChangesNothingTheSecondTime(t *testing.T) {
	r := newRig(t, nil)
	_, ck := r.pair("Ana")
	tk := r.mk("fix pump")
	c := cmd("note1", "note")
	c.Task, c.Text = tk.ID, "hello"
	if r.sync(ck, c)[0].Outcome != "applied" {
		t.Fatal("first")
	}
	res := r.sync(ck, c)[0]
	if res.Outcome != "duplicate" || res.Task != tk.ID {
		t.Fatalf("%+v", res)
	}
	if n := r.notes(tk.ID); len(n) != 1 {
		t.Fatalf("%q", n)
	}
}

func TestALateCommandIsRecognisedAgainAfterARestart(t *testing.T) {
	r := newRig(t, nil)
	_, ck := r.pair("Ana")
	tk := r.mk("fix pump")
	c := cmd("late1", "note")
	c.Task, c.Text, c.AgeMS = tk.ID, "from the field", time.Hour.Milliseconds()
	r.sync(ck, c)
	h2, err := Handler(Config{Store: r.st, Secret: r.secret, Now: r.clk.now}) // a new process: no memory of what was applied
	if err != nil {
		t.Fatal(err)
	}
	r.h = h2
	res := r.sync(ck, c)[0]
	if res.Outcome != "duplicate" || res.Task != tk.ID {
		t.Fatalf("%+v", res)
	}
	if n := r.notes(tk.ID); len(n) != 1 {
		t.Fatalf("applied twice: %q", n)
	}
}

func TestCreateThenEditInOneBatchUsesTheNewTaskByReference(t *testing.T) {
	r := newRig(t, nil)
	_, ck := r.pair("Ana")
	cr := cmd("mk1", "create")
	cr.Title, cr.Site, cr.Priority = "replace filter", "farm", "high"
	st := cmd("mk2", "status")
	st.Task, st.Status = "@"+cr.ID, "in_progress"
	nt := cmd("mk3", "note")
	nt.Task, nt.Text = "@"+cr.ID, "bought one"
	res := r.sync(ck, cr, st, nt)
	for _, x := range res {
		if x.Outcome != "applied" {
			t.Fatalf("%+v", res)
		}
	}
	tks, _ := r.st.List(context.Background(), task.Filter{})
	if len(tks) != 1 || tks[0].Title != "replace filter" || tks[0].Site != "farm" || tks[0].Priority != task.PriorityHigh || tks[0].Status != task.StatusInProgress || len(tks[0].Notes) != 1 {
		t.Fatalf("%+v", tks)
	}
	if res[0].Task != tks[0].ID {
		t.Fatalf("the phone needs the real ID back: %+v", res[0])
	}
}

func TestACreateThatMatchesAnUnfinishedTaskIsADuplicate(t *testing.T) {
	r := newRig(t, nil)
	_, ck := r.pair("Ana")
	tk := r.mk("fix pump")
	c := cmd("dup1", "create")
	c.Title = "fix pump"
	res := r.sync(ck, c)[0]
	if res.Outcome != "duplicate" || res.Task != tk.ID {
		t.Fatalf("%+v", res)
	}
}

func TestAReplacedChangeIsFlaggedAndKept(t *testing.T) {
	r := newRig(t, nil)
	_, ck := r.pair("Ana")
	tk := r.mk("fix pump")
	// While the phone was away someone made it high priority.
	hi := task.PriorityHigh
	if _, _, err := r.st.Update(context.Background(), tk.ID, task.Changes{Priority: &hi}); err != nil {
		t.Fatal(err)
	}
	c := cmd("ow1", "priority")
	c.Task, c.Priority, c.Was = tk.ID, "low", "medium" // the phone had seen medium
	res := r.sync(ck, c)[0]
	if res.Outcome != "applied" || res.Overwrote == "" || !strings.Contains(res.Overwrote, "was high") || !strings.Contains(res.Overwrote, "had seen medium") {
		t.Fatalf("%+v", res)
	}
	got, _ := r.st.Get(context.Background(), tk.ID)
	if got.Priority != task.PriorityLow {
		t.Fatalf("last command to arrive wins: %v", got.Priority)
	}
	if n := r.notes(tk.ID); len(n) != 1 || !strings.Contains(n[0], "it replaced a newer change") || strings.Contains(n[0], "late,") {
		t.Fatalf("%q", n)
	}
	// no conflict when the phone saw the current value
	c2 := cmd("ow2", "priority")
	c2.Task, c2.Priority, c2.Was = tk.ID, "high", "low"
	if res := r.sync(ck, c2)[0]; res.Overwrote != "" {
		t.Fatalf("%+v", res)
	}
}

func TestACommandThatChangesNothingIsADuplicate(t *testing.T) {
	r := newRig(t, nil)
	_, ck := r.pair("Ana")
	tk := r.mk("fix pump")
	c := cmd("nc1", "status")
	c.Task, c.Status = tk.ID, "open"
	if res := r.sync(ck, c)[0]; res.Outcome != "duplicate" {
		t.Fatalf("%+v", res)
	}
}

func TestSuggestionsCanBeAcceptedOrDismissedFromTheQueue(t *testing.T) {
	r := newRig(t, nil)
	_, ck := r.pair("Ana")
	tk := r.mk("fix pump")
	hi := task.PriorityHigh
	sg1, _, err := r.st.Suggest(context.Background(), task.Draft{Task: tk.ID, Priority: &hi, By: "model", Note: "leaking", Key: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	tk2 := r.mk("paint fence")
	sg2, _, _ := r.st.Suggest(context.Background(), task.Draft{Task: tk2.ID, Priority: &hi, By: "model", Key: "k2"})
	a, d := cmd("ac1", "accept"), cmd("ds1", "dismiss")
	a.Task, d.Task = sg1.ID, sg2.ID
	res := r.sync(ck, a, d)
	if res[0].Outcome != "applied" || res[1].Outcome != "applied" {
		t.Fatalf("%+v", res)
	}
	g1, _ := r.st.Get(context.Background(), tk.ID)
	g2, _ := r.st.Get(context.Background(), tk2.ID)
	if g1.Priority != task.PriorityHigh || g2.Priority != task.PriorityMedium {
		t.Fatalf("%v %v", g1.Priority, g2.Priority)
	}
	// resolving again: refused, not applied twice
	a2 := cmd("ac2", "accept")
	a2.Task = sg1.ID
	if r := r.sync(ck, a2)[0]; r.Outcome != "rejected" || r.Code != "already_resolved" {
		t.Fatalf("%+v", r)
	}
}

func TestAStaleSuggestionIsRefusedNotForced(t *testing.T) {
	r := newRig(t, nil)
	_, ck := r.pair("Ana")
	tk := r.mk("fix pump")
	hi := task.PriorityHigh
	sg, _, _ := r.st.Suggest(context.Background(), task.Draft{Task: tk.ID, Priority: &hi, By: "model", Key: "k3"})
	lo := task.PriorityLow
	r.clk.advance(time.Minute)
	if _, _, err := r.st.Update(context.Background(), tk.ID, task.Changes{Priority: &lo}); err != nil {
		t.Fatal(err)
	}
	a := cmd("st1", "accept")
	a.Task = sg.ID
	if res := r.sync(ck, a)[0]; res.Outcome != "rejected" || res.Code != "stale" {
		t.Fatalf("%+v", res)
	}
	if g, _ := r.st.Get(context.Background(), tk.ID); g.Priority != task.PriorityLow {
		t.Fatalf("the person's edit was undone: %v", g.Priority)
	}
}

func TestBadCommandsAreRejectedOneByOneAndTheRestStillRun(t *testing.T) {
	r := newRig(t, nil)
	_, ck := r.pair("Ana")
	tk := r.mk("fix pump")
	ok := cmd("good1", "note")
	ok.Task, ok.Text = tk.ID, "fine"
	cases := []struct {
		name string
		c    Command
		code string
	}{
		{"short id", Command{ID: "x", Op: "note"}, "bad_id"},
		{"bad id chars", Command{ID: "has space 123", Op: "note"}, "bad_id"},
		{"unknown op", Command{ID: "unknown1", Op: "delete", Task: tk.ID}, "unknown_op"},
		{"gone task", Command{ID: "gone0001", Op: "status", Task: "nope", Status: "done"}, "not_found"},
		{"bad status", Command{ID: "badst001", Op: "status", Task: tk.ID, Status: "finished"}, "invalid"},
		{"bad priority", Command{ID: "badpr001", Op: "priority", Task: tk.ID, Priority: "urgent"}, "invalid"},
		{"bad create priority", Command{ID: "badcp001", Op: "create", Title: "x", Priority: "urgent"}, "invalid"},
		{"empty title", Command{ID: "empty001", Op: "create", Title: "  "}, "invalid"},
		{"empty note", Command{ID: "emptyn01", Op: "note", Task: tk.ID, Text: " "}, "invalid"},
		{"no task", Command{ID: "notask01", Op: "note", Text: "x"}, "bad_reference"},
		{"dangling ref", Command{ID: "dangl001", Op: "note", Task: "@nothing01", Text: "x"}, "bad_reference"},
		{"no suggestion", Command{ID: "nosug001", Op: "accept"}, "bad_reference"},
		{"unknown suggestion", Command{ID: "unsug001", Op: "accept", Task: "zzz"}, "not_found"},
		{"huge text", Command{ID: "huge0001", Op: "note", Task: tk.ID, Text: strings.Repeat("a", maxText+1)}, "invalid"},
	}
	cmds := []Command{ok}
	for _, c := range cases {
		cmds = append(cmds, c.c)
	}
	last := cmd("good2", "note")
	last.Task, last.Text = tk.ID, "also fine"
	cmds = append(cmds, last)
	res := r.sync(ck, cmds...)
	if res[0].Outcome != "applied" || res[len(res)-1].Outcome != "applied" {
		t.Fatalf("%+v", res)
	}
	for i, c := range cases {
		if got := res[i+1]; got.Outcome != "rejected" || got.Code != c.code {
			t.Errorf("%s: %+v", c.name, got)
		}
	}
	if n := r.notes(tk.ID); len(n) != 2 {
		t.Fatalf("%q", n)
	}
}

func TestTooManyCommandsOrTooLargeABodyIsRefused(t *testing.T) {
	r := newRig(t, nil)
	_, ck := r.pair("Ana")
	var cs []Command
	for i := 0; i <= maxCommands; i++ {
		cs = append(cs, Command{ID: "bulk" + strings.Repeat("0", 4) + string(rune('a'+i%26)) + string(rune('a'+i/26)), Op: "note"})
	}
	if rp := r.do("POST", "/api/sync", syncRequest{Commands: cs}, ck, nil); rp.code != 413 || rp.errCode(t) != "too_many" {
		t.Fatalf("%d %s", rp.code, rp.body)
	}
	if rp := r.do("POST", "/api/sync", bytes.Repeat([]byte(" "), maxSyncBody+10), ck, nil); rp.code != 413 && rp.code != 400 {
		t.Fatalf("%d", rp.code)
	}
	if rp := r.do("POST", "/api/sync", []byte(`{"commands":[]} {"x":1}`), ck, nil); rp.code != 400 {
		t.Fatalf("two values: %d", rp.code)
	}
	exactly := make([]Command, maxCommands)
	for i := range exactly {
		exactly[i] = Command{ID: "okay" + strings.Repeat("0", 4) + string(rune('a'+i%26)) + string(rune('a'+i/26)), Op: "note"}
	}
	if rp := r.do("POST", "/api/sync", syncRequest{Commands: exactly}, ck, nil); rp.code != 200 {
		t.Fatalf("exactly the limit: %d %s", rp.code, rp.body)
	}
}

func TestTheMemoryOfAppliedCommandsIsBounded(t *testing.T) {
	r := newRig(t, nil)
	h := r.hd
	for i := 0; i < rememberLimit+10; i++ {
		h.remember("id"+strings.Repeat("x", 8)+string(rune(i%90+33))+string(rune(i/90+33)), "T")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.seen) > rememberLimit || len(h.seenList) > rememberLimit {
		t.Fatalf("%d %d", len(h.seen), len(h.seenList))
	}
}

// ---------------------------------------------------------------- photos

func jpegData(n int, seed byte) []byte {
	b := bytes.Repeat([]byte{seed}, n)
	copy(b, []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F', 0})
	return b
}

func photoHdr(extra map[string]string) map[string]string {
	h := map[string]string{"Content-Type": "image/jpeg", "X-Offgrid-Name": "leak%20one.jpg"}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

func TestAPhotoIsStoredOnTheTaskAndServedBackOnlyToAPairedPhone(t *testing.T) {
	r := newRig(t, nil)
	_, ck := r.pair("Ana")
	tk := r.mk("fix pump")
	data := jpegData(4000, 3)
	rp := r.do("POST", "/api/tasks/"+tk.ID+"/photos", data, ck, photoHdr(nil))
	if rp.code != 201 {
		t.Fatalf("%d %s", rp.code, rp.body)
	}
	var out struct {
		Outcome, Hash string
		Late          bool
	}
	rp.json(t, &out)
	if out.Outcome != "applied" || out.Hash == "" || out.Late {
		t.Fatalf("%+v", out)
	}
	atts, _ := r.st.Attachments(context.Background(), tk.ID)
	if len(atts) != 1 || atts[0].Name != "leak one.jpg" || atts[0].Size != 4000 || atts[0].Type != "image/jpeg" {
		t.Fatalf("%+v", atts)
	}
	if n := r.notes(tk.ID); len(n) != 0 {
		t.Fatalf("a prompt photo needs no marker: %q", n)
	}
	path := "/api/tasks/" + tk.ID + "/attachments/" + out.Hash
	got := r.do("GET", path, nil, ck, nil)
	if got.code != 200 || got.body != string(data) || !strings.Contains(got.h.Get("Content-Security-Policy"), "sandbox") || got.h.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("%d %q", got.code, got.h)
	}
	if r.do("GET", path, nil, "", nil).code != 401 {
		t.Fatal("an unpaired phone read a photo")
	}
}

func TestAPhotoUploadedAgainByHashIsADuplicateAndAddsNothing(t *testing.T) {
	r := newRig(t, nil)
	_, ck := r.pair("Ana")
	tk := r.mk("fix pump")
	data := jpegData(2500, 4)
	var first struct{ Hash string }
	rp := r.do("POST", "/api/tasks/"+tk.ID+"/photos", data, ck, photoHdr(nil))
	rp.json(t, &first)
	sum := strings.TrimPrefix(first.Hash, "sha256:")
	rp = r.do("POST", "/api/tasks/"+tk.ID+"/photos", data, ck, photoHdr(map[string]string{"X-Offgrid-Sha256": strings.ToUpper(sum)}))
	if rp.code != 200 || !strings.Contains(rp.body, "duplicate") {
		t.Fatalf("%d %s", rp.code, rp.body)
	}
	if atts, _ := r.st.Attachments(context.Background(), tk.ID); len(atts) != 1 {
		t.Fatalf("%d attachments", len(atts))
	}
}

func TestALatePhotoIsMarkedLate(t *testing.T) {
	r := newRig(t, nil)
	_, ck := r.pair("Ana")
	tk := r.mk("fix pump")
	rp := r.do("POST", "/api/tasks/"+tk.ID+"/photos", jpegData(2000, 5), ck, photoHdr(map[string]string{"X-Offgrid-Age-Ms": "3600000"}))
	if rp.code != 201 || !strings.Contains(rp.body, `"late":true`) {
		t.Fatalf("%d %s", rp.code, rp.body)
	}
	if n := r.notes(tk.ID); len(n) != 1 || !strings.Contains(n[0], `from phone "Ana" (photo): late, made 1h00m`) {
		t.Fatalf("%q", n)
	}
}

func TestPhotoRefusals(t *testing.T) {
	r := newRig(t, func(c *Config) { c.MaxPhoto = 5000 })
	_, ck := r.pair("Ana")
	tk := r.mk("fix pump")
	up := func(id string, body []byte, hdr map[string]string) reply {
		return r.do("POST", "/api/tasks/"+id+"/photos", body, ck, hdr)
	}
	for name, c := range map[string]struct {
		id   string
		body []byte
		hdr  map[string]string
		code int
	}{
		"not a picture type":   {tk.ID, jpegData(100, 1), photoHdr(map[string]string{"Content-Type": "application/pdf"}), 415},
		"svg":                  {tk.ID, []byte("<svg/>"), photoHdr(map[string]string{"Content-Type": "image/svg+xml"}), 415},
		"no type":              {tk.ID, jpegData(100, 1), photoHdr(map[string]string{"Content-Type": ""}), 415},
		"bytes are not a jpeg": {tk.ID, []byte("hello, I am not a picture at all, really"), photoHdr(nil), 415},
		"empty":                {tk.ID, nil, photoHdr(nil), 400},
		"too large":            {tk.ID, jpegData(6000, 1), photoHdr(nil), 413},
		"no such task":         {"nope", jpegData(100, 1), photoHdr(nil), 404},
	} {
		if rp := up(c.id, c.body, c.hdr); rp.code != c.code {
			t.Errorf("%s: %d %s", name, rp.code, rp.body)
		}
	}
	if rp := up(tk.ID, jpegData(5000, 1), photoHdr(nil)); rp.code != 201 {
		t.Errorf("exactly the limit: %d %s", rp.code, rp.body)
	}
	if atts, _ := r.st.Attachments(context.Background(), tk.ID); len(atts) != 1 {
		t.Errorf("refused photos left %d attachments", len(atts))
	}
}

func TestHumanDurations(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * time.Second: "under a minute", time.Minute: "1m", 59 * time.Minute: "59m", time.Hour: "1h00m",
		25*time.Hour + 5*time.Minute: "1d01h", 90 * 24 * time.Hour: "90d00h",
	} {
		if got := human(d); got != want {
			t.Errorf("%v: %q want %q", d, got, want)
		}
	}
}

func TestARevokedPhoneCannotSignInEvenWithinTheMembershipCacheWindow(t *testing.T) {
	r := newRig(t, nil)
	p, ck := r.pair("Ana")
	if rp := r.do("GET", "/api/state", nil, ck, nil); rp.code != 200 { // warms the membership cache
		t.Fatal(rp.code)
	}
	if _, err := r.st.Revoke(context.Background(), mustDev(t, p)); err != nil {
		t.Fatal(err)
	}
	if rp := r.login(p, r.nonce(), true); rp.code != 401 {
		t.Fatalf("a revoked phone signed in on a stale cache: %d", rp.code)
	}
}

func TestALongPhotoNameIsShortened(t *testing.T) {
	r := newRig(t, nil)
	_, ck := r.pair("Ana")
	tk := r.mk("fix pump")
	long := strings.Repeat("é", 150) + ".jpg"
	rp := r.do("POST", "/api/tasks/"+tk.ID+"/photos", jpegData(2000, 6), ck, photoHdr(map[string]string{"X-Offgrid-Name": long}))
	if rp.code != 201 {
		t.Fatalf("%d %s", rp.code, rp.body)
	}
	atts, _ := r.st.Attachments(context.Background(), tk.ID)
	if n := len([]rune(atts[0].Name)); n != 100 {
		t.Fatalf("name has %d characters", n)
	}
}

func TestAReferenceToACommandThatFailedIsABadReference(t *testing.T) {
	r := newRig(t, nil)
	_, ck := r.pair("Ana")
	bad := cmd("mkbad", "create")
	bad.Title, bad.Priority = "x", "urgent"
	nt := cmd("mknote", "note")
	nt.Task, nt.Text = "@"+bad.ID, "hello"
	res := r.sync(ck, bad, nt)
	if res[0].Outcome != "rejected" || res[1].Outcome != "rejected" || res[1].Code != "bad_reference" {
		t.Fatalf("%+v", res)
	}
}
