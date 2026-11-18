// Package hub is the relay a site owner runs (Day 43, ADR 009's "simple and honest" option).
//
// A hub hosts one replica per group, each its own SQLite file, behind https://HOST/g/GROUP/. Nodes sync with
// it exactly as they sync with each other (the same client, the same mutual TLS, the same frames), so two
// sites that cannot reach each other can both reach the hub and meet there. The hub stores signed ops and
// serves them by cursor (a replica's own summary). It cannot forge an op, because it holds no member's key;
// and a node does not take the hub's word for anything, because it checks every signature and the membership
// rules itself. The hub does read the data (see ADR 009 for the design that would stop that, not built).
//
// What it refuses on its own account: a device that is not a member of the group asked for (it gets the same
// "no such group" as for a group that does not exist, so a member of one group cannot probe for another), a
// group's first membership op from anyone but the founder named when the group was added, ops with no
// signature, anything the replica rules refuse (bad chain, bad signature, a signer who is not a member or has
// been revoked past their cutoff), bodies over the limit, and more requests per second than the group's limit.
package hub

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"offgrid/internal/identity"
	"offgrid/internal/store/sqlstore"
	"offgrid/internal/syncer"
)

// Prefix is where a group's sync endpoints live: Prefix + group + "/sync/...".
const Prefix = "/g/"

// CodeNoSuchGroup answers a request for a group that is missing or not the caller's (404).
const CodeNoSuchGroup = "no_such_group"

// CodeRateLimited answers a request over the group's rate (429, with Retry-After).
const CodeRateLimited = "rate_limited"

var (
	groupRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	founderRe = regexp.MustCompile(`^dev_[0-9a-f]{32}$`)
)

// Options configures a Hub. Dir and Identity are required.
type Options struct {
	// Dir holds groups.json and one SQLite file per group. Created 0700.
	Dir string
	// Identity is the hub's device key: its certificate is what nodes pin.
	Identity *identity.Identity
	Now      func() time.Time
	Log      *log.Logger
	// RatePerSecond and RateBurst are each group's request budget (defaults 20 and 40): all of a group's
	// devices share it, so one noisy device cannot starve another group, and cannot starve its own group
	// for longer than the budget refills.
	RatePerSecond float64
	RateBurst     int
	// MaxBody bounds every request body, as sent (default 16 MiB).
	MaxBody int64
	// MaxGroups bounds the groups one hub hosts (default 64).
	MaxGroups int
	// Window is the sync time window passed to each group's handler (zero: the syncer default).
	Window time.Duration
}

// Hub hosts the groups.
type Hub struct {
	opt    Options
	mu     sync.RWMutex
	groups map[string]*group
}

type group struct {
	id      string
	founder string
	st      *sqlstore.Store
	h       http.Handler
	lim     *Limiter
	members syncer.Authorizer
	founded atomic.Bool
	limited atomic.Int64
	lastLog atomic.Int64
}

type record struct {
	ID      string    `json:"id"`
	Founder string    `json:"founder"`
	Added   time.Time `json:"added"`
}

// ErrExists means the group is already hosted.
var ErrExists = errors.New("hub: that group already exists")

// New opens the hub's directory and every group in it.
func New(ctx context.Context, opt Options) (*Hub, error) {
	if opt.Dir == "" || opt.Identity == nil {
		return nil, errors.New("hub: a directory and a device identity are required")
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.RatePerSecond <= 0 {
		opt.RatePerSecond = 20
	}
	if opt.RateBurst <= 0 {
		opt.RateBurst = 40
	}
	if opt.MaxBody <= 0 {
		opt.MaxBody = 16 << 20
	}
	if opt.MaxGroups <= 0 {
		opt.MaxGroups = 64
	}
	if err := os.MkdirAll(opt.Dir, 0o700); err != nil {
		return nil, err
	}
	h := &Hub{opt: opt, groups: map[string]*group{}}
	recs, err := readRegistry(opt.Dir)
	if err != nil {
		return nil, err
	}
	for _, r := range recs {
		if _, err := h.open(ctx, r); err != nil {
			h.Close()
			return nil, fmt.Errorf("hub: opening group %s: %w", r.ID, err)
		}
	}
	return h, nil
}

func registryPath(dir string) string { return filepath.Join(dir, "groups.json") }

func readRegistry(dir string) ([]record, error) {
	b, err := os.ReadFile(registryPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var recs []record
	if err := json.Unmarshal(b, &recs); err != nil {
		return nil, fmt.Errorf("hub: %s is not valid: %w", registryPath(dir), err)
	}
	seen := make(map[string]bool, len(recs))
	for _, r := range recs {
		if !groupRe.MatchString(r.ID) || !founderRe.MatchString(r.Founder) {
			return nil, fmt.Errorf("hub: %s has an invalid entry %q", registryPath(dir), r.ID)
		}
		if seen[r.ID] {
			return nil, fmt.Errorf("hub: %s contains duplicate group %q", registryPath(dir), r.ID)
		}
		seen[r.ID] = true
	}
	return recs, nil
}

// writeRegistry replaces groups.json atomically: a crash leaves the old list or the new one.
func writeRegistry(dir string, recs []record) error {
	b, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "groups-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), registryPath(dir))
}

func (h *Hub) open(ctx context.Context, r record) (*group, error) {
	st, err := sqlstore.Open(ctx, filepath.Join(h.opt.Dir, r.ID+".db"), sqlstore.Options{
		NodeID: "hub", Now: h.opt.Now, Signer: h.opt.Identity, RequireSigned: true,
	})
	if err != nil {
		return nil, err
	}
	g := &group{id: r.ID, founder: r.Founder, st: st,
		lim:     NewLimiter(h.opt.RatePerSecond, h.opt.RateBurst, h.opt.Now),
		members: syncer.MembershipAuthorizer(st, h.opt.Now)}
	g.h = syncer.Handler(st, syncer.ServerOptions{MutualTLS: true, Authorizer: g.authorize, Now: h.opt.Now, Window: h.opt.Window, Log: h.opt.Log})
	h.mu.Lock()
	if existing, ok := h.groups[r.ID]; ok {
		h.mu.Unlock()
		st.Close()
		return existing, nil
	}
	h.groups[r.ID] = g
	h.mu.Unlock()
	return g, nil
}

// authorize is who may use the group: its members once it has any, and until its founding op arrives only the
// founder named when it was added (otherwise whoever connected first could found it).
func (g *group) authorize(device, key string) error {
	if key != "" {
		if did, err := identity.DeviceID(key); err != nil || did != device {
			return syncer.ErrNotEnrolled
		}
	}
	if !g.founded.Load() {
		rep, err := g.st.Members(context.Background())
		if err != nil {
			return err
		}
		if len(rep.Members) > 0 {
			g.founded.Store(true)
		} else if device == g.founder {
			return nil
		} else {
			return syncer.ErrNotEnrolled
		}
	}
	return g.members(device, key)
}

// AddGroup starts hosting a group whose founding admin is the device `founder`. The hub does not hold the
// membership: the founder's node pushes its founding op like any other.
func (h *Hub) AddGroup(ctx context.Context, id, founder string) error {
	if !groupRe.MatchString(id) {
		return fmt.Errorf("hub: a group name is 1 to 40 characters of a-z, 0-9 and '-', starting with a letter or digit (got %q)", id)
	}
	if !founderRe.MatchString(founder) {
		return fmt.Errorf("hub: the founder must be a device ID like dev_ followed by 32 hex digits (got %q)", founder)
	}
	h.mu.Lock()
	if _, ok := h.groups[id]; ok {
		h.mu.Unlock()
		return ErrExists
	}
	if len(h.groups) >= h.opt.MaxGroups {
		h.mu.Unlock()
		return fmt.Errorf("hub: this hub already hosts its limit of %d groups", h.opt.MaxGroups)
	}
	h.mu.Unlock()
	r := record{ID: id, Founder: founder, Added: h.opt.Now().UTC()}
	g, err := h.open(ctx, r)
	if err != nil {
		return err
	}
	recs, err := readRegistry(h.opt.Dir)
	if err != nil {
		h.mu.Lock()
		delete(h.groups, id)
		h.mu.Unlock()
		g.st.Close()
		return err
	}
	if err := writeRegistry(h.opt.Dir, append(recs, r)); err != nil {
		h.mu.Lock()
		delete(h.groups, id)
		h.mu.Unlock()
		g.st.Close()
		return err
	}
	return nil
}

// Reload picks up groups added to groups.json by another process (`offgrid hub group add` while the hub
// runs). It only adds: a group is never dropped from a running hub.
func (h *Hub) Reload(ctx context.Context) (added int, err error) {
	recs, err := readRegistry(h.opt.Dir)
	if err != nil {
		return 0, err
	}
	for _, r := range recs {
		if h.lookup(r.ID) != nil {
			continue
		}
		h.mu.RLock()
		full := len(h.groups) >= h.opt.MaxGroups
		h.mu.RUnlock()
		if full {
			return added, fmt.Errorf("hub: this hub already hosts its limit of %d groups", h.opt.MaxGroups)
		}
		if _, err := h.open(ctx, r); err != nil {
			return added, fmt.Errorf("hub: opening group %s: %w", r.ID, err)
		}
		added++
	}
	return added, nil
}

// GroupInfo describes a hosted group.
type GroupInfo struct {
	ID      string
	Founder string
	Founded bool  // its founding membership op has arrived
	Members int   // enrolled, including revoked ones
	Ops     int64 // ops held
}

// Groups lists the hosted groups by name.
func (h *Hub) Groups(ctx context.Context) ([]GroupInfo, error) {
	h.mu.RLock()
	gs := make([]*group, 0, len(h.groups))
	for _, g := range h.groups {
		gs = append(gs, g)
	}
	h.mu.RUnlock()
	sort.Slice(gs, func(i, j int) bool { return gs[i].id < gs[j].id })
	out := make([]GroupInfo, 0, len(gs))
	for _, g := range gs {
		rep, err := g.st.Members(ctx)
		if err != nil {
			return nil, err
		}
		sum, err := g.st.Summary(ctx)
		if err != nil {
			return nil, err
		}
		var n int64
		for _, s := range sum {
			n += s
		}
		out = append(out, GroupInfo{ID: g.id, Founder: g.founder, Founded: len(rep.Members) > 0, Members: len(rep.Members), Ops: n})
	}
	return out, nil
}

// Close closes every group's database.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, g := range h.groups {
		g.st.Close()
		delete(h.groups, id)
	}
}

// HandshakeAuthorizer decides who may finish a TLS handshake: a member of some hosted group, or the named
// founder of one that has no members yet. Which group a request may touch is decided per request.
func (h *Hub) HandshakeAuthorizer() syncer.Authorizer {
	return func(device, key string) error {
		h.mu.RLock()
		defer h.mu.RUnlock()
		for _, g := range h.groups {
			if g.authorize(device, key) == nil {
				return nil
			}
		}
		return syncer.ErrNotEnrolled
	}
}

// TLSConfig is the hub's listener configuration: TLS 1.3, a client certificate required.
func (h *Hub) TLSConfig() (*tls.Config, error) {
	return syncer.ServerTLSConfig(h.opt.Identity, h.HandshakeAuthorizer(), h.opt.Now)
}

// Server wraps the hub in an http.Server with the syncer's timeouts.
func (h *Hub) Server() (*http.Server, error) {
	cfg, err := h.TLSConfig()
	if err != nil {
		return nil, err
	}
	return syncer.NewHTTPServer(h, cfg, syncer.DefaultTimeouts), nil
}

func (h *Hub) lookup(id string) *group {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.groups[id]
}

func reply(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(syncer.ErrorResponse{Code: code, Error: msg})
}

// ServeHTTP routes /g/GROUP/rest to the group's handler as /rest.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.URL.Path, Prefix)
	id, sub, _ := strings.Cut(rest, "/")
	var g *group
	if ok {
		g = h.lookup(id)
	}
	if g == nil || !h.callerMayUse(r, g) {
		// the same answer for "not there" and "not yours"
		reply(w, http.StatusNotFound, CodeNoSuchGroup, "no such group")
		return
	}
	if allowed, retry := g.lim.Allow(); !allowed {
		h.noteLimited(g)
		secs := int(math.Ceil(retry.Seconds()))
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		reply(w, http.StatusTooManyRequests, CodeRateLimited, "this group is sending requests faster than the hub allows; wait and retry")
		return
	}
	if r.ContentLength > h.opt.MaxBody {
		reply(w, http.StatusRequestEntityTooLarge, syncer.CodeBadRequest, fmt.Sprintf("the request body is over the hub's limit of %d bytes", h.opt.MaxBody))
		return
	}
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/" + sub
	r2.URL.RawPath = ""
	r2.Body = http.MaxBytesReader(w, r.Body, h.opt.MaxBody) // chunked bodies have no Content-Length to check
	g.h.ServeHTTP(w, r2)
}

// callerMayUse checks the TLS identity against the group, here as well as inside the group's handler, so that
// the refusal is the same 404 as for a group that does not exist.
func (h *Hub) callerMayUse(r *http.Request, g *group) bool {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return false
	}
	dev, key, err := identity.DeviceOfCertificate(r.TLS.PeerCertificates[0], h.opt.Now())
	if err != nil {
		return false
	}
	return g.authorize(dev, key) == nil
}

// noteLimited counts a refusal and logs it, at most once a minute per group.
func (h *Hub) noteLimited(g *group) {
	n := g.limited.Add(1)
	now := h.opt.Now().Unix()
	last := g.lastLog.Load()
	if h.opt.Log != nil && now-last >= 60 && g.lastLog.CompareAndSwap(last, now) {
		h.opt.Log.Printf("group %s is over its rate limit (%d requests refused so far)", g.id, n)
	}
}

// Limited says how many requests the group's rate limit has refused since the hub started.
func (h *Hub) Limited(id string) int64 {
	if g := h.lookup(id); g != nil {
		return g.limited.Load()
	}
	return 0
}
