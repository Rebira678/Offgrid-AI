package sqlstore

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"offgrid/internal/hlc"
	"offgrid/internal/task"
)

// replicaAt opens a fresh replica with its own hand-set clock.
func replicaAt(t *testing.T, node string, start time.Time) (*Store, *clock) {
	t.Helper()
	clk := &clock{t: start}
	return open(t, newDB(t), Options{NodeID: node, Now: clk.now}), clk
}

// pull brings dst up to date with src the way the protocol does: ask what is
// missing, apply it, repeat until nothing is left. Small batches on purpose.
func pull(t *testing.T, dst, src *Store, batch int) ApplyResult {
	t.Helper()
	ctx := context.Background()
	var total ApplyResult
	for {
		have, err := dst.Summary(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ops, more, err := src.OpsSince(ctx, have, batch)
		if err != nil {
			t.Fatal(err)
		}
		r, err := dst.ApplyRemote(ctx, ops)
		if err != nil {
			t.Fatalf("pull %s <- %s: %v", dst.node, src.node, err)
		}
		total.Applied += r.Applied
		total.Duplicates += r.Duplicates
		total.Rebuilt = total.Rebuilt || r.Rebuilt
		if !more {
			return total
		}
	}
}

func hashOf(t *testing.T, s *Store) string {
	t.Helper()
	h, err := s.StateHash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func mustVerify(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.Verify(context.Background()); err != nil {
		t.Fatalf("%s: Verify: %v", s.node, err)
	}
}

func setStatusOf(t *testing.T, s *Store, id string, st task.Status) {
	t.Helper()
	if _, _, err := s.Update(context.Background(), id, task.Changes{Status: &st}); err != nil {
		t.Fatalf("%s: update %s: %v", s.node, id, err)
	}
}

func TestAReplicaThatPullsEndsUpWithTheSameStateAndSummary(t *testing.T) {
	ctx := context.Background()
	a, aclk := replicaAt(t, "node-a", epoch)
	b, _ := replicaAt(t, "node-b", epoch.Add(time.Minute))
	for i := 0; i < 7; i++ {
		aclk.tick(time.Minute)
		create(t, a, fmt.Sprintf("task %d", i))
	}
	r := pull(t, b, a, 3) // several batches
	if r.Applied != 7 || r.Duplicates != 0 {
		t.Fatalf("result = %+v, want 7 applied", r)
	}
	if hashOf(t, a) != hashOf(t, b) {
		t.Fatal("state hashes differ after one pull")
	}
	sa, _ := a.Summary(ctx)
	sb, _ := b.Summary(ctx)
	if fmt.Sprint(sa) != fmt.Sprint(sb) || sa["node-a"] != 7 {
		t.Fatalf("summaries differ: %v vs %v", sa, sb)
	}
	mustVerify(t, b)
}

func TestApplyingTheSameBatchTwiceChangesNothing(t *testing.T) {
	ctx := context.Background()
	a, _ := replicaAt(t, "node-a", epoch)
	b, _ := replicaAt(t, "node-b", epoch)
	create(t, a, "one")
	create(t, a, "two")
	ops, _, _ := a.OpsSince(ctx, Summary{}, 100)
	if r, err := b.ApplyRemote(ctx, ops); err != nil || r.Applied != 2 {
		t.Fatalf("first: %+v %v", r, err)
	}
	h := hashOf(t, b)
	posBefore := countOps(t, b)
	r, err := b.ApplyRemote(ctx, ops)
	if err != nil || r.Applied != 0 || r.Duplicates != 2 {
		t.Fatalf("second: %+v %v, want 0 applied 2 duplicates", r, err)
	}
	if hashOf(t, b) != h || countOps(t, b) != posBefore {
		t.Fatal("a repeated batch changed the replica")
	}
	// the same op twice inside one batch, and a batch overlapping what is held
	create(t, a, "three")
	more, _, _ := a.OpsSince(ctx, Summary{}, 100)
	doubled := append(append([]Op{}, more...), more[2])
	if r, err := b.ApplyRemote(ctx, doubled); err != nil || r.Applied != 1 || r.Duplicates != 2 {
		t.Fatalf("overlap: %+v %v, want 1 applied 2 duplicates", r, err)
	}
	if hashOf(t, a) != hashOf(t, b) {
		t.Fatal("hashes differ")
	}
}

func TestABatchThatSkipsOpsIsRefusedAndNothingIsKept(t *testing.T) {
	ctx := context.Background()
	a, _ := replicaAt(t, "node-a", epoch)
	b, _ := replicaAt(t, "node-b", epoch)
	for i := 0; i < 4; i++ {
		create(t, a, fmt.Sprintf("t%d", i))
	}
	ops, _, _ := a.OpsSince(ctx, Summary{}, 100)
	h, n := hashOf(t, b), countOps(t, b)
	_, err := b.ApplyRemote(ctx, []Op{ops[0], ops[1], ops[3]}) // 3 missing
	if !errors.Is(err, ErrGap) || !strings.Contains(err.Error(), "node-a") {
		t.Fatalf("err = %v, want ErrGap naming node-a", err)
	}
	_, err = b.ApplyRemote(ctx, ops[1:]) // starts at 2, holds nothing
	if !errors.Is(err, ErrGap) {
		t.Fatalf("err = %v, want ErrGap for a batch that starts late", err)
	}
	if hashOf(t, b) != h || countOps(t, b) != n {
		t.Fatal("a refused batch left something behind")
	}
}

func TestSameIdentityDifferentContentIsAForkAndIsRefused(t *testing.T) {
	ctx := context.Background()
	a, _ := replicaAt(t, "node-a", epoch)
	b, _ := replicaAt(t, "node-b", epoch)
	create(t, a, "original")
	ops, _, _ := a.OpsSince(ctx, Summary{}, 10)
	if _, err := b.ApplyRemote(ctx, ops); err != nil {
		t.Fatal(err)
	}
	forged := ops[0]
	forged.Payload = []byte(strings.Replace(string(forged.Payload), "original", "forged", 1))
	if _, err := b.ApplyRemote(ctx, []Op{forged}); !errors.Is(err, ErrFork) {
		t.Fatalf("held op with other content: err = %v, want ErrFork", err)
	}
	// the same op with different JSON whitespace is the same op
	spaced := ops[0]
	spaced.Payload = []byte(strings.Replace(string(spaced.Payload), `","`, `", "`, -1))
	if r, err := b.ApplyRemote(ctx, []Op{spaced}); err != nil || r.Duplicates != 1 {
		t.Fatalf("whitespace-only difference: %+v %v, want a duplicate", r, err)
	}
	other := ops[0]
	other.Counter++
	if _, err := b.ApplyRemote(ctx, []Op{other}); !errors.Is(err, ErrFork) {
		t.Fatalf("different counter: err = %v, want ErrFork", err)
	}
	// two different ops with one identity inside a single batch
	c, _ := replicaAt(t, "node-c", epoch)
	if _, err := c.ApplyRemote(ctx, []Op{ops[0], forged}); !errors.Is(err, ErrFork) {
		t.Fatalf("conflicting pair in one batch: err = %v, want ErrFork", err)
	}
	if countOps(t, c) != 0 {
		t.Fatal("the refused batch left ops behind")
	}
}

func TestMalformedIncomingOpsAreRefused(t *testing.T) {
	ctx := context.Background()
	a, _ := replicaAt(t, "node-a", epoch)
	create(t, a, "x")
	good, _, _ := a.OpsSince(ctx, Summary{}, 10)
	mut := map[string]func(*Op){
		"bad node id":      func(o *Op) { o.NodeID = "no spaces/allowed" },
		"empty node":       func(o *Op) { o.NodeID = "" },
		"seq zero":         func(o *Op) { o.Seq = 0 },
		"zero time":        func(o *Op) { o.TS = time.Time{} },
		"before 1970":      func(o *Op) { o.TS = time.Unix(-5, 0) },
		"unknown kind":     func(o *Op) { o.Kind = "task.exploded" },
		"empty payload":    func(o *Op) { o.Payload = nil },
		"payload not json": func(o *Op) { o.Payload = []byte("{nope") },
		"payload too big":  func(o *Op) { o.Payload = []byte(`"` + strings.Repeat("x", maxRemotePayload) + `"`) },
	}
	for name, f := range mut {
		b, _ := replicaAt(t, "node-b", epoch)
		op := good[0]
		f(&op)
		_, err := b.ApplyRemote(ctx, []Op{op})
		if !errors.Is(err, ErrBadOp) {
			t.Errorf("%s: err = %v, want ErrBadOp", name, err)
		}
		// the refusal happens before anything is tried, and says why
		if want := map[string]string{"unknown kind": "newer version", "payload too big": "too large"}[name]; want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, want)
		}
		if countOps(t, b) != 0 {
			t.Errorf("%s: something was stored", name)
		}
	}
	// well-formed but meaningless: a note on a task nobody has
	b, _ := replicaAt(t, "node-b", epoch)
	bad := Op{NodeID: "node-z", Seq: 1, TS: epoch, Kind: KindNoteAdded, Payload: []byte(`{"id":"T404","text":"hi"}`)}
	if _, err := b.ApplyRemote(ctx, []Op{bad}); !errors.Is(err, ErrBadOp) {
		t.Fatalf("note on a missing task: err = %v, want ErrBadOp", err)
	}
	if countOps(t, b) != 0 || hashOf(t, b) == "" {
		t.Fatal("a refused op was kept")
	}
}

func TestAFarFutureTimestampIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	ctx := context.Background()
	a, _ := replicaAt(t, "node-a", epoch.Add(48*time.Hour)) // a's clock is two days ahead
	b, _ := replicaAt(t, "node-b", epoch)
	create(t, a, "from the future")
	ops, _, _ := a.OpsSince(ctx, Summary{}, 10)
	_, err := b.ApplyRemote(ctx, ops)
	if !errors.Is(err, ErrClockDrift) || !errors.Is(err, hlc.ErrDrift) {
		t.Fatalf("err = %v, want ErrClockDrift wrapping hlc.ErrDrift", err)
	}
	if countOps(t, b) != 0 {
		t.Fatal("ops were stored despite the refusal")
	}
	// with the limit lifted the same batch is accepted
	c := open(t, newDB(t), Options{NodeID: "node-c", Now: (&clock{t: epoch}).now, MaxDrift: hlc.NoDriftLimit})
	if r, err := c.ApplyRemote(ctx, ops); err != nil || r.Applied != 1 {
		t.Fatalf("no limit: %+v %v", r, err)
	}
}

func TestTakingInARemoteOpMovesTheLocalClockPastIt(t *testing.T) {
	ctx := context.Background()
	a, _ := replicaAt(t, "node-a", epoch.Add(30*time.Minute)) // a's clock is ahead of b's
	b, _ := replicaAt(t, "node-b", epoch)                     // b's clock does not move
	create(t, a, "x")
	pull(t, b, a, 10)
	remote := allOps(t, a)[0]
	local := create(t, b, "made after seeing x")
	_ = local
	var mine Op
	for _, op := range allOps(t, b) {
		if op.NodeID == "node-b" {
			mine = op
		}
	}
	if !remote.Stamp().Before(mine.Stamp()) {
		t.Fatalf("b's next op %v is not after the op it had seen %v", mine.Stamp(), remote.Stamp())
	}
	_ = ctx
}

func TestConcurrentEditsOnTwoReplicasConvergeInEitherOrder(t *testing.T) {
	ctx := context.Background()
	a, aclk := replicaAt(t, "node-a", epoch)
	b, bclk := replicaAt(t, "node-b", epoch)
	c, cclk := replicaAt(t, "node-c", epoch)
	create(t, a, "pump")
	pull(t, b, a, 100)
	pull(t, c, a, 100)
	id := "T1"
	// while apart: a and b both edit the same task, b also adds a task, c is idle
	aclk.tick(time.Minute)
	setStatusOf(t, a, id, task.StatusDone)
	bclk.tick(2 * time.Minute)
	setStatusOf(t, b, id, task.StatusInProgress)
	bclk.tick(time.Minute)
	nb := create(t, b, "filter")
	if !strings.HasPrefix(nb.ID, "T2.node-b") {
		t.Fatalf("b's new task ID = %q, want a replica-qualified ID", nb.ID)
	}
	cclk.tick(time.Minute)
	if _, err := c.AddNote(ctx, id, "seal cracked"); err != nil {
		t.Fatal(err)
	}

	// three different orders of exchange
	orders := [][][2]*Store{
		{{a, b}, {b, a}, {c, a}, {a, c}, {b, c}, {c, b}},
		{{c, b}, {a, c}, {b, a}, {a, b}, {c, a}, {b, c}},
		{{b, c}, {c, b}, {a, b}, {b, a}, {a, c}, {c, a}},
	}
	var want string
	for oi, order := range orders {
		x, y, z := cloneStore(t, a, "node-a"), cloneStore(t, b, "node-b"), cloneStore(t, c, "node-c")
		by := map[*Store]*Store{a: x, b: y, c: z}
		for _, p := range order {
			pull(t, by[p[0]], by[p[1]], 2)
		}
		// one more full round so everyone has everything
		for _, p := range [][2]*Store{{x, y}, {y, z}, {z, x}, {x, y}, {y, z}} {
			pull(t, p[0], p[1], 100)
		}
		h := hashOf(t, x)
		if hashOf(t, y) != h || hashOf(t, z) != h {
			t.Fatalf("order %d: replicas disagree", oi)
		}
		if want == "" {
			want = h
		} else if h != want {
			t.Fatalf("order %d reached a different state than order 0", oi)
		}
		for _, s := range []*Store{x, y, z} {
			mustVerify(t, s)
		}
	}
}

// cloneStore copies a replica's database file so one scenario can be replayed
// several ways from the same start.
func cloneStore(t *testing.T, src *Store, node string) *Store {
	t.Helper()
	ctx := context.Background()
	dst := open(t, newDB(t), Options{NodeID: node, Now: (&clock{t: epoch.Add(time.Hour)}).now})
	ops, _, err := src.OpsSince(ctx, Summary{}, 100000)
	if err != nil {
		t.Fatal(err)
	}
	// replay in sequence order; the new replica holds exactly the same ops
	if _, err := dst.ApplyRemote(ctx, ops); err != nil {
		// src's own ops carry its node ID; dst is the same node, so insert directly instead
		t.Fatalf("clone: %v", err)
	}
	return dst
}

func TestAnOpSortingBeforeWhatIsHeldRebuildsTheViewAndFlagsIt(t *testing.T) {
	ctx := context.Background()
	a, aclk := replicaAt(t, "node-a", epoch)
	b, bclk := replicaAt(t, "node-b", epoch)
	create(t, a, "pump")
	pull(t, b, a, 100)
	// b edits at an earlier time than a's later edit, but a has not seen b's yet
	bclk.tick(time.Minute)
	setStatusOf(t, b, "T1", task.StatusInProgress)
	aclk.tick(10 * time.Minute)
	setStatusOf(t, a, "T1", task.StatusDone)
	r := pull(t, a, b, 100)
	if !r.Rebuilt {
		t.Fatal("b's earlier edit sorts before a's later one: the view should have been rebuilt")
	}
	got, _ := a.Get(ctx, "T1")
	if got.Status != task.StatusDone {
		t.Fatalf("status = %s: the later edit (a's) must win", got.Status)
	}
	r = pull(t, b, a, 100)
	if r.Rebuilt {
		t.Fatal("a's edit sorts after everything b holds; no rebuild needed")
	}
	if hashOf(t, a) != hashOf(t, b) {
		t.Fatal("hashes differ")
	}
	mustVerify(t, a)
	mustVerify(t, b)
}

func TestTwoReplicasThatCreatedTasksSeparatelyAreToldAboutTheIDCollision(t *testing.T) {
	ctx := context.Background()
	a, _ := replicaAt(t, "node-a", epoch)
	b, _ := replicaAt(t, "node-b", epoch)
	create(t, a, "pump")
	create(t, b, "something else") // both are now "T1"
	h, n := hashOf(t, b), countOps(t, b)
	ops, _, _ := a.OpsSince(ctx, Summary{}, 10)
	_, err := b.ApplyRemote(ctx, ops)
	if !errors.Is(err, ErrIDCollision) {
		t.Fatalf("err = %v, want ErrIDCollision", err)
	}
	if hashOf(t, b) != h || countOps(t, b) != n {
		t.Fatal("a refused batch changed b")
	}
	mustVerify(t, b)
}

func TestOpsSinceReturnsOnlyWhatIsMissingInOrderAndSaysWhenThereIsMore(t *testing.T) {
	ctx := context.Background()
	a, _ := replicaAt(t, "node-a", epoch)
	b, _ := replicaAt(t, "node-b", epoch)
	for i := 0; i < 5; i++ {
		create(t, a, fmt.Sprintf("a%d", i))
	}
	pull(t, b, a, 100)
	for i := 0; i < 3; i++ {
		create(t, b, fmt.Sprintf("b%d", i))
	}
	ops, more, err := b.OpsSince(ctx, Summary{"node-a": 2}, 4)
	if err != nil || !more || len(ops) != 4 {
		t.Fatalf("got %d ops, more=%v, err=%v; want 4 and more", len(ops), more, err)
	}
	if ops[0].NodeID != "node-a" || ops[0].Seq != 3 || ops[2].Seq != 5 || ops[3].NodeID != "node-b" || ops[3].Seq != 1 {
		t.Fatalf("wrong ops or order: %+v", ops)
	}
	ops, more, _ = b.OpsSince(ctx, Summary{"node-a": 5, "node-b": 1}, 10)
	if more || len(ops) != 2 || ops[0].Seq != 2 {
		t.Fatalf("got %d ops more=%v", len(ops), more)
	}
	ops, more, _ = b.OpsSince(ctx, Summary{"node-a": 5, "node-b": 3, "node-zzz": 9}, 10)
	if more || len(ops) != 0 {
		t.Fatalf("up to date should be empty, got %d", len(ops))
	}
	// exactly limit ops left: more must be false
	ops, more, _ = b.OpsSince(ctx, Summary{"node-a": 5}, 3)
	if more || len(ops) != 3 {
		t.Fatalf("exactly-limit case: %d ops more=%v", len(ops), more)
	}
}

func TestRandomReplicasThatExchangeOpsInRandomOrderAlwaysConverge(t *testing.T) {
	ctx := context.Background()
	for seed := int64(1); seed <= 12; seed++ {
		rng := rand.New(rand.NewSource(seed))
		names := []string{"node-a", "node-b", "node-c"}
		var reps []*Store
		var clks []*clock
		for i, n := range names {
			s, c := replicaAt(t, n, epoch.Add(time.Duration(rng.Intn(120)-60)*time.Second)) // skewed clocks
			_ = i
			reps, clks = append(reps, s), append(clks, c)
		}
		create(t, reps[0], "seed task")
		for _, s := range reps[1:] {
			pull(t, s, reps[0], 100)
		}
		for step := 0; step < 60; step++ {
			i := rng.Intn(3)
			s, c := reps[i], clks[i]
			c.tick(time.Duration(rng.Intn(5)) * time.Second) // sometimes no time passes at all
			switch rng.Intn(6) {
			case 0:
				create(t, s, fmt.Sprintf("t-%d-%d", i, step))
			case 1, 2:
				all, _ := s.List(ctx, task.Filter{})
				tk := all[rng.Intn(len(all))]
				setStatusOf(t, s, tk.ID, statuses[rng.Intn(3)])
			case 3:
				all, _ := s.List(ctx, task.Filter{})
				if _, err := s.AddNote(ctx, all[rng.Intn(len(all))].ID, fmt.Sprintf("note %d", step)); err != nil {
					t.Fatal(err)
				}
			default:
				j := rng.Intn(3)
				if j != i {
					pull(t, s, reps[j], 1+rng.Intn(5))
				}
			}
		}
		for round := 0; round < 2; round++ {
			for i := range reps {
				for j := range reps {
					if i != j {
						pull(t, reps[i], reps[j], 7)
					}
				}
			}
		}
		h := hashOf(t, reps[0])
		for i, s := range reps {
			if hashOf(t, s) != h {
				t.Fatalf("seed %d: replica %d disagrees", seed, i)
			}
			mustVerify(t, s)
		}
		// and the incremental view equals a from-scratch fold of the same log
		if rep, err := reps[1].Rebuild(ctx); err != nil || hashOf(t, reps[1]) != h {
			t.Fatalf("seed %d: rebuild changed the state (%v, %+v)", seed, err, rep)
		}
	}
}
