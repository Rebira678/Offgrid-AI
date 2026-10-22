package sqlstore

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"pgregory.net/rapid"

	"offgrid/internal/crdt"
	"offgrid/internal/task"
)

func fieldsOf(t *testing.T, s *Store) crdt.Map {
	t.Helper()
	m, err := s.Fields(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// The registers are a second reading of the same log. For every task, they
// must say exactly what the materialized view says.
func TestFieldRegistersAgreeWithTheMaterializedViewOnRandomWorkloads(t *testing.T) {
	for seed := int64(1); seed <= 12; seed++ {
		s, clk := freshStore(t)
		workload(t, s, clk, seed, 120)
		got := crdt.TaskFields(fieldsOf(t, s))
		tasks, err := s.List(context.Background(), task.Filter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(tasks) {
			t.Fatalf("seed %d: %d tasks in registers, %d in the view", seed, len(got), len(tasks))
		}
		for _, tk := range tasks {
			want := map[string]string{"title": tk.Title, "site": tk.Site, "priority": string(tk.Priority), "status": string(tk.Status)}
			if !reflect.DeepEqual(got[tk.ID], want) {
				t.Fatalf("seed %d, %s: registers %v, view %v", seed, tk.ID, got[tk.ID], want)
			}
		}
	}
}

func TestFieldsReadsTheWholeLogAcrossPages(t *testing.T) {
	s, clk := freshStore(t)
	workload(t, s, clk, 3, 60)
	want, err := FieldsFromOps(allOps(t, s))
	if err != nil {
		t.Fatal(err)
	}
	old := fieldsPageSize
	defer func() { fieldsPageSize = old }()
	for _, size := range []int{1, 3, 7, 1000} {
		fieldsPageSize = size
		if got := fieldsOf(t, s); !crdt.Equal(got, want) {
			t.Errorf("page size %d: registers differ", size)
		}
	}
}

func TestFieldsFromOpsDoesNotDependOnTheOrderOpsArePresented(t *testing.T) {
	s, clk := freshStore(t)
	workload(t, s, clk, 7, 80)
	ops := allOps(t, s)
	want, err := FieldsFromOps(ops)
	if err != nil {
		t.Fatal(err)
	}
	rev := make([]Op, len(ops))
	for i, o := range ops {
		rev[len(ops)-1-i] = o
	}
	got, err := FieldsFromOps(rev)
	if err != nil {
		t.Fatal(err)
	}
	if !crdt.Equal(got, want) {
		t.Error("reversing the ops changed the registers")
	}
}

func TestFieldRegistersCarryTheHLCStampAndNodeOfTheWritingOp(t *testing.T) {
	s, clk := freshStore(t) // frozen clock: the counter does the ordering
	tk := create(t, s, "Pump")
	hi := task.PriorityHigh
	if _, _, err := s.Update(context.Background(), tk.ID, task.Changes{Priority: &hi}); err != nil {
		t.Fatal(err)
	}
	_ = clk
	ops := allOps(t, s)
	m := fieldsOf(t, s)
	pri := m[crdt.TaskKey(tk.ID, crdt.FieldPriority)]
	if pri.Value != "high" || pri.Node != "node-a" || pri.Stamp != ops[1].Stamp() {
		t.Errorf("priority register %+v, update op stamp %v", pri, ops[1].Stamp())
	}
	title := m[crdt.TaskKey(tk.ID, crdt.FieldTitle)]
	if title.Stamp != ops[0].Stamp() || !pri.Stamp.After(title.Stamp) {
		t.Errorf("title %+v should carry the create op's stamp %v", title, ops[0].Stamp())
	}
}

func TestNotesAreNotRegistersAndAnUnknownOpKindIsACorruptLog(t *testing.T) {
	s, _ := freshStore(t)
	tk := create(t, s, "Pump")
	before := fieldsOf(t, s)
	if _, err := s.AddNote(context.Background(), tk.ID, "seal cracked"); err != nil {
		t.Fatal(err)
	}
	if !crdt.Equal(before, fieldsOf(t, s)) {
		t.Error("adding a note changed the registers")
	}
	_, err := FieldsFromOps([]Op{{Pos: 1, Kind: "task.teleported"}})
	if err == nil || !errorsIs(err, ErrCorruptLog) {
		t.Errorf("unknown kind: %v", err)
	}
	_, err = FieldsFromOps([]Op{{Pos: 1, Kind: KindTaskCreated, Payload: []byte("{")}})
	if err == nil || !errorsIs(err, ErrCorruptLog) {
		t.Errorf("bad created payload: %v", err)
	}
	_, err = FieldsFromOps([]Op{{Pos: 1, Kind: KindTaskUpdated, Payload: []byte("[")}})
	if err == nil || !errorsIs(err, ErrCorruptLog) {
		t.Errorf("bad updated payload: %v", err)
	}
}

// ---- two replicas edit the same field while apart

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
}

// fork makes a second replica from a closed database file: the same history,
// then its own node ID, as if it had received everything up to now.
func fork(t *testing.T, path, node string, now time.Time) *Store {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "offgrid.db")
	copyFile(t, path, dst)
	clk := &clock{t: now}
	s := open(t, dst, Options{Now: clk.now})
	if err := setMeta(context.Background(), s.db, "node_id", node); err != nil {
		t.Fatal(err)
	}
	s.node = node
	return s
}

// originWithOneTask writes a history of one task, closes the database, and
// returns its path and the task's ID.
func originWithOneTask(t *testing.T) (path, id string) {
	t.Helper()
	path = newDB(t)
	clk := &clock{t: epoch}
	origin := open(t, path, Options{NodeID: "origin", Now: clk.now})
	tk := create(t, origin, "Leaking pump")
	if err := origin.Close(); err != nil {
		t.Fatal(err)
	}
	return path, tk.ID
}

func setPriority(t *testing.T, s *Store, id string, p task.Priority) {
	t.Helper()
	if _, _, err := s.Update(context.Background(), id, task.Changes{Priority: &p}); err != nil {
		t.Fatal(err)
	}
}

func setStatus(t *testing.T, s *Store, id string, st task.Status) {
	t.Helper()
	if _, _, err := s.Update(context.Background(), id, task.Changes{Status: &st}); err != nil {
		t.Fatal(err)
	}
}

func replicas(t *testing.T, nowA, nowB time.Time) (a, b *Store, id string) {
	t.Helper()
	path, id := originWithOneTask(t)
	return fork(t, path, "node-a", nowA), fork(t, path, "node-b", nowB), id
}

func priorityOf(m crdt.Map, id string) string {
	v, _ := m.Get(crdt.TaskKey(id, crdt.FieldPriority))
	return v
}

func TestConcurrentEditsToTheSameFieldOnTwoReplicasConverge(t *testing.T) {
	a, b, id := replicas(t, epoch.Add(time.Minute), epoch.Add(2*time.Minute))
	setPriority(t, a, id, task.PriorityLow)  // earlier
	setPriority(t, b, id, task.PriorityHigh) // later
	fa, fb := fieldsOf(t, a), fieldsOf(t, b)
	if priorityOf(fa, id) != "low" || priorityOf(fb, id) != "high" {
		t.Fatalf("before merging each replica sees its own edit: %q %q", priorityOf(fa, id), priorityOf(fb, id))
	}
	ab, ba := crdt.Merge(fa, fb), crdt.Merge(fb, fa)
	if !crdt.Equal(ab, ba) {
		t.Fatal("merge order changed the result")
	}
	if got := priorityOf(ab, id); got != "high" {
		t.Errorf("the later edit (node-b) should win, got %q", got)
	}
}

func TestEditsToDifferentFieldsOnTwoReplicasAreBothKept(t *testing.T) {
	a, b, id := replicas(t, epoch.Add(time.Minute), epoch.Add(2*time.Minute))
	setPriority(t, a, id, task.PriorityHigh)
	setStatus(t, b, id, task.StatusDone)
	m := crdt.Merge(fieldsOf(t, a), fieldsOf(t, b))
	f := crdt.TaskFields(m)[id]
	if f["priority"] != "high" || f["status"] != "done" {
		t.Errorf("both edits should survive: %v", f)
	}
}

// Both replicas stamp the same wall time and counter. Only the node ID can
// decide, and it decides the same way whichever side merges.
func TestAnExactStampTieBetweenReplicasIsDecidedByNodeID(t *testing.T) {
	a, b, id := replicas(t, epoch.Add(time.Minute), epoch.Add(time.Minute))
	setPriority(t, a, id, task.PriorityLow)
	setPriority(t, b, id, task.PriorityHigh)
	fa, fb := fieldsOf(t, a), fieldsOf(t, b)
	ea, eb := fa[crdt.TaskKey(id, crdt.FieldPriority)], fb[crdt.TaskKey(id, crdt.FieldPriority)]
	if ea.Stamp != eb.Stamp {
		t.Fatalf("the test needs equal stamps: %v %v", ea.Stamp, eb.Stamp)
	}
	for name, m := range map[string]crdt.Map{"a+b": crdt.Merge(fa, fb), "b+a": crdt.Merge(fb, fa)} {
		if got := priorityOf(m, id); got != "high" {
			t.Errorf("%s: %q, want high (node-b sorts after node-a)", name, got)
		}
	}
}

func TestAReplicaWithASlowClockLosesToTheOlderEditAndTheDocsSayWhy(t *testing.T) {
	// B's phone is 10 minutes behind, and nothing has been exchanged yet. Its
	// edit gets the earlier stamp, so it loses. This is the behaviour the HLC
	// cannot fix by itself: only receiving A's stamp first (sync, Day 19)
	// moves B's clock past it. The test pins the current, honest behaviour.
	a, b, id := replicas(t, epoch.Add(time.Minute), epoch.Add(-10*time.Minute))
	setPriority(t, a, id, task.PriorityLow)
	setPriority(t, b, id, task.PriorityHigh) // really later, stamped earlier
	m := crdt.Merge(fieldsOf(t, a), fieldsOf(t, b))
	if got := priorityOf(m, id); got != "low" {
		t.Errorf("got %q", got)
	}
	// After B learns A's stamp, B's next edit is ordered after it and wins.
	e := fieldsOf(t, a)[crdt.TaskKey(id, crdt.FieldPriority)]
	if _, err := b.Clock().Update(e.Stamp); err != nil {
		t.Fatal(err)
	}
	setPriority(t, b, id, task.PriorityMedium)
	m = crdt.Merge(fieldsOf(t, a), fieldsOf(t, b))
	if got := priorityOf(m, id); got != "medium" {
		t.Errorf("after receiving A's stamp, B's edit should win: %q", got)
	}
}

func errorsIs(err, target error) bool { return errors.Is(err, target) }

// ---- properties: the registers behave like the merge says they should

// buildOpLists makes a few real op logs from random workloads, once per test,
// so the properties below can draw subsets of them thousands of times cheaply.
func buildOpLists(t *testing.T) [][]Op {
	t.Helper()
	var out [][]Op
	for seed := int64(1); seed <= 6; seed++ {
		s, clk := freshStore(t)
		workload(t, s, clk, seed, 60)
		out = append(out, allOps(t, s))
	}
	return out
}

// Reading a log in a different order, with some ops delivered twice, gives the
// same registers. (Sync will deliver ops late, early and more than once.)
func TestPropertyFieldsDoNotDependOnOrderOrDuplicates(t *testing.T) {
	lists := buildOpLists(t)
	rapid.Check(t, func(rt *rapid.T) {
		ops := lists[rapid.IntRange(0, len(lists)-1).Draw(rt, "log")]
		want, err := FieldsFromOps(ops)
		if err != nil {
			rt.Fatal(err)
		}
		shuffled := rapid.Permutation(ops).Draw(rt, "order")
		for _, i := range rapid.SliceOfN(rapid.IntRange(0, len(ops)-1), 0, 20).Draw(rt, "delivered twice") {
			shuffled = append(shuffled, ops[i])
		}
		got, err := FieldsFromOps(shuffled)
		if err != nil {
			rt.Fatal(err)
		}
		if !crdt.Equal(got, want) {
			rt.Fatalf("a different delivery order or duplicates changed the registers")
		}
	})
}

// The property that makes sync possible: registers built from two partial
// logs, merged, equal the registers built from everything. Two replicas can
// therefore exchange registers or ops and reach the same state.
func TestPropertyMergingPartialRegistersEqualsRegistersOfTheUnion(t *testing.T) {
	lists := buildOpLists(t)
	rapid.Check(t, func(rt *rapid.T) {
		ops := lists[rapid.IntRange(0, len(lists)-1).Draw(rt, "log")]
		var a, b []Op
		for _, op := range ops {
			switch rapid.IntRange(0, 2).Draw(rt, "who has it") {
			case 0:
				a = append(a, op)
			case 1:
				b = append(b, op)
			default: // both have it
				a, b = append(a, op), append(b, op)
			}
		}
		fa, err1 := FieldsFromOps(a)
		fb, err2 := FieldsFromOps(b)
		all, err3 := FieldsFromOps(append(append([]Op{}, a...), b...))
		if err1 != nil || err2 != nil || err3 != nil {
			rt.Fatal(err1, err2, err3)
		}
		if got := crdt.Merge(fa, fb); !crdt.Equal(got, all) {
			rt.Fatalf("merge of the halves differs from the registers of the union")
		}
		if got := crdt.Merge(fb, fa); !crdt.Equal(got, all) {
			rt.Fatalf("merge in the other order differs from the registers of the union")
		}
	})
}
