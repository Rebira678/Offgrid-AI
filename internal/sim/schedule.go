// Package sim is a deterministic simulation of the whole replicated store.
//
// One seed produces one failure schedule: edits on three replicas, network
// partitions, clock skew, crashes and restarts, and a lossy, duplicating,
// delaying link between them. The schedule is run against the real sqlstore
// (real SQLite files, real op log, real sync functions), then everything is
// healed and the replicas must agree.
//
// "Deterministic" is the point. The only source of randomness is the seed,
// which is turned into a plain list of Actions before anything runs. There are
// no goroutines, no real clock and no real network inside a run, so the same
// seed gives the same trace and the same final hash, every time, on any
// machine. A failing seed is therefore a complete bug report, and Shrink cuts
// its schedule down to the few actions that matter.
//
// What it is not: it does not model a disk that loses committed writes, a
// replica restored from an old backup (that forks the log, which sync refuses
// by design), or a malicious peer. See docs/day-24.md.
package sim

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"
)

// Kind is what an Action does.
type Kind string

const (
	Create    Kind = "create"    // replica A creates a task
	Update    Kind = "update"    // A changes status and/or priority of a task
	Note      Kind = "note"      // A adds a note
	Delete    Kind = "delete"    // A deletes a task
	Suggest   Kind = "suggest"   // an AI on A proposes a change
	Resolve   Kind = "resolve"   // a person on A accepts or dismisses a suggestion
	Skew      Kind = "skew"      // A's physical clock is set to true time plus an offset
	Crash     Kind = "crash"     // A is killed and restarted from its file; maybe its clock resets
	Partition Kind = "partition" // the link between A and B goes down
	Heal      Kind = "heal"      // the link between A and B comes back
	Sync      Kind = "sync"      // A pulls from B over the (possibly faulty) link
	Wait      Kind = "wait"      // time passes
)

// Action is one step of a schedule. It never refers to state: a task is chosen
// as "the N-th one that exists when this runs", so any subset of a schedule is
// still a valid schedule. That is what lets Shrink delete actions freely.
type Action struct {
	Kind Kind
	A, B int // replica indexes (B only for Partition, Heal, Sync)
	N    int // a random number the action interprets (which task, which value, which fault)
}

func (a Action) String() string {
	switch a.Kind {
	case Partition, Heal, Sync:
		return fmt.Sprintf("%s r%d-r%d n=%d", a.Kind, a.A, a.B, a.N)
	case Wait:
		return fmt.Sprintf("wait n=%d", a.N)
	}
	return fmt.Sprintf("%s r%d n=%d", a.Kind, a.A, a.N)
}

// weights decide how often each kind of action appears. Edits and syncs
// dominate; the nasty events are rarer, as they are in life.
var weights = []struct {
	k Kind
	w int
}{
	{Create, 10}, {Update, 14}, {Note, 6}, {Delete, 5},
	{Suggest, 6}, {Resolve, 5},
	{Skew, 4}, {Crash, 4},
	{Partition, 5}, {Heal, 5},
	{Sync, 30}, {Wait, 6},
}

// Generate turns a seed into a schedule. It is a pure function of its
// arguments.
func Generate(seed int64, cfg Config) []Action {
	cfg = cfg.withDefaults()
	rng := rand.New(rand.NewSource(seed))
	total := 0
	for _, w := range weights {
		total += w.w
	}
	out := make([]Action, 0, cfg.Steps)
	for i := 0; i < cfg.Steps; i++ {
		pick := rng.Intn(total)
		var k Kind
		for _, w := range weights {
			if pick < w.w {
				k = w.k
				break
			}
			pick -= w.w
		}
		a := Action{Kind: k, A: rng.Intn(cfg.Replicas), N: rng.Intn(1 << 20)}
		if k == Partition || k == Heal || k == Sync {
			a.B = rng.Intn(cfg.Replicas - 1)
			if a.B >= a.A {
				a.B++ // never itself
			}
		}
		if k == Wait {
			a.A = 0 // time passes for everyone; there is no replica to print
		}
		out = append(out, a)
	}
	return out
}

// ParseAction reads what Action.String prints, so a shrunk schedule can be
// saved in a file and replayed.
func ParseAction(line string) (Action, error) {
	var a Action
	f := strings.Fields(line)
	bad := func() (Action, error) { return Action{}, fmt.Errorf("sim: cannot read action %q", line) }
	if len(f) < 2 {
		return bad()
	}
	a.Kind = Kind(f[0])
	known := false
	for _, w := range weights {
		if w.k == a.Kind {
			known = true
		}
	}
	if !known {
		return bad()
	}
	last := f[len(f)-1]
	if !strings.HasPrefix(last, "n=") {
		return bad()
	}
	n, err := strconv.Atoi(last[2:])
	if err != nil || n < 0 {
		return bad()
	}
	a.N = n
	switch a.Kind {
	case Wait:
		if len(f) != 2 {
			return bad()
		}
	case Partition, Heal, Sync:
		if len(f) != 3 {
			return bad()
		}
		var x, y int
		if _, err := fmt.Sscanf(f[1], "r%d-r%d", &x, &y); err != nil || x < 0 || y < 0 || x == y {
			return bad()
		}
		a.A, a.B = x, y
	default:
		if len(f) != 3 {
			return bad()
		}
		if _, err := fmt.Sscanf(f[1], "r%d", &a.A); err != nil || a.A < 0 {
			return bad()
		}
	}
	return a, nil
}
