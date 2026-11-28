// Package phone is the phone client's server: a small web app (a PWA) that a site node serves to the phones
// enrolled in it, so a person can see the task list, add and update tasks, attach photos and accept or
// dismiss the AI's suggestions from a phone, with or without a connection to the node.
//
// It is NOT a replica. A phone keeps a copy of what it last read and a queue of what the person did since;
// when it reaches the node again the queue is sent as COMMANDS ("set T3 done", "add this note") and the node
// applies each one as a new, signed op stamped with the time it ARRIVED, and marks the ones that waited as
// "late" in a note on the task. Weaker than a replica (there is no merge: the last command to arrive wins),
// and enough inside one site's network. See ADR 018.
//
// It is a separate server from the localhost dashboard (package web). The dashboard has no login and answers
// only to this machine; this one answers to a network, so everything but the app's own files needs a session.
package phone

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"offgrid/internal/blobstore"
	"offgrid/internal/identity"
	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
	"offgrid/internal/web"
)

//go:embed static
var staticFiles embed.FS

// ActionHeader must be on every POST, exactly as on the dashboard: a page on another site cannot add a
// custom header without a CORS preflight, and this server never answers one with permission.
const ActionHeader = web.ActionHeader

// CookieName carries the session. The __Host- prefix makes a browser refuse it unless it is Secure, set
// for this exact host and Path=/, so a sibling name or a plain-HTTP page cannot plant one.
const CookieName = "__Host-offgrid"

// Store is what the phone server needs from a node's store. *sqlstore.Store satisfies it.
type Store interface {
	web.Store
	Members(ctx context.Context) (sqlstore.MembershipReport, error)
	RedeemInvite(ctx context.Context, token, publicKey, label string, proof []byte) (sqlstore.Enrollment, error)
	Revoke(ctx context.Context, device string) (sqlstore.Revocation, error)
	Get(ctx context.Context, id string) (task.Task, error)
	Create(ctx context.Context, n task.NewTask) (task.Task, bool, error)
	Update(ctx context.Context, id string, c task.Changes) (task.Task, bool, error)
	AddNote(ctx context.Context, id, text string) (task.Task, error)
	AddAttachment(ctx context.Context, taskID string, r io.Reader, declared int64, mediaType, name string) (sqlstore.Attachment, error)
	Attachments(ctx context.Context, taskID string) ([]sqlstore.Attachment, error)
}

// Config is everything the phone server reads.
type Config struct {
	Store Store
	// Secret signs sessions (at least MinSecret bytes). Changing it logs every phone out.
	Secret []byte
	// Hosts are the names and addresses a phone may use to reach this node. Empty: any private address
	// and the names .local, .lan and .offgrid (what the Day 33 certificate authority will sign for).
	Hosts      []string
	Now        func() time.Time
	SessionTTL time.Duration // default 30 days, at most MaxSessionTTL
	LateAfter  time.Duration // a command this much older than its arrival is "late" (default 2 minutes)
	MaxPhoto   int64         // largest photo upload (default 16 MiB; the blob store has its own cap)
}

const (
	DefaultSessionTTL = 30 * 24 * time.Hour
	MaxSessionTTL     = 90 * 24 * time.Hour
	DefaultLateAfter  = 2 * time.Minute
	defaultMaxPhoto   = 16 << 20

	maxCommands   = 100
	maxSyncBody   = 256 << 10
	maxText       = 1000
	maxAge        = 90 * 24 * time.Hour // a phone cannot claim to have waited longer than a session lives
	memberTTL     = 3 * time.Second     // how long a membership check may be reused: the delay before a revoked phone is cut off
	rememberLimit = 4096
)

// csp is the dashboard's policy (Day 26) plus what an installed app needs: its manifest, its service
// worker (covered by script-src), and blob: pictures for photos that are still waiting on the phone.
const csp = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' blob: data:; " +
	"manifest-src 'self'; worker-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

type handler struct {
	c   Config
	sg  signer
	lim *limiter
	nc  nonces

	mu       sync.Mutex
	members  map[string]sqlstore.Member
	checked  time.Time
	seen     map[string]string // command ID -> task it touched, for commands already applied
	seenList []string
}

// Handler returns the phone server's handler.
func Handler(c Config) (http.Handler, error) {
	_, hd, err := build(c)
	return hd, err
}

func build(c Config) (*handler, http.Handler, error) {
	if c.Store == nil {
		return nil, nil, errors.New("phone: no store")
	}
	if len(c.Secret) < MinSecret {
		return nil, nil, fmt.Errorf("phone: the session secret must be at least %d bytes", MinSecret)
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.SessionTTL <= 0 {
		c.SessionTTL = DefaultSessionTTL
	}
	if c.SessionTTL > MaxSessionTTL {
		return nil, nil, fmt.Errorf("phone: a session may live at most %s", MaxSessionTTL)
	}
	if c.LateAfter <= 0 {
		c.LateAfter = DefaultLateAfter
	}
	if c.MaxPhoto <= 0 {
		c.MaxPhoto = defaultMaxPhoto
	}
	h := &handler{c: c, sg: signer{key: append([]byte(nil), c.Secret...)}, lim: newLimiter(), seen: map[string]string{}}

	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err) // the embedded folder is part of the program
	}
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(sub))
	mux.HandleFunc("POST /api/pair", h.pair)
	mux.HandleFunc("POST /api/challenge", h.challenge)
	mux.HandleFunc("POST /api/login", h.login)
	mux.HandleFunc("POST /api/logout", h.logout)
	mux.HandleFunc("GET /api/state", h.authed(h.state))
	mux.HandleFunc("POST /api/sync", h.authed(h.sync))
	mux.HandleFunc("POST /api/tasks/{id}/photos", h.authed(h.photo))
	mux.HandleFunc("GET /api/tasks/{id}/attachments/{hash}", h.authed(func(w http.ResponseWriter, r *http.Request, _ session) {
		web.ServeAttachment(w, r, c.Store)
	}))
	return h, h.guard(mux), nil
}

// ---- errors and replies

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, apiError{Code: code, Message: msg})
}

// ---- the rules every request meets

// HostOK reports whether a Host header is one this node serves. A page on evil.example that re-points its
// name at this node ("DNS rebinding") arrives with Host: evil.example and is refused.
func hostOK(hosts []string, hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if host == "" {
		return false
	}
	ip := net.ParseIP(host)
	if len(hosts) > 0 {
		for _, h := range hosts {
			if strings.EqualFold(h, host) {
				return true
			}
			if hip := net.ParseIP(h); hip != nil && ip != nil && hip.Equal(ip) {
				return true
			}
		}
		return false
	}
	if ip != nil {
		return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()
	}
	l := strings.ToLower(host)
	return l == "localhost" || strings.HasSuffix(l, ".local") || strings.HasSuffix(l, ".lan") || strings.HasSuffix(l, ".offgrid")
}

func (h *handler) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", csp)
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("Strict-Transport-Security", "max-age=31536000")
		hd.Set("Cache-Control", "no-cache, no-store, must-revalidate")
		if !hostOK(h.c.Hosts, r.Host) {
			fail(w, http.StatusForbidden, "bad_host", "this node does not answer to that name")
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead:
		case http.MethodPost:
			if r.Header.Get(ActionHeader) != "1" {
				fail(w, http.StatusForbidden, "missing_header", "missing "+ActionHeader+" header")
				return
			}
			if o := r.Header.Get("Origin"); o != "" {
				u, err := url.Parse(o)
				if err != nil || !strings.EqualFold(u.Host, r.Host) {
					fail(w, http.StatusForbidden, "cross_origin", "cross-origin request refused")
					return
				}
			}
			if sf := r.Header.Get("Sec-Fetch-Site"); sf != "" && sf != "same-origin" && sf != "none" {
				fail(w, http.StatusForbidden, "cross_site", "cross-site request refused")
				return
			}
		default:
			hd.Set("Allow", "GET, HEAD, POST")
			fail(w, http.StatusMethodNotAllowed, "method", "method not allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// session is who is asking: an enrolled, un-revoked phone.
type session struct {
	Device string
	Label  string
}

func (h *handler) member(ctx context.Context, device string) (sqlstore.Member, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.c.Now()
	if h.members == nil || now.Sub(h.checked) >= memberTTL || now.Before(h.checked) {
		rep, err := h.c.Store.Members(ctx)
		if err != nil {
			return sqlstore.Member{}, false, err
		}
		h.members = make(map[string]sqlstore.Member, len(rep.Members))
		for _, m := range rep.Members {
			h.members[m.Device] = m
		}
		h.checked = now
	}
	m, ok := h.members[device]
	return m, ok && !m.Revoked, nil
}

// forgetMembers drops the cached membership (after this server itself changed it).
func (h *handler) forgetMembers() {
	h.mu.Lock()
	h.members = nil
	h.mu.Unlock()
}

func (h *handler) authed(next func(http.ResponseWriter, *http.Request, session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ck, err := r.Cookie(CookieName)
		if err != nil {
			fail(w, http.StatusUnauthorized, "unpaired", "this phone is not paired with this node")
			return
		}
		cl, err := h.sg.verify(ck.Value, h.c.Now())
		if err != nil {
			fail(w, http.StatusUnauthorized, "unpaired", "this phone's session is not valid: "+err.Error())
			return
		}
		m, ok, err := h.member(r.Context(), cl.Device)
		if err != nil {
			fail(w, http.StatusInternalServerError, "store", "cannot check the membership: "+err.Error())
			return
		}
		if !ok {
			fail(w, http.StatusUnauthorized, "unpaired", "this phone is no longer enrolled in this node")
			return
		}
		next(w, r, session{Device: m.Device, Label: m.Label})
	}
}

func (h *handler) setCookie(w http.ResponseWriter, device string) time.Time {
	now := h.c.Now()
	exp := now.Add(h.c.SessionTTL)
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: h.sg.issue(device, now, h.c.SessionTTL), Path: "/",
		Expires: exp, MaxAge: int(h.c.SessionTTL.Seconds()), HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	return exp
}

func remoteKey(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

func decode(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			fail(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("the request is over %d bytes", limit))
		} else {
			fail(w, http.StatusBadRequest, "bad_request", "cannot read the request: "+err.Error())
		}
		return false
	}
	if dec.More() {
		fail(w, http.StatusBadRequest, "bad_request", "more than one JSON value in the request")
		return false
	}
	return true
}

// ---- pairing, login, logout

type pairRequest struct {
	Code      string `json:"code"`
	PublicKey string `json:"public_key"`
	Label     string `json:"label"`
	Proof     string `json:"proof"` // base64url of the 64-byte signature over the enrollment message
}

type sessionReply struct {
	Device  string    `json:"device"`
	Label   string    `json:"label"`
	Expires time.Time `json:"expires"`
}

func (h *handler) pair(w http.ResponseWriter, r *http.Request) {
	key, now := remoteKey(r), h.c.Now()
	if !h.lim.allowed(key, now) {
		fail(w, http.StatusTooManyRequests, "slow_down", "too many wrong attempts; wait a minute")
		return
	}
	var req pairRequest
	if !decode(w, r, 4<<10, &req) {
		return
	}
	req.Label = strings.TrimSpace(req.Label)
	proof, err := base64.RawURLEncoding.Strict().DecodeString(req.Proof)
	if err != nil || len(proof) != 64 {
		h.lim.fail(key, now)
		fail(w, http.StatusBadRequest, "bad_proof", "the proof is not a 64-byte signature")
		return
	}
	en, err := h.c.Store.RedeemInvite(r.Context(), req.Code, req.PublicKey, req.Label, proof)
	switch {
	case err == nil:
	case errors.Is(err, sqlstore.ErrInviteInvalid):
		h.lim.fail(key, now)
		fail(w, http.StatusForbidden, "invalid_code", "that pairing code is not valid")
		return
	case errors.Is(err, sqlstore.ErrInviteExpired):
		h.lim.fail(key, now)
		fail(w, http.StatusForbidden, "expired_code", "that pairing code has expired; ask for a new one")
		return
	case errors.Is(err, sqlstore.ErrInviteUsed):
		h.lim.fail(key, now)
		fail(w, http.StatusForbidden, "used_code", "that pairing code was already used; ask for a new one")
		return
	case errors.Is(err, sqlstore.ErrBadSignature):
		h.lim.fail(key, now)
		fail(w, http.StatusBadRequest, "bad_proof", "the phone's proof of its key does not verify")
		return
	case errors.Is(err, sqlstore.ErrAlreadyMember):
		fail(w, http.StatusConflict, "already_member", "this phone is already enrolled; it signs in with its key instead of a code")
		return
	case errors.Is(err, sqlstore.ErrNotAuthorized):
		fail(w, http.StatusInternalServerError, "node_not_admin", "this node is not an admin of the group, so it cannot enroll phones")
		return
	default:
		fail(w, http.StatusBadRequest, "pair_failed", err.Error())
		return
	}
	h.forgetMembers()
	if en.Role != sqlstore.RoleMember {
		// A phone is never an admin. The code was an admin invite; undo what it did.
		_, _ = h.c.Store.Revoke(r.Context(), en.Device)
		h.forgetMembers()
		fail(w, http.StatusForbidden, "not_a_member_invite", "that code was for an admin; a phone can only be a member, so it was not paired")
		return
	}
	exp := h.setCookie(w, en.Device)
	writeJSON(w, http.StatusOK, sessionReply{Device: en.Device, Label: en.Label, Expires: exp})
}

type challengeReply struct {
	Nonce string `json:"nonce"`
}

func (h *handler) challenge(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, challengeReply{Nonce: h.sg.challenge(h.c.Now())})
}

type loginRequest struct {
	Device string `json:"device"`
	Nonce  string `json:"nonce"`
	Sig    string `json:"sig"` // base64url of the 64-byte signature over LoginMessage
}

// LoginMessage is what a paired phone signs to start a new session without a new code.
func LoginMessage(device, nonce string) []byte {
	return []byte("offgrid-phone-login-v1\x00" + device + "\x00" + nonce)
}

func (h *handler) login(w http.ResponseWriter, r *http.Request) {
	key, now := remoteKey(r), h.c.Now()
	if !h.lim.allowed(key, now) {
		fail(w, http.StatusTooManyRequests, "slow_down", "too many wrong attempts; wait a minute")
		return
	}
	var req loginRequest
	if !decode(w, r, 2<<10, &req) {
		return
	}
	deny := func(msg string) {
		h.lim.fail(key, now)
		fail(w, http.StatusUnauthorized, "unpaired", msg)
	}
	if err := h.sg.checkChallenge(req.Nonce, now); err != nil {
		deny("that sign-in has expired; try again")
		return
	}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(req.Sig)
	if err != nil || len(sig) != 64 {
		deny("the signature is not 64 bytes")
		return
	}
	h.forgetMembers() // signing in is rare; look at the membership as it is now
	m, ok, err := h.member(r.Context(), req.Device)
	if err != nil {
		fail(w, http.StatusInternalServerError, "store", "cannot check the membership: "+err.Error())
		return
	}
	if !ok {
		deny("this phone is not enrolled in this node")
		return
	}
	if err := identity.Verify(m.PublicKey, LoginMessage(req.Device, req.Nonce), sig); err != nil {
		deny("the signature does not verify")
		return
	}
	if !h.nc.claim(req.Nonce, now) {
		deny("that sign-in was already used")
		return
	}
	exp := h.setCookie(w, m.Device)
	writeJSON(w, http.StatusOK, sessionReply{Device: m.Device, Label: m.Label, Expires: exp})
}

func (h *handler) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- reading

// StateReply is everything the phone shows, and what it keeps for offline reading.
type StateReply struct {
	Now         time.Time        `json:"now"`
	Node        string           `json:"node"`
	Device      string           `json:"device"`
	Label       string           `json:"label"`
	LateAfterMS int64            `json:"late_after_ms"`
	Tasks       []web.Task       `json:"tasks"`
	TasksTotal  int              `json:"tasks_total"`
	Suggestions []web.Suggestion `json:"suggestions"`
}

func (h *handler) state(w http.ResponseWriter, r *http.Request, s session) {
	v, err := web.Work(r.Context(), h.c.Store)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		fail(w, http.StatusInternalServerError, "store", "cannot read the task log: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, StateReply{Now: h.c.Now().UTC(), Node: h.c.Store.NodeID(), Device: s.Device, Label: s.Label,
		LateAfterMS: h.c.LateAfter.Milliseconds(), Tasks: v.Tasks, TasksTotal: v.TasksTotal, Suggestions: v.Suggestions})
}

// ---- writing: replaying the phone's queue

// Command is one thing the person did on the phone.
type Command struct {
	ID       string `json:"id"` // made by the phone; applying the same ID twice does nothing the second time
	Op       string `json:"op"` // create, status, priority, note, accept, dismiss
	Task     string `json:"task,omitempty"`
	Title    string `json:"title,omitempty"`
	Site     string `json:"site,omitempty"`
	Priority string `json:"priority,omitempty"`
	Status   string `json:"status,omitempty"`
	Text     string `json:"text,omitempty"`
	// Was is the value the phone was showing when the person changed it. If the task has a different value
	// when the command arrives, someone else changed it meanwhile, and the command replaces their change.
	Was string `json:"was,omitempty"`
	// AgeMS is how long the command waited on the phone, by the phone's own clock (so a wrong phone clock
	// does not matter). The node does not trust it for anything but the word "late".
	AgeMS int64 `json:"age_ms"`
}

type syncRequest struct {
	Commands []Command `json:"commands"`
}

// Result is what happened to one command.
type Result struct {
	ID        string `json:"id"`
	Outcome   string `json:"outcome"` // applied, duplicate (already so) or rejected
	Code      string `json:"code,omitempty"`
	Message   string `json:"message,omitempty"`
	Task      string `json:"task,omitempty"` // the task it touched or made: the phone replaces its temporary ID with this
	Late      bool   `json:"late,omitempty"`
	Overwrote string `json:"overwrote,omitempty"`
}

type syncReply struct {
	Results []Result `json:"results"`
}

func validID(id string) bool {
	if len(id) < 8 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func (h *handler) sync(w http.ResponseWriter, r *http.Request, s session) {
	var req syncRequest
	if !decode(w, r, maxSyncBody, &req) {
		return
	}
	if len(req.Commands) > maxCommands {
		fail(w, http.StatusRequestEntityTooLarge, "too_many", fmt.Sprintf("send at most %d commands at once", maxCommands))
		return
	}
	refs := map[string]string{} // command ID -> task it made, for "@ID" references later in the same batch
	out := make([]Result, 0, len(req.Commands))
	for _, c := range req.Commands {
		if r.Context().Err() != nil {
			return // the phone went away; it will send the rest again
		}
		res := h.apply(r.Context(), s, c, refs)
		if res.Task != "" { // only commands that went through carry a task
			refs[c.ID] = res.Task
		}
		out = append(out, res)
	}
	writeJSON(w, http.StatusOK, syncReply{Results: out})
}

func (h *handler) remember(id, taskID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.seen[id]; !ok {
		h.seenList = append(h.seenList, id)
		if len(h.seenList) > rememberLimit {
			delete(h.seen, h.seenList[0])
			h.seenList = h.seenList[1:]
		}
	}
	h.seen[id] = taskID
}

func (h *handler) recall(id string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t, ok := h.seen[id]
	return t, ok
}

func marker(id string) string { return "[c:" + id + "]" }

// findMarker looks for a command's marker in the notes, so a late command that was applied before a
// restart is still recognised when the phone sends it again.
func (h *handler) findMarker(ctx context.Context, id string) (string, bool) {
	ts, err := h.c.Store.List(ctx, task.Filter{})
	if err != nil {
		return "", false
	}
	m := marker(id)
	for _, t := range ts {
		for _, n := range t.Notes {
			if strings.Contains(n.Text, m) {
				return t.ID, true
			}
		}
	}
	return "", false
}

func human(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd%02dh", int(d.Hours())/24, int(d.Hours())%24)
}

// provenance is the sentence the node writes on a task for a command that waited or replaced a newer
// change. It carries the command's marker, which is how a repeat is recognised.
func provenance(what, label string, late bool, age time.Duration, overwrote, id string) string {
	var parts []string
	if late {
		parts = append(parts, "late, made "+human(age)+" before it reached this node")
	}
	if overwrote != "" {
		parts = append(parts, "it replaced a newer change ("+overwrote+")")
	}
	if what != "" {
		what = " (" + what + ")"
	}
	return fmt.Sprintf("from phone %q%s: %s %s", label, what, strings.Join(parts, "; "), marker(id))
}

func clampAge(ms int64) time.Duration {
	if ms < 0 {
		return 0
	}
	if ms > maxAge.Milliseconds() {
		return maxAge
	}
	return time.Duration(ms) * time.Millisecond
}

func resolveRef(ref string, refs map[string]string) (string, bool) {
	if strings.HasPrefix(ref, "@") {
		id, ok := refs[ref[1:]]
		return id, ok
	}
	return ref, ref != ""
}

func (h *handler) apply(ctx context.Context, s session, c Command, refs map[string]string) Result {
	res := Result{ID: c.ID}
	reject := func(code, msg string) Result {
		res.Outcome, res.Code, res.Message = "rejected", code, msg
		return res
	}
	if !validID(c.ID) {
		return reject("bad_id", "a command ID is 8 to 64 letters, digits, - or _")
	}
	age := clampAge(c.AgeMS)
	late := age >= h.c.LateAfter
	if t, ok := h.recall(c.ID); ok {
		res.Outcome, res.Task, res.Message = "duplicate", t, "this command was already applied"
		return res
	}
	if late {
		if t, ok := h.findMarker(ctx, c.ID); ok {
			h.remember(c.ID, t)
			res.Outcome, res.Task, res.Message = "duplicate", t, "this command was already applied"
			return res
		}
	}
	for _, f := range []string{c.Title, c.Site, c.Text} {
		if utf8.RuneCountInString(f) > maxText { // JSON decoding has already made the text valid UTF-8
			return reject("invalid", fmt.Sprintf("text longer than %d characters", maxText))
		}
	}
	// finish records a command that went through and, when it waited or replaced something, says so on the task.
	finish := func(taskID, overwrote string, note bool) Result {
		res.Task, res.Late, res.Overwrote, res.Outcome = taskID, late, overwrote, "applied"
		if (late || overwrote != "") && note {
			if _, err := h.c.Store.AddNote(ctx, taskID, provenance("", s.Label, late, age, overwrote, c.ID)); err != nil {
				res.Message = "applied, but the late marker could not be written: " + err.Error()
			}
		}
		h.remember(c.ID, taskID)
		return res
	}
	storeErr := func(err error) Result {
		switch {
		case errors.Is(err, task.ErrNotFound), errors.Is(err, task.ErrSuggestionNotFound):
			return reject("not_found", "that task or suggestion no longer exists")
		case errors.Is(err, task.ErrStale):
			return reject("stale", "the task was edited after the AI proposed this, so applying it could undo that edit")
		case errors.Is(err, task.ErrSuggestionResolved):
			return reject("already_resolved", err.Error())
		case errors.Is(err, task.ErrInvalid):
			return reject("invalid", err.Error())
		}
		return reject("failed", err.Error())
	}

	switch c.Op {
	case "create":
		pr := task.PriorityMedium
		if c.Priority != "" {
			p, err := task.ParsePriority(c.Priority)
			if err != nil {
				return reject("invalid", err.Error())
			}
			pr = p
		}
		t, created, err := h.c.Store.Create(ctx, task.NewTask{Title: c.Title, Site: c.Site, Priority: pr})
		if err != nil {
			return storeErr(err)
		}
		if !created {
			res.Outcome, res.Task, res.Message = "duplicate", t.ID, "an unfinished task with this title already exists"
			h.remember(c.ID, t.ID)
			return res
		}
		return finish(t.ID, "", true)

	case "status", "priority":
		id, ok := resolveRef(c.Task, refs)
		if !ok {
			return reject("bad_reference", "this refers to a task that is not known")
		}
		cur, err := h.c.Store.Get(ctx, id)
		if err != nil {
			return storeErr(err)
		}
		var ch task.Changes
		var was, now string
		if c.Op == "status" {
			st, err := task.ParseStatus(c.Status)
			if err != nil {
				return reject("invalid", err.Error())
			}
			ch.Status, was, now = &st, string(cur.Status), string(st)
		} else {
			p, err := task.ParsePriority(c.Priority)
			if err != nil {
				return reject("invalid", err.Error())
			}
			ch.Priority, was, now = &p, string(cur.Priority), string(p)
		}
		_, changed, err := h.c.Store.Update(ctx, id, ch)
		if err != nil {
			return storeErr(err)
		}
		if !changed {
			res.Outcome, res.Task, res.Message = "duplicate", id, "the task already has that "+c.Op
			h.remember(c.ID, id)
			return res
		}
		over := ""
		if c.Was != "" && c.Was != was {
			over = fmt.Sprintf("%s was %s, the phone had seen %s, now %s", c.Op, was, c.Was, now)
		}
		return finish(id, over, true)

	case "note":
		id, ok := resolveRef(c.Task, refs)
		if !ok {
			return reject("bad_reference", "this refers to a task that is not known")
		}
		text := strings.TrimSpace(c.Text)
		if text == "" {
			return reject("invalid", "an empty note")
		}
		if late {
			text += " — " + provenance("", s.Label, true, age, "", c.ID) // one op: the note and its marker cannot be separated
		}
		if _, err := h.c.Store.AddNote(ctx, id, text); err != nil {
			return storeErr(err)
		}
		return finish(id, "", false)

	case "accept", "dismiss":
		if c.Task == "" {
			return reject("bad_reference", "name the suggestion")
		}
		t, err := h.c.Store.Resolve(ctx, c.Task, c.Op == "accept")
		if err != nil {
			return storeErr(err)
		}
		return finish(t.ID, "", true)
	}
	return reject("unknown_op", fmt.Sprintf("%q is not one of create, status, priority, note, accept, dismiss", c.Op))
}

// ---- photos

var photoTypes = map[string]bool{"image/jpeg": true, "image/png": true, "image/webp": true}

func (h *handler) photo(w http.ResponseWriter, r *http.Request, s session) {
	taskID := r.PathValue("id")
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !photoTypes[mt] {
		fail(w, http.StatusUnsupportedMediaType, "bad_type", "send a JPEG, PNG or WebP picture with its Content-Type")
		return
	}
	if sum := strings.ToLower(r.Header.Get("X-Offgrid-Sha256")); len(sum) == 64 {
		if atts, err := h.c.Store.Attachments(r.Context(), taskID); err == nil {
			for _, a := range atts {
				if strings.EqualFold(strings.TrimPrefix(a.Hash, "sha256:"), sum) {
					writeJSON(w, http.StatusOK, map[string]string{"outcome": "duplicate", "hash": a.Hash})
					return
				}
			}
		}
	}
	name, err := url.PathUnescape(r.Header.Get("X-Offgrid-Name"))
	if err != nil || strings.TrimSpace(name) == "" {
		name = "photo"
	}
	if utf8.RuneCountInString(name) > 100 {
		name = string([]rune(name)[:100])
	}
	var ageMS int64
	_, _ = fmt.Sscan(r.Header.Get("X-Offgrid-Age-Ms"), &ageMS)
	age := clampAge(ageMS)

	if r.ContentLength > h.c.MaxPhoto {
		fail(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("a photo may be at most %d bytes", h.c.MaxPhoto))
		return
	}
	body := http.MaxBytesReader(w, r.Body, h.c.MaxPhoto)
	att, err := h.c.Store.AddAttachment(r.Context(), taskID, body, r.ContentLength, mt, name)
	var tooBig *http.MaxBytesError
	switch {
	case err == nil:
	case errors.Is(err, task.ErrNotFound):
		fail(w, http.StatusNotFound, "not_found", "that task no longer exists")
		return
	case errors.Is(err, blobstore.ErrTooLarge), errors.As(err, &tooBig):
		fail(w, http.StatusRequestEntityTooLarge, "too_large", "that photo is over the size limit")
		return
	case errors.Is(err, blobstore.ErrEmpty):
		fail(w, http.StatusBadRequest, "empty", "that photo is empty")
		return
	case errors.Is(err, sqlstore.ErrBadAttachment):
		fail(w, http.StatusUnsupportedMediaType, "bad_photo", err.Error())
		return
	case errors.Is(err, task.ErrInvalid):
		fail(w, http.StatusConflict, "invalid", err.Error())
		return
	default:
		fail(w, http.StatusInternalServerError, "failed", err.Error())
		return
	}
	late := age >= h.c.LateAfter
	if late {
		id := "photo-" + att.Hash
		if len(id) > 64 {
			id = id[:64]
		}
		_, _ = h.c.Store.AddNote(r.Context(), taskID, provenance("photo", s.Label, true, age, "", id))
	}
	writeJSON(w, http.StatusCreated, map[string]any{"outcome": "applied", "hash": att.Hash, "late": late})
}
