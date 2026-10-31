// Package web is Offgrid's dashboard: one page served from the binary itself.
//
// The page, its script and its style are embedded with go:embed, so the
// dashboard needs no files next to the program, no CDN and no network: it works
// on a laptop with the WiFi off. The server only reads what the other
// commands already keep (the task log, the queue, the snapshots that "drain"
// and "agent -route" publish) and has exactly one way to change anything:
// a person pressing Accept or Dismiss on an AI suggestion, the same rule as
// `offgrid suggestions`.
//
// It listens on this machine only. There is no login and no encryption, so it
// refuses any other address (see RequireLoopback) and any request that does
// not look like it came from its own page (see guard).
package web

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"offgrid/internal/dash"
	"offgrid/internal/jobqueue"
	"offgrid/internal/obs"
	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
)

// Store is the part of the task store the dashboard uses. *sqlstore.Store
// satisfies it.
type Store interface {
	NodeID() string
	Info(ctx context.Context) (sqlstore.Info, error)
	Replicas(ctx context.Context) ([]sqlstore.ReplicaInfo, error)
	LastOps(ctx context.Context, n int) ([]sqlstore.Op, error)
	List(ctx context.Context, f task.Filter) ([]task.Task, error)
	Suggestions(ctx context.Context, all bool) ([]task.Suggestion, error)
	Resolve(ctx context.Context, id string, accept bool) (task.Task, error)
}

// Config is everything a Server reads.
type Config struct {
	Store      Store
	Queue      func(context.Context) (jobqueue.Stats, error) // nil: no queue configured
	MetricsDir string                                        // where "drain" and "agent -route" publish; "" reads none
	Now        func() time.Time                              // default time.Now
	ReadOnly   bool                                          // refuse Accept and Dismiss
}

// Limits on what one poll returns, so a big log cannot make the page heavy.
const (
	MaxTasks     = 200
	MaxMerges    = 12
	MaxDecisions = 12
	MaxFeed      = 12
	opsWindow    = 60 // recent ops looked at to find the ones that came from other replicas
)

// State is the whole dashboard, as JSON. Durations are milliseconds, times are
// RFC 3339 in UTC, so the page does no guessing.
type State struct {
	Now      time.Time `json:"now"`
	ReadOnly bool      `json:"read_only"`

	Node     string    `json:"node"`
	Log      Log       `json:"log"`
	Replicas []Replica `json:"replicas"`
	Merges   []Merge   `json:"merges"` // recent ops that came from other replicas

	Connection Connection `json:"connection"`
	Queue      *Queue     `json:"queue"`
	QueueErr   string     `json:"queue_error,omitempty"`
	Procs      []Proc     `json:"procs"`
	Jobs       []Job      `json:"jobs"`

	Tiers     []Tier     `json:"tiers"`
	Decisions []Decision `json:"decisions"`
	Feed      []FeedItem `json:"feed"`

	Tasks       []Task       `json:"tasks"`
	TasksTotal  int          `json:"tasks_total"`
	Suggestions []Suggestion `json:"suggestions"`

	Problems []string `json:"problems,omitempty"`
}

type Log struct {
	Ops        int    `json:"ops"`
	Tasks      int    `json:"tasks"`
	Hash       string `json:"hash"` // first 12 hex digits of the state hash
	ClockAhead int64  `json:"clock_ahead_ms"`
}

type Replica struct {
	Node   string    `json:"node"`
	Ops    int64     `json:"ops"`
	Newest time.Time `json:"newest"`
	Local  bool      `json:"local"`
}

type Merge struct {
	At   time.Time `json:"at"`
	Node string    `json:"node"`
	Seq  int64     `json:"seq"`
	Text string    `json:"text"`
}

type Connection struct {
	Known     bool       `json:"known"`
	State     string     `json:"state"`
	Since     *time.Time `json:"since,omitempty"`
	Stale     bool       `json:"stale"` // the reporter is not publishing now: last known, not live
	Stopped   bool       `json:"stopped"`
	From      string     `json:"from,omitempty"`
	At        *time.Time `json:"at,omitempty"`
	Probe     *Probe     `json:"probe,omitempty"`
	ProbesOK  int        `json:"probes_ok"`
	ProbeFail int        `json:"probes_failed"`
	ProbeMS   []int64    `json:"probe_ms"` // -1 marks a failed probe
	Flips     []Flip     `json:"flips"`
}

type Probe struct {
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latency_ms"`
	Err       string `json:"err,omitempty"`
}

type Flip struct {
	At     time.Time `json:"at"`
	From   string    `json:"from"`
	To     string    `json:"to"`
	Reason string    `json:"reason,omitempty"`
}

type Queue struct {
	Queued    int        `json:"queued"`
	Running   int        `json:"running"`
	Done      int        `json:"done"`
	Dead      int        `json:"dead"`
	NextReady *time.Time `json:"next_ready,omitempty"`
}

type Proc struct {
	Name    string `json:"name"`
	Fresh   bool   `json:"fresh"`
	Stopped bool   `json:"stopped"`
	AgeMS   int64  `json:"age_ms"`
}

type Job struct {
	Kind    string `json:"kind"`
	Started int    `json:"started"`
	Done    int    `json:"done"`
	Retried int    `json:"retried"`
	Dead    int    `json:"dead"`
	Back    int    `json:"back"` // released or lost, returned to the queue
	P50MS   int64  `json:"p50_ms"`
	P95MS   int64  `json:"p95_ms"`
}

type Tier struct {
	Name  string `json:"name"`
	N     int    `json:"n"`
	P50MS int64  `json:"p50_ms"`
	P95MS int64  `json:"p95_ms"`
}

type Decision struct {
	At      time.Time `json:"at"`
	Text    string    `json:"text"`
	Tier    string    `json:"tier"`
	Reason  string    `json:"reason,omitempty"`
	TotalMS int64     `json:"total_ms"`
}

type FeedItem struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	Text string    `json:"text"`
	Proc string    `json:"proc"`
}

type Task struct {
	ID        string    `json:"id"`
	ShortID   string    `json:"short_id"`
	Title     string    `json:"title"`
	Site      string    `json:"site,omitempty"`
	Status    string    `json:"status"`
	Priority  string    `json:"priority"`
	Notes     int       `json:"notes"`
	NotesList []string  `json:"notes_list,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Suggestion struct {
	ID       string    `json:"id"`
	Task     string    `json:"task"`
	TaskName string    `json:"task_name"` // short ID, and the title when the task still exists
	Status   string    `json:"status,omitempty"`
	Priority string    `json:"priority,omitempty"`
	Note     string    `json:"note,omitempty"`
	By       string    `json:"by"`
	At       time.Time `json:"at"`
	Stale    bool      `json:"stale"` // the task changed since: Accept would be refused
	// What the task shows now, so a person sees what Accept would change.
	NowStatus   string `json:"now_status,omitempty"`
	NowPriority string `json:"now_priority,omitempty"`
}

func ms(d time.Duration) int64 { return d.Milliseconds() }

func utc(t time.Time) time.Time { return t.UTC() }

func optTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := utc(t)
	return &u
}

// Build reads everything the page shows. It never writes. A part that cannot
// be read is reported inside the State (QueueErr, Problems) instead of failing
// the whole screen, except the task log itself: without it there is no page.
func Build(ctx context.Context, c Config) (State, error) {
	if c.Store == nil {
		return State{}, errors.New("no store configured")
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	t0 := now()
	st := State{Now: utc(t0), ReadOnly: c.ReadOnly, Node: c.Store.NodeID(),
		Replicas: []Replica{}, Merges: []Merge{}, Procs: []Proc{}, Jobs: []Job{}, Tiers: []Tier{},
		Decisions: []Decision{}, Feed: []FeedItem{}, Tasks: []Task{}, Suggestions: []Suggestion{}}

	info, err := c.Store.Info(ctx)
	if err != nil {
		return State{}, err
	}
	hash := info.StateHash
	if len(hash) > 12 {
		hash = hash[:12]
	}
	st.Log = Log{Ops: info.Ops, Tasks: info.Tasks, Hash: hash, ClockAhead: ms(info.ClockAhead)}

	reps, err := c.Store.Replicas(ctx)
	if err != nil {
		return State{}, err
	}
	for _, r := range reps {
		st.Replicas = append(st.Replicas, Replica{Node: r.Node, Ops: r.Ops, Newest: utc(r.Newest), Local: r.Node == st.Node})
	}
	// This replica is always listed, even before its first op.
	if !hasNode(st.Replicas, st.Node) {
		st.Replicas = append([]Replica{{Node: st.Node, Local: true}}, st.Replicas...)
	}

	sort.Slice(st.Replicas, func(i, j int) bool { return st.Replicas[i].Node < st.Replicas[j].Node }) // a stable order: the page must not shuffle
	ops, err := c.Store.LastOps(ctx, opsWindow)
	if err != nil {
		return State{}, err
	}
	for i := len(ops) - 1; i >= 0 && len(st.Merges) < MaxMerges; i-- { // newest first
		o := ops[i]
		if o.NodeID == st.Node {
			continue
		}
		st.Merges = append(st.Merges, Merge{At: utc(o.TS), Node: o.NodeID, Seq: o.Seq, Text: o.Describe()})
	}

	tasks, err := c.Store.List(ctx, task.Filter{})
	if err != nil {
		return State{}, err
	}
	byID := make(map[string]task.Task, len(tasks))
	for _, t := range tasks {
		byID[t.ID] = t
	}
	st.TasksTotal = len(tasks)
	for i, t := range tasks {
		if i == MaxTasks {
			break
		}
		var noteTexts []string
		for _, n := range t.Notes {
			if strings.TrimSpace(n.Text) != "" {
				noteTexts = append(noteTexts, n.Text)
			}
		}
		st.Tasks = append(st.Tasks, Task{ID: t.ID, ShortID: task.ShortID(t.ID), Title: t.Title, Site: t.Site,
			Status: string(t.Status), Priority: string(t.Priority), Notes: len(t.Notes), NotesList: noteTexts, UpdatedAt: utc(t.UpdatedAt)})
	}

	sugs, err := c.Store.Suggestions(ctx, false)
	if err != nil {
		return State{}, err
	}
	for _, s := range sugs {
		x := Suggestion{ID: s.ID, Task: s.Task, TaskName: task.ShortID(s.Task), Note: s.Note, By: s.By, At: utc(s.At), Stale: s.Stale}
		if t, ok := byID[s.Task]; ok {
			x.TaskName = task.ShortID(s.Task) + " " + t.Title
			x.NowStatus, x.NowPriority = string(t.Status), string(t.Priority)
		}
		if s.Status != nil {
			x.Status = string(*s.Status)
		}
		if s.Priority != nil {
			x.Priority = string(*s.Priority)
		}
		st.Suggestions = append(st.Suggestions, x)
	}

	// ---- queue, connection, tiers
	if c.Queue == nil {
		st.QueueErr = "no queue file given (use -queue)"
	} else if q, err := c.Queue(ctx); err != nil {
		st.QueueErr = err.Error()
	} else {
		st.Queue = &Queue{Queued: q.Queued, Running: q.Running, Done: q.Done, Dead: q.Dead, NextReady: optTime(q.NextReady)}
	}
	var snaps []obs.Snapshot
	if c.MetricsDir != "" {
		snaps, st.Problems = obs.ReadSnapshots(c.MetricsDir)
	}
	sum := dash.Summarize(snaps, t0)
	st.Connection = connection(sum)
	for _, p := range sum.Procs {
		st.Procs = append(st.Procs, Proc{Name: p.Name, Fresh: p.Fresh, Stopped: p.Stopped, AgeMS: ms(p.Age)})
	}
	kinds := make([]string, 0, len(sum.Jobs))
	for k := range sum.Jobs {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		j, l := sum.Jobs[k], sum.JobLat[k]
		st.Jobs = append(st.Jobs, Job{Kind: k, Started: j.Claimed, Done: j.Done, Retried: j.Retried, Dead: j.Dead,
			Back: j.Released + j.Lost, P50MS: ms(l.P50), P95MS: ms(l.P95)})
	}
	for _, t := range sum.Tiers {
		st.Tiers = append(st.Tiers, Tier{Name: t.Name, N: t.N, P50MS: ms(t.Lat.P50), P95MS: ms(t.Lat.P95)})
	}
	for i, d := range sum.Decided {
		if i == MaxDecisions {
			break
		}
		st.Decisions = append(st.Decisions, Decision{At: utc(d.At), Text: d.Text, Tier: d.Tier, Reason: d.Reason, TotalMS: ms(d.Total)})
	}
	for i, f := range sum.Feed {
		if i == MaxFeed {
			break
		}
		st.Feed = append(st.Feed, FeedItem{At: utc(f.At), Kind: f.Kind, Text: f.Text, Proc: f.Proc})
	}
	return st, nil
}

func hasNode(rs []Replica, n string) bool {
	for _, r := range rs {
		if r.Node == n {
			return true
		}
	}
	return false
}

func connection(s dash.Summary) Connection {
	c := Connection{Known: s.HaveNet, ProbeMS: []int64{}, Flips: []Flip{}}
	if !s.HaveNet {
		return c
	}
	c.State, c.Since, c.Stale, c.Stopped, c.From, c.At = s.Net, optTime(s.NetSince), s.NetStale, s.NetStopped, s.NetFrom, optTime(s.NetAt)
	c.ProbesOK, c.ProbeFail = s.ProbesOK, s.ProbesFail
	if s.Probe != nil {
		c.Probe = &Probe{OK: s.Probe.OK, LatencyMS: ms(s.Probe.Latency), Err: s.Probe.Err}
	}
	if s.ProbeMS != nil {
		c.ProbeMS = s.ProbeMS
	}
	for _, f := range s.Flips {
		c.Flips = append(c.Flips, Flip{At: utc(f.At), From: f.From, To: f.To, Reason: f.Reason})
	}
	return c
}

// Result of pressing Accept or Dismiss.
type Resolved struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// Errors the HTTP layer turns into status codes.
var errReadOnly = errors.New("the dashboard was started read-only")
