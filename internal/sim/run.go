package sim

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"time"

	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
)

// Config sets the size of a run. The zero value is the default.
type Config struct {
	Replicas int    // default 3
	Steps    int    // actions per schedule, default 80
	Dir      string // where the replicas' files go; default /dev/shm if present, else the temp dir
	Keep     bool   // keep the files of a failing run
	// Shared starts the replicas from common history: r0 holds a first task and
	// the others begin by pulling it. By default every replica starts from an
	// empty database and nobody has met anybody, which is the harder case (and
	// the one that exposed the Day 24 bug).
	Shared bool
}

func (c Config) withDefaults() Config {
	if c.Replicas < 2 {
		c.Replicas = 3
	}
	if c.Steps <= 0 {
		c.Steps = 80
	}
	if c.Dir == "" {
		c.Dir = os.TempDir()
		if st, err := os.Stat("/dev/shm"); err == nil && st.IsDir() {
			c.Dir = "/dev/shm"
		}
	}
	return c
}

// Failure says what went wrong. Class is short and stable (it is what Shrink
// keeps equal while it cuts), Msg has the detail.
type Failure struct {
	Class string
	Msg   string
}

func (f *Failure) Error() string { return f.Class + ": " + f.Msg }

// Stats counts what a run did, so "passed" can be checked against "was
// actually hit by faults".
type Stats struct {
	Edits, Syncs, Partitions, Blocked, Skews, Crashes int
	Dropped, Duplicated, Delayed, Truncated           int
	DriftRefusals, Rebuilds                           int
	Reversed, Gapped, GapRefused                      int
}

// Result is everything a run produced.
type Result struct {
	Seed    int64
	Actions []Action
	Trace   []string
	Failure *Failure // nil when the replicas converged and every invariant held
	Hash    string   // the agreed state hash (empty on failure before agreement)
	Ops     int      // ops in each converged log
	Stats   Stats
}

// Run generates the schedule for seed and executes it.
func Run(seed int64, cfg Config) Result {
	cfg = cfg.withDefaults()
	return RunActions(seed, Generate(seed, cfg), cfg)
}

// RunActions executes a given schedule (used by Shrink and by replay). The
// seed only names the run and seeds the final order-independence check.
func RunActions(seed int64, acts []Action, cfg Config) Result {
	cfg = cfg.withDefaults()
	w := &world{cfg: cfg, seed: seed, now: epoch, off: make([]time.Duration, cfg.Replicas), down: map[[2]int]bool{}}
	res := Result{Seed: seed, Actions: acts}
	dir, err := os.MkdirTemp(cfg.Dir, "offgrid-sim-")
	if err != nil {
		res.Failure = &Failure{"setup", err.Error()}
		return res
	}
	w.dir = dir
	defer func() {
		for _, s := range w.st {
			if s != nil {
				s.Close()
			}
		}
		if res.Failure == nil || !cfg.Keep {
			os.RemoveAll(dir)
		}
	}()

	if f := w.start(); f != nil {
		res.Failure = f
	} else {
		for i, a := range acts {
			w.step = i + 1
			if f := w.do(a); f != nil {
				res.Failure = f
				break
			}
		}
		if res.Failure == nil {
			res.Failure = w.settle()
		}
	}
	if res.Failure != nil && cfg.Keep {
		w.log("files kept in %s", dir)
	}
	res.Trace, res.Stats, res.Hash, res.Ops = w.trace, w.stats, w.hash, w.ops
	return res
}

var epoch = time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)

type packet struct {
	to  int
	ops []sqlstore.Op
}

type world struct {
	cfg  Config
	seed int64
	dir  string
	step int
	now  time.Time       // true time
	off  []time.Duration // each replica's clock error
	st   []*sqlstore.Store
	down map[[2]int]bool
	late []packet // delayed batches still in flight

	trace []string
	stats Stats
	hash  string
	ops   int
	nth   int // counter for unique note text and suggestion keys

	applied int // ops newly applied by pulls (settle runs until a whole round applies none)
}

func (w *world) log(format string, args ...any) {
	w.trace = append(w.trace, fmt.Sprintf("%03d  ", w.step)+fmt.Sprintf(format, args...))
}

func (w *world) clockOf(i int) func() time.Time {
	return func() time.Time { return w.now.Add(w.off[i]) }
}

func nodeName(i int) string { return fmt.Sprintf("r%d", i) }
func (w *world) path(i int) string {
	return filepath.Join(w.dir, nodeName(i)+".db")
}

func (w *world) open(i int) (*sqlstore.Store, error) {
	return sqlstore.Open(context.Background(), w.path(i), sqlstore.Options{NodeID: nodeName(i), Now: w.clockOf(i)})
}

func (w *world) start() *Failure {
	for i := 0; i < w.cfg.Replicas; i++ {
		s, err := w.open(i)
		if err != nil {
			return &Failure{"setup", err.Error()}
		}
		w.st = append(w.st, s)
	}
	if !w.cfg.Shared {
		w.log("start: %d empty replicas, no shared history", w.cfg.Replicas)
		return nil
	}
	ctx := context.Background()
	if _, _, err := w.st[0].Create(ctx, task.NewTask{Title: "first task", Site: "plant", Priority: task.PriorityMedium}); err != nil {
		return &Failure{"setup", err.Error()}
	}
	for i := 1; i < w.cfg.Replicas; i++ {
		if _, err := w.pull(i, 0, 5, faultNone); err != nil {
			return &Failure{"setup", err.Error()}
		}
	}
	w.log("start: r0 holds a first task, the others pulled it")
	return nil
}

type fault int

const (
	faultNone fault = iota
	faultDrop
	faultDup
	faultLate
	faultCut
	faultGap     // the first op of the reply is lost: what arrives skips something
	faultReverse // the reply arrives in the opposite order
)

// notAtomic is a refused batch that nevertheless changed the replica.
type notAtomic struct{ msg string }

func (e *notAtomic) Error() string { return e.msg }

// syncFailure classifies an error from taking in a batch.
func syncFailure(prefix string, err error) *Failure {
	var na *notAtomic
	if errors.As(err, &na) {
		return &Failure{"atomic", prefix + ": " + na.msg}
	}
	return &Failure{"sync", prefix + ": " + err.Error()}
}

// pull is one request-and-reply: dst asks src for what it is missing (at most
// batch ops) and applies the reply, with the given fault done to the reply.
func (w *world) pull(dst, src, batch int, f fault) (more bool, err error) {
	ctx := context.Background()
	have, err := w.st[dst].Summary(ctx)
	if err != nil {
		return false, err
	}
	ops, more, err := w.st[src].OpsSince(ctx, have, batch)
	if err != nil {
		return false, err
	}
	var gapHash string
	switch f {
	case faultDrop:
		w.stats.Dropped++
		return true, nil
	case faultLate:
		w.stats.Delayed++
		w.late = append(w.late, packet{to: dst, ops: ops})
		return true, nil
	case faultCut:
		if len(ops) > 1 {
			w.stats.Truncated++
			ops = ops[:1+len(ops)/2]
			more = true
		}
	case faultReverse:
		w.stats.Reversed++
		rev := make([]sqlstore.Op, len(ops))
		for i, op := range ops {
			rev[len(ops)-1-i] = op
		}
		ops = rev
	case faultGap:
		if len(ops) > 1 {
			w.stats.Gapped++
			ops = ops[1:]
			var herr error
			if gapHash, herr = w.st[dst].StateHash(ctx); herr != nil {
				return false, herr
			}
		}
	}
	r, err := w.st[dst].ApplyRemote(ctx, ops)
	if err != nil {
		// A refused batch must leave the replica exactly as it was.
		if after, serr := w.st[dst].Summary(ctx); serr == nil && fmt.Sprint(after) != fmt.Sprint(have) {
			return more, &notAtomic{fmt.Sprintf("a refused batch (%v) changed what %s holds: %v -> %v", err, nodeName(dst), have, after)}
		}
		if gapHash != "" {
			if h, _ := w.st[dst].StateHash(ctx); h != gapHash {
				return more, &notAtomic{fmt.Sprintf("a refused batch (%v) changed the state of %s", err, nodeName(dst))}
			}
			if errors.Is(err, sqlstore.ErrGap) || errors.Is(err, sqlstore.ErrBadOp) {
				w.stats.GapRefused++
				return true, nil // the right answer to a reply with a hole in it
			}
		}
		return more, err
	}
	w.applied += r.Applied
	if r.Rebuilt {
		w.stats.Rebuilds++
	}
	if f == faultDup {
		w.stats.Duplicated++
		if _, err := w.st[dst].ApplyRemote(ctx, ops); err != nil {
			return more, fmt.Errorf("duplicate delivery: %w", err)
		}
	}
	return more, nil
}

// benign says an error is a normal refusal of a person's command, not a bug.
func benign(err error) bool {
	for _, e := range []error{task.ErrNotFound, task.ErrAmbiguous, task.ErrInvalid, task.ErrStale,
		task.ErrSuggestionNotFound, task.ErrSuggestionResolved, task.ErrNothingToSuggest} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

func (w *world) pickTask(i, n int) (task.Task, bool) {
	ts, err := w.st[i].List(context.Background(), task.Filter{})
	if err != nil || len(ts) == 0 {
		return task.Task{}, false
	}
	return ts[n%len(ts)], true
}

// orDash prints what a pointer points at ("-" for nil). Printing the pointer
// itself would put a memory address in the trace and make two runs of one seed
// differ.
func orDash[T any](p *T) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprint(*p)
}

var statuses = []task.Status{task.StatusOpen, task.StatusInProgress, task.StatusDone}
var priorities = []task.Priority{task.PriorityLow, task.PriorityMedium, task.PriorityHigh}

// do runs one action and then checks the one promise that can be checked at
// once, not only at the end: causality. An edit a replica makes is stamped
// later than every op that replica held when it made it (that is what the
// hybrid clock is for), so the new op must be the last one in fold order.
func (w *world) do(a Action) *Failure {
	edit := isEdit(a.Kind)
	var before int64
	if edit {
		before = w.ownSeq(a.A)
	}
	if f := w.act(a); f != nil {
		return f
	}
	if edit {
		if after := w.ownSeq(a.A); after > before {
			ops, _, err := w.st[a.A].OpsSince(context.Background(), sqlstore.Summary{}, 1<<30)
			if err != nil {
				return &Failure{"causality", err.Error()}
			}
			last := ops[len(ops)-1]
			if last.NodeID != nodeName(a.A) || last.Seq != after {
				return &Failure{"causality", fmt.Sprintf("step %d (%s): %s's new op %d sorts before %s/%d, which it already held: a later edit would lose to an earlier one",
					w.step, a, nodeName(a.A), after, last.NodeID, last.Seq)}
			}
		}
	}
	return nil
}

func (w *world) ownSeq(i int) int64 {
	sum, err := w.st[i].Summary(context.Background())
	if err != nil {
		return -1
	}
	return sum[nodeName(i)]
}

func (w *world) act(a Action) *Failure {
	ctx := context.Background()
	w.now = w.now.Add(time.Second) // every action takes a second
	s := w.st[a.A]
	who := nodeName(a.A)
	fail := func(class string, err error) *Failure {
		return &Failure{class, fmt.Sprintf("step %d (%s): %v", w.step, a, err)}
	}
	w.nth++
	switch a.Kind {
	case Create:
		title := fmt.Sprintf("job %d", a.N%6) // few titles, so replicas collide on purpose
		t, created, err := s.Create(ctx, task.NewTask{Title: title, Site: "plant", Priority: priorities[a.N/7%3]})
		if err != nil && !benign(err) {
			return fail("edit", err)
		}
		w.stats.Edits++
		w.log("%s create %q -> %s created=%v err=%v", who, title, t.ID, created, err)
	case Update:
		t, ok := w.pickTask(a.A, a.N)
		if !ok {
			w.log("%s update: nothing to update", who)
			return nil
		}
		st, pr := statuses[a.N/3%3], priorities[a.N/11%3]
		c := task.Changes{}
		switch a.N % 3 {
		case 0:
			c.Status = &st
		case 1:
			c.Priority = &pr
		default:
			c.Status, c.Priority = &st, &pr
		}
		_, changed, err := s.Update(ctx, t.ID, c)
		if err != nil && !benign(err) {
			return fail("edit", err)
		}
		w.stats.Edits++
		w.log("%s update %s status=%s priority=%s changed=%v err=%v", who, t.ID, orDash(c.Status), orDash(c.Priority), changed, err)
	case Note:
		t, ok := w.pickTask(a.A, a.N)
		if !ok {
			w.log("%s note: nothing to annotate", who)
			return nil
		}
		_, err := s.AddNote(ctx, t.ID, fmt.Sprintf("note %d", w.nth))
		if err != nil && !benign(err) {
			return fail("edit", err)
		}
		w.stats.Edits++
		w.log("%s note on %s err=%v", who, t.ID, err)
	case Delete:
		t, ok := w.pickTask(a.A, a.N)
		if !ok {
			w.log("%s delete: nothing to delete", who)
			return nil
		}
		err := s.DeleteTask(ctx, t.ID)
		if err != nil && !benign(err) {
			return fail("edit", err)
		}
		w.stats.Edits++
		w.log("%s delete %s err=%v", who, t.ID, err)
	case Suggest:
		t, ok := w.pickTask(a.A, a.N)
		if !ok {
			w.log("%s suggest: no task", who)
			return nil
		}
		pr := priorities[a.N/5%3]
		d := task.Draft{Task: t.ID, By: "ai", Key: fmt.Sprintf("k%d-%d", a.A, w.nth)}
		if a.N%2 == 0 {
			d.Priority = &pr
		} else {
			d.Note = fmt.Sprintf("idea %d", w.nth)
		}
		_, _, err := s.Suggest(ctx, d)
		if err != nil && !benign(err) {
			return fail("edit", err)
		}
		w.stats.Edits++
		w.log("%s suggest on %s err=%v", who, t.ID, err)
	case Resolve:
		sug, err := s.Suggestions(ctx, false)
		if err != nil {
			return fail("edit", err)
		}
		if len(sug) == 0 {
			w.log("%s resolve: nothing pending", who)
			return nil
		}
		x := sug[a.N%len(sug)]
		accept := a.N/13%2 == 0
		_, err = s.Resolve(ctx, x.ID, accept)
		if err != nil && !benign(err) {
			return fail("edit", err)
		}
		w.stats.Edits++
		w.log("%s resolve %s accept=%v err=%v", who, x.ID, accept, err)
	case Skew:
		w.stats.Skews++
		w.off[a.A] = time.Duration(a.N%181-90) * time.Minute
		w.log("%s clock error is now %v", who, w.off[a.A])
	case Crash:
		w.stats.Crashes++
		if err := s.Close(); err != nil {
			return fail("crash", err)
		}
		reset := ""
		if a.N%3 == 0 { // the battery died: the clock restarts far in the past
			w.off[a.A] = -time.Duration(1+a.N%40) * time.Hour
			reset = fmt.Sprintf(", clock reset to %v", w.off[a.A])
		}
		ns, err := w.open(a.A)
		if err != nil {
			return fail("crash", err)
		}
		w.st[a.A] = ns
		w.log("%s crashed and restarted%s", who, reset)
	case Partition:
		w.stats.Partitions++
		w.down[link(a.A, a.B)] = true
		w.log("link %s-%s down", who, nodeName(a.B))
	case Heal:
		delete(w.down, link(a.A, a.B))
		w.log("link %s-%s up", who, nodeName(a.B))
	case Wait:
		d := time.Duration(1+a.N%900) * time.Second
		w.now = w.now.Add(d)
		w.log("time passes: %v", d)
	case Sync:
		return w.doSync(a)
	}
	return nil
}

func link(a, b int) [2]int {
	if a > b {
		a, b = b, a
	}
	return [2]int{a, b}
}

func (w *world) doSync(a Action) *Failure {
	w.stats.Syncs++
	who, peer := nodeName(a.A), nodeName(a.B)
	// Batches that were delayed earlier arrive now, whatever the link says
	// today: they were already on the wire.
	keep := w.late[:0]
	for _, p := range w.late {
		if p.to != a.A {
			keep = append(keep, p)
			continue
		}
		if _, err := w.st[p.to].ApplyRemote(context.Background(), p.ops); err != nil {
			if errors.Is(err, sqlstore.ErrClockDrift) {
				w.stats.DriftRefusals++
				w.log("%s refused a late batch: clock drift", who)
				continue
			}
			return syncFailure(fmt.Sprintf("step %d: late batch to %s", w.step, who), err)
		}
		w.log("%s took in a late batch of %d ops", who, len(p.ops))
	}
	w.late = keep

	if w.down[link(a.A, a.B)] {
		w.stats.Blocked++
		w.log("%s <- %s blocked by partition", who, peer)
		return nil
	}
	batch := 1 + a.N%7
	f := []fault{faultNone, faultNone, faultNone, faultNone, faultDrop, faultDup, faultLate, faultCut, faultGap, faultReverse}[a.N/7%10]
	rounds := 1 + a.N/56%4
	for r := 0; r < rounds; r++ {
		more, err := w.pull(a.A, a.B, batch, f)
		if err != nil {
			if errors.Is(err, sqlstore.ErrClockDrift) {
				w.stats.DriftRefusals++
				w.log("%s <- %s refused: clock drift", who, peer)
				return nil
			}
			return syncFailure(fmt.Sprintf("step %d (%s)", w.step, a), err)
		}
		if !more || f == faultDrop || f == faultLate {
			break
		}
	}
	w.log("%s <- %s batch=%d fault=%d rounds<=%d ok", who, peer, batch, f, rounds)
	return nil
}

// settle heals everything and checks the promises: the replicas end up the
// same, each one's view matches its own log, rebuilding changes nothing, and a
// fresh replica that is given the same ops in a different order lands on the
// same state.
func (w *world) settle() *Failure {
	ctx := context.Background()
	w.step++
	w.down = map[[2]int]bool{}
	w.now = w.now.Add(4 * time.Hour) // clocks that ran ahead have been passed by real time
	for i := range w.off {
		w.off[i] = 0 // and every clock that was wrong has been set right
	}
	w.log("settle: all links up, 4h later, clocks corrected, late batches delivered, reliable sync until quiet")
	for _, p := range w.late {
		if _, err := w.st[p.to].ApplyRemote(ctx, p.ops); err != nil {
			return &Failure{"settle", fmt.Sprintf("late batch to %s: %v", nodeName(p.to), err)}
		}
	}
	w.late = nil
	n := len(w.st)
	for round := 0; ; round++ {
		if round == 8 {
			return &Failure{"settle", "the replicas were still exchanging ops after 8 full rounds"}
		}
		before := w.applied
		for i := 0; i < n; i++ {
			for j := 0; j < n; j++ {
				if i == j {
					continue
				}
				for {
					more, err := w.pull(i, j, 16, faultNone)
					if err != nil {
						f := syncFailure(fmt.Sprintf("%s <- %s", nodeName(i), nodeName(j)), err)
						if f.Class == "sync" {
							f.Class = "settle"
						}
						return f
					}
					if !more {
						break
					}
				}
			}
		}
		if w.applied == before {
			break // a whole round of reliable pulls had nothing left to carry
		}
	}

	sums := make([]sqlstore.Summary, n)
	hashes := make([]string, n)
	for i, s := range w.st {
		var err error
		if sums[i], err = s.Summary(ctx); err != nil {
			return &Failure{"settle", err.Error()}
		}
		if hashes[i], err = s.StateHash(ctx); err != nil {
			return &Failure{"settle", err.Error()}
		}
	}
	for i := 1; i < n; i++ {
		if fmt.Sprint(sums[i]) != fmt.Sprint(sums[0]) {
			return &Failure{"ops-differ", fmt.Sprintf("r0 holds %v but r%d holds %v", sums[0], i, sums[i])}
		}
	}
	for i := 1; i < n; i++ {
		if hashes[i] != hashes[0] {
			return &Failure{"diverged", fmt.Sprintf("same ops %v, different state: r0 %s, r%d %s", sums[0], hashes[0][:12], i, hashes[i][:12])}
		}
	}
	for i, s := range w.st {
		rep, err := s.Verify(ctx)
		if err != nil {
			return &Failure{"verify", fmt.Sprintf("r%d: %v", i, err)}
		}
		w.ops = rep.Ops
		if _, err := s.Rebuild(ctx); err != nil {
			return &Failure{"rebuild", fmt.Sprintf("r%d: %v", i, err)}
		}
		if h, err := s.StateHash(ctx); err != nil || h != hashes[0] {
			return &Failure{"rebuild", fmt.Sprintf("r%d: rebuilding the view changed the state (%v)", i, err)}
		}
	}
	if f := w.witness(hashes[0]); f != nil {
		return f
	}
	all, _, err := w.st[0].OpsSince(ctx, sqlstore.Summary{}, 1<<30)
	if err != nil {
		return &Failure{"oracle", err.Error()}
	}
	if f := checkAgainstTheLog(ctx, w.st[0], all); f != nil {
		return f
	}
	w.hash = hashes[0]
	return nil
}

// witness builds one more replica from nothing, feeds it every op in an order
// of its own (each replica's ops stay in sequence, but replicas are
// interleaved at random and in small batches), and requires the same state. It
// checks that the agreed state is a function of the set of ops, not of the
// history of who synced with whom.
func (w *world) witness(want string) *Failure {
	ctx := context.Background()
	src := w.st[0]
	ops, _, err := src.OpsSince(ctx, sqlstore.Summary{}, 1<<30)
	if err != nil {
		return &Failure{"witness", err.Error()}
	}
	by := map[string][]sqlstore.Op{}
	var nodes []string
	for _, op := range ops {
		if _, ok := by[op.NodeID]; !ok {
			nodes = append(nodes, op.NodeID)
		}
		by[op.NodeID] = append(by[op.NodeID], op)
	}
	sort.Strings(nodes)
	for _, n := range nodes {
		sort.Slice(by[n], func(i, j int) bool { return by[n][i].Seq < by[n][j].Seq })
	}
	rng := rand.New(rand.NewSource(w.seed ^ 0x5eed))
	wit, err := sqlstore.Open(ctx, filepath.Join(w.dir, "witness.db"), sqlstore.Options{NodeID: "witness", Now: func() time.Time { return w.now.Add(24 * time.Hour) }})
	if err != nil {
		return &Failure{"witness", err.Error()}
	}
	defer wit.Close()
	for {
		var live []string
		for _, n := range nodes {
			if len(by[n]) > 0 {
				live = append(live, n)
			}
		}
		if len(live) == 0 {
			break
		}
		rng.Shuffle(len(live), func(i, j int) { live[i], live[j] = live[j], live[i] })
		progressed := false
		for _, n := range live {
			k := 1 + rng.Intn(3)
			if k > len(by[n]) {
				k = len(by[n])
			}
			var err error
			for ; k >= 1; k-- { // a longer run may reach an op that needs another replica's; a shorter one may not
				if _, err = wit.ApplyRemote(ctx, by[n][:k]); err == nil {
					break
				}
				if !errors.Is(err, sqlstore.ErrBadOp) {
					return &Failure{"witness", fmt.Sprintf("a fresh replica refused ops from %s: %v", n, err)}
				}
			}
			if err == nil {
				by[n] = by[n][k:]
				progressed = true
				break
			}
			// The first op itself needs something another replica holds. The
			// protocol never sends it first, and the store refuses it,
			// atomically, if it arrives first. Try another replica.
		}
		if !progressed {
			return &Failure{"witness", fmt.Sprintf("a fresh replica could not take any next op, with %d replicas still holding ops", len(live))}
		}
	}
	got, err := wit.StateHash(ctx)
	if err != nil {
		return &Failure{"witness", err.Error()}
	}
	if got != want {
		return &Failure{"witness", fmt.Sprintf("a fresh replica fed the same %d ops in another order reached %s, not %s", len(ops), got[:12], want[:12])}
	}
	return nil
}

func isEdit(k Kind) bool {
	switch k {
	case Create, Update, Note, Delete, Suggest, Resolve:
		return true
	}
	return false
}
