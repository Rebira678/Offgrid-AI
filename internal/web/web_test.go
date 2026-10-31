package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"offgrid/internal/jobqueue"
	"offgrid/internal/obs"
	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
)

var t0 = time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)

type clk struct{ t time.Time }

func (c *clk) now() time.Time { return c.t }

func newStore(t *testing.T, node string, c *clk) *sqlstore.Store {
	t.Helper()
	st, err := sqlstore.Open(context.Background(), filepath.Join(t.TempDir(), node+".db"), sqlstore.Options{NodeID: node, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func mk(t *testing.T, s *sqlstore.Store, title string) task.Task {
	t.Helper()
	got, _, err := s.Create(context.Background(), task.NewTask{Title: title, Priority: task.PriorityMedium})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// pull copies everything src has and dst lacks.
func pull(t *testing.T, dst, src *sqlstore.Store) {
	t.Helper()
	ctx := context.Background()
	have, err := dst.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ops, _, err := src.OpsSince(ctx, have, 100000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dst.ApplyRemote(ctx, ops); err != nil {
		t.Fatal(err)
	}
}

func build(t *testing.T, c Config) State {
	t.Helper()
	st, err := Build(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestAnEmptyLogStillGivesAWholePage(t *testing.T) {
	c := &clk{t0}
	cfg := Config{Store: newStore(t, "laptop", c), Now: c.now}
	st := build(t, cfg)
	if st.Node != "laptop" || len(st.Replicas) != 1 || !st.Replicas[0].Local || st.Replicas[0].Ops != 0 {
		t.Errorf("an empty replica must still list itself: %+v", st.Replicas)
	}
	if st.Connection.Known || st.Queue != nil || st.QueueErr == "" {
		t.Errorf("nothing is publishing and no queue is configured: %+v %v %q", st.Connection, st.Queue, st.QueueErr)
	}
	// The page indexes into these lists, so none of them may be JSON null.
	b, _ := json.Marshal(st)
	var raw map[string]json.RawMessage
	json.Unmarshal(b, &raw)
	for _, k := range []string{"merges", "procs", "jobs", "tiers", "decisions", "feed", "tasks", "suggestions"} {
		if string(raw[k]) != "[]" {
			t.Errorf("%s = %s, want []", k, raw[k])
		}
	}
	var conn struct {
		ProbeMS json.RawMessage `json:"probe_ms"`
		Flips   json.RawMessage `json:"flips"`
	}
	json.Unmarshal(raw["connection"], &conn)
	if string(conn.ProbeMS) != "[]" || string(conn.Flips) != "[]" {
		t.Errorf("connection lists: %s %s", conn.ProbeMS, conn.Flips)
	}
}

func TestMergesAreWhatArrivedFromOtherReplicasNewestFirst(t *testing.T) {
	ca, cb := &clk{t0}, &clk{t0.Add(time.Minute)}
	a, b := newStore(t, "laptop", ca), newStore(t, "phone", cb)
	mk(t, a, "local one")
	first := mk(t, b, "from the phone")
	cb.t = cb.t.Add(time.Minute)
	pr := task.PriorityHigh
	if _, _, err := b.Update(context.Background(), first.ID, task.Changes{Priority: &pr}); err != nil {
		t.Fatal(err)
	}
	pull(t, a, b)

	st := build(t, Config{Store: a, Now: ca.now})
	if len(st.Merges) != 2 {
		t.Fatalf("merges = %+v, want the 2 phone ops and none of the laptop's own", st.Merges)
	}
	if st.Merges[0].Node != "phone" || !strings.HasPrefix(st.Merges[0].Text, "changed T1:") || !strings.Contains(st.Merges[1].Text, `created T1 "from the phone"`) {
		t.Errorf("newest first, described: %+v", st.Merges)
	}
	if st.Merges[0].Seq != 2 || st.Merges[1].Seq != 1 {
		t.Errorf("seq = %d, %d", st.Merges[0].Seq, st.Merges[1].Seq)
	}
	if len(st.Replicas) != 2 || st.Replicas[0].Node != "laptop" || !st.Replicas[0].Local || st.Replicas[1].Node != "phone" || st.Replicas[1].Local || st.Replicas[1].Ops != 2 {
		t.Errorf("replicas = %+v", st.Replicas)
	}
	if st.Log.Ops != 3 || st.Log.Tasks != 2 || len(st.Log.Hash) != 12 {
		t.Errorf("log = %+v", st.Log)
	}
}

func TestMergesAreCappedAndTasksAreCapped(t *testing.T) {
	ca, cb := &clk{t0}, &clk{t0}
	a, b := newStore(t, "laptop", ca), newStore(t, "phone", cb)
	for i := 0; i < MaxTasks+5; i++ {
		mk(t, b, fmt.Sprintf("task %d", i))
	}
	pull(t, a, b)
	st := build(t, Config{Store: a, Now: ca.now})
	if len(st.Merges) != MaxMerges {
		t.Errorf("merges = %d, want %d", len(st.Merges), MaxMerges)
	}
	if len(st.Tasks) != MaxTasks || st.TasksTotal != MaxTasks+5 {
		t.Errorf("tasks shown %d of %d, want %d of %d", len(st.Tasks), st.TasksTotal, MaxTasks, MaxTasks+5)
	}
}

func TestSuggestionsShowWhatTheyWouldChangeAndWhenTheyAreStale(t *testing.T) {
	ctx := context.Background()
	c := &clk{t0}
	s := newStore(t, "laptop", c)
	pump := mk(t, s, "Leaking pump")
	filter := mk(t, s, "Replace filter")
	ip, hi := task.StatusInProgress, task.PriorityHigh
	if _, _, err := s.Suggest(ctx, task.Draft{Task: filter.ID, Status: &ip, By: "qwen2.5:7b"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Suggest(ctx, task.Draft{Task: pump.ID, Priority: &hi, Note: "check the seal", By: "qwen2.5:7b"}); err != nil {
		t.Fatal(err)
	}
	low := task.PriorityLow // a person edits the pump after the AI spoke
	if _, _, err := s.Update(ctx, pump.ID, task.Changes{Priority: &low}); err != nil {
		t.Fatal(err)
	}

	st := build(t, Config{Store: s, Now: c.now})
	if len(st.Suggestions) != 2 {
		t.Fatalf("suggestions = %+v", st.Suggestions)
	}
	byTask := map[string]Suggestion{}
	for _, g := range st.Suggestions {
		byTask[g.Task] = g
	}
	f, p := byTask[filter.ID], byTask[pump.ID]
	if f.Stale || f.Status != "in_progress" || f.NowStatus != "open" || f.By != "qwen2.5:7b" || f.TaskName != "T2 Replace filter" {
		t.Errorf("filter suggestion = %+v", f)
	}
	if !p.Stale || p.Priority != "high" || p.NowPriority != "low" || p.Note != "check the seal" || p.Status != "" {
		t.Errorf("pump suggestion = %+v", p)
	}
	if f.ID == "" || !strings.Contains(f.ID, "laptop.") {
		t.Errorf("the id the page sends back must be the real one: %q", f.ID)
	}
}

func TestOnlyTheNewestDecisionsAreSent(t *testing.T) {
	c := &clk{t0}
	dir := t.TempDir()
	snapshot(t, dir, "agent", t0, func(s *obs.Snapshot) {
		for i := 0; i < MaxDecisions+5; i++ {
			s.Decisions = append(s.Decisions, obs.Decision{At: t0.Add(time.Duration(i) * time.Second), Text: fmt.Sprint("m", i), Tier: obs.TierRules})
		}
		for i := 0; i < MaxFeed+5; i++ {
			s.Feed = append(s.Feed, obs.FeedLine{At: t0.Add(time.Duration(i) * time.Second), Kind: "info", Text: fmt.Sprint("e", i)})
		}
	})
	st := build(t, Config{Store: newStore(t, "laptop", c), Now: c.now, MetricsDir: dir})
	if len(st.Decisions) != MaxDecisions || st.Decisions[0].Text != fmt.Sprint("m", MaxDecisions+4) {
		t.Errorf("decisions = %d, first %q: want the newest %d", len(st.Decisions), st.Decisions[0].Text, MaxDecisions)
	}
	if len(st.Feed) != MaxFeed || st.Feed[0].Text != fmt.Sprint("e", MaxFeed+4) {
		t.Errorf("feed = %d, first %q: want the newest %d", len(st.Feed), st.Feed[0].Text, MaxFeed)
	}
}

func TestReplicasAreListedByName(t *testing.T) {
	ca, cb, cc := &clk{t0}, &clk{t0}, &clk{t0}
	a, b, z := newStore(t, "m-middle", ca), newStore(t, "z-last", cb), newStore(t, "a-first", cc)
	mk(t, b, "x")
	mk(t, z, "y")
	pull(t, a, b)
	pull(t, a, z)
	st := build(t, Config{Store: a, Now: ca.now})
	var names []string
	for _, r := range st.Replicas {
		names = append(names, r.Node)
	}
	if fmt.Sprint(names) != "[a-first m-middle z-last]" {
		t.Errorf("replicas = %v, want them by name so the page does not shuffle", names)
	}
}

func TestQueueAndItsFailureAreBothReported(t *testing.T) {
	c := &clk{t0}
	s := newStore(t, "laptop", c)
	next := t0.Add(90 * time.Second)
	cfg := Config{Store: s, Now: c.now, Queue: func(context.Context) (jobqueue.Stats, error) {
		return jobqueue.Stats{Queued: 3, Running: 1, Done: 7, Dead: 2, NextReady: next}, nil
	}}
	st := build(t, cfg)
	if st.Queue == nil || st.Queue.Queued != 3 || st.Queue.Running != 1 || st.Queue.Done != 7 || st.Queue.Dead != 2 || st.Queue.NextReady == nil || !st.Queue.NextReady.Equal(next) || st.QueueErr != "" {
		t.Errorf("queue = %+v err %q", st.Queue, st.QueueErr)
	}
	cfg.Queue = func(context.Context) (jobqueue.Stats, error) {
		return jobqueue.Stats{}, errors.New("no queue yet at /x/queue.db")
	}
	st = build(t, cfg)
	if st.Queue != nil || st.QueueErr != "no queue yet at /x/queue.db" {
		t.Errorf("a queue that cannot be read must say so, not show zeros: %+v %q", st.Queue, st.QueueErr)
	}
	cfg.Queue = func(context.Context) (jobqueue.Stats, error) { return jobqueue.Stats{Queued: 1}, nil }
	if st = build(t, cfg); st.Queue.NextReady != nil {
		t.Errorf("no NextReady time must be omitted, got %v", st.Queue.NextReady)
	}
}

func snapshot(t *testing.T, dir, proc string, at time.Time, edit func(*obs.Snapshot)) {
	t.Helper()
	s := obs.Snapshot{Version: obs.SnapshotVersion, Proc: proc, PID: 1, Started: at.Add(-time.Hour), At: at, Interval: time.Second,
		Jobs: map[string]obs.JobCounts{}, JobLatency: map[string]obs.Latency{}, Tiers: map[string]int{}, TierLat: map[string]obs.Latency{}}
	edit(&s)
	if err := obs.WriteSnapshot(obs.SnapshotPath(dir, proc), s); err != nil {
		t.Fatal(err)
	}
}

func TestConnectionTiersAndDecisionsComeFromThePublishingProcesses(t *testing.T) {
	c := &clk{t0}
	dir := t.TempDir()
	snapshot(t, dir, "drain", t0, func(s *obs.Snapshot) {
		s.Net, s.NetSince = "offline", t0.Add(-5*time.Minute)
		s.Probes.OK, s.Probes.Fail = 4, 2
		s.LastProbe = &obs.Probe{At: t0, OK: false, Latency: 1500 * time.Millisecond, Err: "dial tcp: no route"}
		s.ProbeMS = []int64{20, -1, 30}
		s.Flips = []obs.Flip{{At: t0.Add(-5 * time.Minute), From: "online", To: "offline", Reason: "probe failed"}}
		s.Jobs["interpret"] = obs.JobCounts{Claimed: 5, Done: 4, Retried: 1, Released: 1, Lost: 1}
		s.JobLatency["interpret"] = obs.Latency{N: 4, P50: 40 * time.Millisecond, P95: 90 * time.Millisecond}
		s.Feed = []obs.FeedLine{{At: t0.Add(-time.Minute), Kind: "warn", Text: "went offline"}}
	})
	snapshot(t, dir, "agent", t0, func(s *obs.Snapshot) {
		s.Tiers = map[string]int{obs.TierRules: 7, obs.TierModel: 2, obs.TierDeferred: 1}
		s.TierLat = map[string]obs.Latency{obs.TierModel: {N: 2, P50: 800 * time.Millisecond, P95: 2 * time.Second}}
		s.Decisions = []obs.Decision{{At: t0.Add(-2 * time.Second), Text: "pump is loud", Tier: obs.TierModel, Reason: "needs a model", Total: 900 * time.Millisecond}}
		s.Feed = []obs.FeedLine{{At: t0, Kind: "ok", Text: "answered"}}
	})
	st := build(t, Config{Store: newStore(t, "laptop", c), Now: c.now, MetricsDir: dir})

	cn := st.Connection
	if !cn.Known || cn.State != "offline" || cn.Stale || cn.From != "drain" || cn.ProbesOK != 4 || cn.ProbeFail != 2 {
		t.Errorf("connection = %+v", cn)
	}
	if cn.Probe == nil || cn.Probe.OK || cn.Probe.LatencyMS != 1500 || cn.Probe.Err != "dial tcp: no route" {
		t.Errorf("probe = %+v", cn.Probe)
	}
	if fmt.Sprint(cn.ProbeMS) != "[20 -1 30]" || len(cn.Flips) != 1 || cn.Flips[0].To != "offline" {
		t.Errorf("probe times / flips: %v %v", cn.ProbeMS, cn.Flips)
	}
	if len(st.Jobs) != 1 || st.Jobs[0] != (Job{Kind: "interpret", Started: 5, Done: 4, Retried: 1, Back: 2, P50MS: 40, P95MS: 90}) {
		t.Errorf("jobs = %+v", st.Jobs)
	}
	if len(st.Tiers) != 3 || st.Tiers[0].Name != obs.TierRules || st.Tiers[0].N != 7 || st.Tiers[1].Name != obs.TierModel || st.Tiers[1].P95MS != 2000 || st.Tiers[2].Name != obs.TierDeferred {
		t.Errorf("tiers (fixed order, rules first) = %+v", st.Tiers)
	}
	if len(st.Decisions) != 1 || st.Decisions[0].Text != "pump is loud" || st.Decisions[0].TotalMS != 900 || st.Decisions[0].Reason != "needs a model" {
		t.Errorf("decisions = %+v", st.Decisions)
	}
	if len(st.Feed) != 2 || st.Feed[0].Text != "answered" || st.Feed[0].Proc != "agent" || st.Feed[1].Proc != "drain" {
		t.Errorf("feed newest first = %+v", st.Feed)
	}
	if len(st.Procs) != 2 || st.Procs[0].Name != "agent" || !st.Procs[0].Fresh {
		t.Errorf("procs = %+v", st.Procs)
	}
}

func TestAStoppedOrSilentProcessIsNeverShownAsLive(t *testing.T) {
	c := &clk{t0.Add(time.Hour)} // an hour later, nobody has published since t0
	dir := t.TempDir()
	snapshot(t, dir, "drain", t0, func(s *obs.Snapshot) { s.Net, s.NetSince = "online", t0 })
	st := build(t, Config{Store: newStore(t, "laptop", c), Now: c.now, MetricsDir: dir})
	if !st.Connection.Stale || st.Connection.State != "online" || st.Connection.Stopped || st.Procs[0].Fresh {
		t.Errorf("a silent process: %+v %+v", st.Connection, st.Procs)
	}
	snapshot(t, dir, "drain", t0, func(s *obs.Snapshot) { s.Net, s.NetSince, s.Stopped = "online", t0, true })
	st = build(t, Config{Store: newStore(t, "laptop", c), Now: c.now, MetricsDir: dir})
	if !st.Connection.Stale || !st.Connection.Stopped || !st.Procs[0].Stopped {
		t.Errorf("a stopped process: %+v %+v", st.Connection, st.Procs)
	}
}

func TestADamagedSnapshotIsAProblemNotACrash(t *testing.T) {
	c := &clk{t0}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "drain.json"), []byte("{not json"), 0o644)
	st := build(t, Config{Store: newStore(t, "laptop", c), Now: c.now, MetricsDir: dir})
	if len(st.Problems) != 1 || !strings.Contains(st.Problems[0], "drain.json") {
		t.Errorf("problems = %v", st.Problems)
	}
}

// ---- HTTP ----------------------------------------------------------------

type rig struct {
	t   *testing.T
	st  *sqlstore.Store
	cfg Config
	srv *httptest.Server
	url string
}

func newRig(t *testing.T, mod func(*Config)) *rig {
	t.Helper()
	c := &clk{t0}
	st := newStore(t, "laptop", c)
	cfg := Config{Store: st, Now: c.now}
	if mod != nil {
		mod(&cfg)
	}
	srv := httptest.NewServer(Handler(cfg))
	t.Cleanup(srv.Close)
	return &rig{t: t, st: st, cfg: cfg, srv: srv, url: srv.URL}
}

// do sends a request to the rig. host, when set, replaces the Host header.
func (r *rig) do(method, path, host string, hdr map[string]string) (int, http.Header, string) {
	r.t.Helper()
	req, err := http.NewRequest(method, r.url+path, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}

func (r *rig) post(path string) (int, string) {
	code, _, body := r.do("POST", path, "", map[string]string{ActionHeader: "1"})
	return code, body
}

func (r *rig) pending() []task.Suggestion {
	r.t.Helper()
	got, err := r.st.Suggestions(context.Background(), false)
	if err != nil {
		r.t.Fatal(err)
	}
	return got
}

// suggest creates a task and an AI suggestion for it, and returns the suggestion.
func (r *rig) suggest(title string) (task.Task, task.Suggestion) {
	r.t.Helper()
	tk := mk(r.t, r.st, title)
	ip := task.StatusInProgress
	sg, _, err := r.st.Suggest(context.Background(), task.Draft{Task: tk.ID, Status: &ip, By: "qwen2.5:7b"})
	if err != nil {
		r.t.Fatal(err)
	}
	return tk, sg
}

func TestThePageIsServedFromTheBinary(t *testing.T) {
	r := newRig(t, nil)
	code, h, body := r.do("GET", "/", "", nil)
	if code != 200 || !strings.Contains(h.Get("Content-Type"), "text/html") || !strings.Contains(body, "<title>Offgrid</title>") {
		t.Fatalf("GET / = %d %q", code, h.Get("Content-Type"))
	}
	for path, typ := range map[string]string{"/app.js": "javascript", "/style.css": "text/css"} {
		code, h, body := r.do("GET", path, "", nil)
		if code != 200 || !strings.Contains(h.Get("Content-Type"), typ) || len(body) < 500 {
			t.Errorf("GET %s = %d %q (%d bytes)", path, code, h.Get("Content-Type"), len(body))
		}
	}
	// The page must name only files that are really embedded.
	_, _, index := r.do("GET", "/", "", nil)
	for _, m := range regexp.MustCompile(`(?:src|href)="([^"#]+)"`).FindAllStringSubmatch(index, -1) {
		if strings.HasPrefix(m[1], "data:") {
			continue
		}
		if code, _, _ := r.do("GET", "/"+m[1], "", nil); code != 200 {
			t.Errorf("the page refers to %q, which is not served (%d)", m[1], code)
		}
	}
}

func TestThePageNeedsNothingFromTheInternetAndNeverBuildsHTMLFromData(t *testing.T) {
	for _, name := range []string{"index.html", "app.js", "style.css"} {
		b, err := staticFiles.ReadFile("static/" + name)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		for _, bad := range []string{"https://", "@import", "url(http"} {
			if strings.Contains(s, bad) {
				t.Errorf("%s contains %q: the page must work with no internet", name, bad)
			}
		}
		if name == "app.js" {
			if strings.Contains(s, "http://") {
				t.Errorf("app.js mentions an absolute URL")
			}
			for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function"} {
				if strings.Contains(s, bad) {
					t.Errorf("app.js uses %s: task titles and AI text must only ever go in as text", bad)
				}
			}
		}
		if name == "index.html" && regexp.MustCompile(`(?i)<script(?:\s[^>]*)?>[^<]|\son[a-z]+=|style=`).MatchString(s) {
			t.Errorf("index.html has inline script, handlers or style, which the page's CSP forbids")
		}
	}
}

func TestEveryResponseCarriesTheSecurityHeaders(t *testing.T) {
	r := newRig(t, nil)
	for _, c := range []struct{ method, path, host string }{
		{"GET", "/", ""}, {"GET", "/app.js", ""}, {"GET", "/api/state", ""}, {"GET", "/nope", ""},
		{"GET", "/api/state", "evil.example"}, {"DELETE", "/api/state", ""}, {"POST", "/api/suggestions/x/accept", ""},
	} {
		_, h, _ := r.do(c.method, c.path, c.host, nil)
		for k, want := range map[string]string{"Content-Security-Policy": csp, "X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer"} {
			if got := h.Get(k); got != want {
				t.Errorf("%s %s (host %q): %s = %q", c.method, c.path, c.host, k, got)
			}
		}
	}
	if !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe") || !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("csp = %q", csp)
	}
	_, h, _ := r.do("GET", "/api/state", "", nil)
	if h.Get("Cache-Control") != "no-store" {
		t.Errorf("state must not be cached: %q", h.Get("Cache-Control"))
	}
}

func TestOnlyThisMachineMayTalkToIt(t *testing.T) {
	r := newRig(t, nil)
	for _, host := range []string{"evil.example", "evil.example:8787", "192.168.1.20:8787", "localhost.evil.example", "127.0.0.1.evil.example", "10.0.0.1"} {
		for _, path := range []string{"/", "/api/state", "/app.js"} {
			if code, _, _ := r.do("GET", path, host, nil); code != http.StatusForbidden {
				t.Errorf("Host %q GET %s = %d, want 403 (DNS rebinding)", host, path, code)
			}
		}
	}
	for _, host := range []string{"localhost", "localhost:8787", "LOCALHOST", "127.0.0.1", "127.0.0.1:9", "[::1]:8787", "127.5.5.5"} {
		if code, _, _ := r.do("GET", "/api/state", host, nil); code != 200 {
			t.Errorf("Host %q = %d, want 200", host, code)
		}
	}
}

func TestLocalHostAndRequireLoopback(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost": true, "localhost:1": true, "127.0.0.1": true, "127.0.0.1:80": true, "[::1]": true, "[::1]:80": true, "::1": true,
		"": false, "example.com": false, "0.0.0.0": false, "0.0.0.0:80": false, "192.168.0.2": false, "[::]:80": false, "localhost.": false,
	} {
		if got := LocalHost(host); got != want {
			t.Errorf("LocalHost(%q) = %v, want %v", host, got, want)
		}
	}
	for addr, ok := range map[string]bool{
		"127.0.0.1:8787": true, "127.0.0.1:0": true, "localhost:8787": true, "[::1]:8787": true, "127.9.9.9:1": true,
		"0.0.0.0:8787": false, ":8787": false, "192.168.1.5:8787": false, "example.com:80": false, "[::]:8787": false, "8787": false, "": false,
	} {
		err := RequireLoopback(addr)
		if (err == nil) != ok {
			t.Errorf("RequireLoopback(%q) = %v, want ok=%v", addr, err, ok)
		}
	}
}

func TestOnlyReadsAndOneKindOfPost(t *testing.T) {
	r := newRig(t, nil)
	for _, m := range []string{"PUT", "DELETE", "PATCH", "TRACE", "OPTIONS"} {
		code, h, _ := r.do(m, "/api/state", "", nil)
		if code != http.StatusMethodNotAllowed || h.Get("Allow") == "" {
			t.Errorf("%s = %d", m, code)
		}
	}
	if code, _, _ := r.do("POST", "/api/state", "", map[string]string{ActionHeader: "1"}); code != 405 {
		t.Errorf("POST /api/state = %d, want 405 (the mux has no such route)", code)
	}
	_, sg := r.suggest("Replace filter")
	if code, _, _ := r.do("GET", "/api/suggestions/"+sg.ID+"/accept", "", nil); code == 200 || len(r.pending()) != 1 {
		t.Errorf("GET on the action route = %d, pending %d: a link or an image tag must never change anything", code, len(r.pending()))
	}
}

func TestAPostFromAnotherSiteIsRefusedAndChangesNothing(t *testing.T) {
	r := newRig(t, nil)
	_, sg := r.suggest("Replace filter")
	path := "/api/suggestions/" + sg.ID + "/accept"
	host := strings.TrimPrefix(r.url, "http://")
	for name, hdr := range map[string]map[string]string{
		"no header":          {},
		"wrong header value": {ActionHeader: "0"},
		"cross origin":       {ActionHeader: "1", "Origin": "http://evil.example"},
		"other port":         {ActionHeader: "1", "Origin": "http://127.0.0.1:1"},
		"null origin":        {ActionHeader: "1", "Origin": "null"},
		"unreadable origin":  {ActionHeader: "1", "Origin": "http://%zz"},
		"cross site fetch":   {ActionHeader: "1", "Sec-Fetch-Site": "cross-site"},
		"same site fetch":    {ActionHeader: "1", "Sec-Fetch-Site": "same-site"},
	} {
		if code, _, body := r.do("POST", path, "", hdr); code != http.StatusForbidden {
			t.Errorf("%s: %d %s, want 403", name, code, body)
		}
	}
	if len(r.pending()) != 1 {
		t.Fatal("a refused request must not resolve the suggestion")
	}
	// The real page: same origin, and the headers a browser adds.
	ok := map[string]string{ActionHeader: "1", "Origin": "http://" + host, "Sec-Fetch-Site": "same-origin"}
	if code, _, body := r.do("POST", path, "", ok); code != 200 {
		t.Fatalf("same-origin POST = %d %s", code, body)
	}
	if len(r.pending()) != 0 {
		t.Error("accepting should have resolved it")
	}
}

func TestAcceptAppliesTheChangeAndDismissLeavesTheTaskAlone(t *testing.T) {
	r := newRig(t, nil)
	tkA, sgA := r.suggest("Replace filter")
	tkD, sgD := r.suggest("Leaking pump")

	code, body := r.post("/api/suggestions/" + sgA.ID + "/accept")
	var res Resolved
	json.Unmarshal([]byte(body), &res)
	if code != 200 || !res.OK || res.Message != "accepted: T1 is now in_progress, medium" {
		t.Fatalf("accept = %d %s", code, body)
	}
	if got, _ := r.st.Get(context.Background(), tkA.ID); got.Status != task.StatusInProgress {
		t.Errorf("accepted task is %s", got.Status)
	}
	code, body = r.post("/api/suggestions/" + sgD.ID + "/dismiss")
	json.Unmarshal([]byte(body), &res)
	if code != 200 || res.Message != "dismissed: T2 is unchanged" {
		t.Fatalf("dismiss = %d %s", code, body)
	}
	if got, _ := r.st.Get(context.Background(), tkD.ID); got.Status != task.StatusOpen {
		t.Errorf("a dismissed suggestion changed the task to %s", got.Status)
	}
	if len(r.pending()) != 0 {
		t.Error("both should be resolved")
	}
	// The page's next poll no longer lists them.
	_, _, st := r.do("GET", "/api/state", "", nil)
	var s State
	json.Unmarshal([]byte(st), &s)
	if len(s.Suggestions) != 0 {
		t.Errorf("state still lists %+v", s.Suggestions)
	}
}

func TestAcceptingAStaleSuggestionIsRefusedBeforeAnythingChanges(t *testing.T) {
	r := newRig(t, nil)
	tk, sg := r.suggest("Replace filter")
	done := task.StatusDone // a person finishes the task after the AI proposed in_progress
	if _, _, err := r.st.Update(context.Background(), tk.ID, task.Changes{Status: &done}); err != nil {
		t.Fatal(err)
	}
	code, body := r.post("/api/suggestions/" + sg.ID + "/accept")
	if code != http.StatusConflict || !strings.Contains(body, "edited after the AI proposed") {
		t.Fatalf("stale accept = %d %s", code, body)
	}
	if got, _ := r.st.Get(context.Background(), tk.ID); got.Status != task.StatusDone {
		t.Errorf("the person's edit was overwritten: %s", got.Status)
	}
	if len(r.pending()) != 1 {
		t.Error("a refused accept must leave the suggestion pending so it can be dismissed")
	}
	if code, _ := r.post("/api/suggestions/" + sg.ID + "/dismiss"); code != 200 {
		t.Errorf("a stale suggestion can still be dismissed: %d", code)
	}
}

func TestResolveErrorsHaveTheRightStatus(t *testing.T) {
	r := newRig(t, nil)
	_, sg := r.suggest("Replace filter")
	if code, _ := r.post("/api/suggestions/nobody.99/accept"); code != 404 {
		t.Errorf("unknown suggestion = %d, want 404", code)
	}
	if code, body := r.post("/api/suggestions/" + sg.ID + "/explode"); code != 404 || !strings.Contains(body, "accept or dismiss") {
		t.Errorf("unknown action = %d %s", code, body)
	}
	if code, _ := r.post("/api/suggestions/" + sg.ID + "/dismiss"); code != 200 {
		t.Fatal("dismiss failed")
	}
	if code, _ := r.post("/api/suggestions/" + sg.ID + "/accept"); code != 409 {
		t.Errorf("resolving twice = %d, want 409", code)
	}
}

func TestReadOnlyRefusesEvenAValidRequest(t *testing.T) {
	r := newRig(t, func(c *Config) { c.ReadOnly = true })
	_, sg := r.suggest("Replace filter")
	for _, a := range []string{"accept", "dismiss"} {
		if code, body := r.post("/api/suggestions/" + sg.ID + "/" + a); code != 403 || !strings.Contains(body, "read-only") {
			t.Errorf("%s = %d %s", a, code, body)
		}
	}
	if len(r.pending()) != 1 {
		t.Error("read-only changed something")
	}
	_, _, st := r.do("GET", "/api/state", "", nil)
	if !strings.Contains(st, `"read_only":true`) {
		t.Error("the page needs to know it is read-only")
	}
}

func TestTheStateEndpointServesTheState(t *testing.T) {
	r := newRig(t, nil)
	r.suggest("Replace filter")
	code, h, body := r.do("GET", "/api/state", "", nil)
	var s State
	if code != 200 || !strings.HasPrefix(h.Get("Content-Type"), "application/json") || json.Unmarshal([]byte(body), &s) != nil {
		t.Fatalf("GET /api/state = %d %q", code, h.Get("Content-Type"))
	}
	if s.Node != "laptop" || len(s.Tasks) != 1 || len(s.Suggestions) != 1 || !s.Now.Equal(t0) {
		t.Errorf("state = %+v", s)
	}
	// Titles are data: markup in them must come back as plain JSON text.
	mk(t, r.st, `<img src=x onerror=alert(1)>`)
	_, _, body = r.do("GET", "/api/state", "", nil)
	if !strings.Contains(body, `\u003cimg src=x onerror=alert(1)\u003e`) {
		t.Errorf("a title was not escaped in the JSON: %s", body)
	}
}

func TestItCannotBeUsedToReadOtherFiles(t *testing.T) {
	r := newRig(t, nil)
	for _, p := range []string{"/../go.mod", "/%2e%2e/go.mod", "/static/../../go.mod", "/..%2fgo.mod", "/static/", "/index.html/../../x"} {
		_, _, body := r.do("GET", p, "", nil)
		if strings.Contains(body, "module offgrid") {
			t.Errorf("GET %s leaked a file", p)
		}
	}
}

func TestManyTabsAtOnceWhileSomeoneResolves(t *testing.T) {
	r := newRig(t, nil)
	var ids []string
	for i := 0; i < 6; i++ {
		_, sg := r.suggest(fmt.Sprintf("task %d", i))
		ids = append(ids, sg.ID)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				if code, _, body := r.do("GET", "/api/state", "", nil); code != 200 {
					t.Errorf("poll = %d %s", code, body)
				}
			}
		}()
	}
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code, body := r.post("/api/suggestions/" + id + "/dismiss"); code != 200 {
				t.Errorf("dismiss = %d %s", code, body)
			}
		}()
	}
	wg.Wait()
	if len(r.pending()) != 0 {
		t.Error("some dismissals were lost")
	}
}

func TestServeListensOnLoopbackAndStopsWhenAsked(t *testing.T) {
	c := &clk{t0}
	cfg := Config{Store: newStore(t, "laptop", c), Now: c.now}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, "127.0.0.1:0", cfg, func(a net.Addr) { addr <- a.String() }) }()
	var a string
	select {
	case a = <-addr:
	case err := <-done:
		t.Fatalf("Serve returned early: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve never started")
	}
	resp, err := http.Get("http://" + a + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status %d", resp.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("a clean stop returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop")
	}
	if _, err := http.Get("http://" + a + "/"); err == nil {
		t.Error("still listening after the stop")
	}
}

func TestServeRefusesAnAddressOffThisMachineWithoutOpeningASocket(t *testing.T) {
	c := &clk{t0}
	called := false
	err := Serve(context.Background(), "0.0.0.0:0", Config{Store: newStore(t, "laptop", c)}, func(net.Addr) { called = true })
	if err == nil || !strings.Contains(err.Error(), "no login and no encryption") || called {
		t.Errorf("err=%v called=%v", err, called)
	}
}
