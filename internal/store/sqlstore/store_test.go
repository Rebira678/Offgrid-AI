package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"offgrid/internal/task"
	"offgrid/internal/task/storetest"
)

// ---- the shared contract -----------------------------------------------------

// The same 13 behaviours the in-memory store must have. Passing them is what
// makes SQLite a drop-in replacement: the tools and the agent loop cannot tell
// the two apart.
func TestSQLiteStoreMeetsTheStoreContract(t *testing.T) {
	storetest.Run(t, func(now func() time.Time) task.Store {
		return open(t, newDB(t), Options{NodeID: "node-a", Now: now})
	})
}

// ---- differential testing ----------------------------------------------------

func sameTask(a, b task.Task) string {
	switch {
	case a.ID != b.ID, a.Title != b.Title, a.Site != b.Site, a.Priority != b.Priority, a.Status != b.Status:
		return fmt.Sprintf("fields differ:\n  mem %+v\n  sql %+v", a, b)
	case !a.CreatedAt.Equal(b.CreatedAt), !a.UpdatedAt.Equal(b.UpdatedAt):
		return fmt.Sprintf("times differ: mem created %v updated %v, sql created %v updated %v", a.CreatedAt, a.UpdatedAt, b.CreatedAt, b.UpdatedAt)
	case a.DoneAt.IsZero() != b.DoneAt.IsZero(), !a.DoneAt.IsZero() && !a.DoneAt.Equal(b.DoneAt):
		return fmt.Sprintf("DoneAt differs: mem %v sql %v", a.DoneAt, b.DoneAt)
	case len(a.Notes) != len(b.Notes):
		return fmt.Sprintf("note counts differ: %d vs %d", len(a.Notes), len(b.Notes))
	}
	for i := range a.Notes {
		if a.Notes[i].Text != b.Notes[i].Text || !a.Notes[i].At.Equal(b.Notes[i].At) {
			return fmt.Sprintf("note %d differs: %+v vs %+v", i, a.Notes[i], b.Notes[i])
		}
	}
	return ""
}

func errClass(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, task.ErrNotFound):
		return "not found"
	case errors.Is(err, task.ErrInvalid):
		return "invalid"
	}
	return "other: " + err.Error()
}

// The in-memory store is the oracle: it is small enough to be obviously right.
// Run the same random workload against it and against SQLite, with the same
// clock, and demand identical answers at every step.
func TestSQLiteAgreesWithTheInMemoryStoreOnRandomWorkloads(t *testing.T) {
	const seeds, steps = 25, 200
	for seed := int64(1); seed <= seeds; seed++ {
		memClk, sqlClk := &clock{t: epoch}, &clock{t: epoch}
		mem := task.NewMemStore(memClk.now)
		sq := open(t, newDB(t), Options{NodeID: "node-a", Now: sqlClk.now})

		want := workload(t, mem, memClk, seed, steps)
		got := workload(t, sq, sqlClk, seed, steps)
		for i := range want {
			w, g := want[i], got[i]
			if errClass(w.err) != errClass(g.err) || w.created != g.created || w.changed != g.changed {
				t.Fatalf("seed %d step %d (%s): mem err=%s created=%v changed=%v; sql err=%s created=%v changed=%v",
					seed, i, w.desc, errClass(w.err), w.created, w.changed, errClass(g.err), g.created, g.changed)
			}
			if len(w.tasks) != len(g.tasks) {
				t.Fatalf("seed %d step %d (%s): %d tasks vs %d", seed, i, w.desc, len(w.tasks), len(g.tasks))
			}
			for j := range w.tasks {
				if w.err != nil {
					continue // a failed call returns no task
				}
				if d := sameTask(w.tasks[j], g.tasks[j]); d != "" {
					t.Fatalf("seed %d step %d (%s) task %d: %s", seed, i, w.desc, j, d)
				}
			}
		}
	}
}

// ---- persistence ---------------------------------------------------------------

func TestStateSurvivesCloseAndReopen(t *testing.T) {
	path := newDB(t)
	clk := &clock{t: epoch}
	ctx := context.Background()

	s, err := Open(ctx, path, Options{NodeID: "node-a", Now: clk.now})
	if err != nil {
		t.Fatal(err)
	}
	a, _, _ := s.Create(ctx, task.NewTask{Title: "Leaking pump", Site: "4", Priority: task.PriorityHigh})
	clk.tick(time.Hour)
	s.Create(ctx, task.NewTask{Title: "Replace filter", Priority: task.PriorityLow})
	clk.tick(time.Hour)
	s.AddNote(ctx, a.ID, "seal cracked")
	done := task.StatusDone
	s.Update(ctx, a.ID, task.Changes{Status: &done})
	before, _ := s.List(ctx, task.Filter{})
	hashBefore, _ := s.StateHash(ctx)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := open(t, path, Options{Now: clk.now})
	after, err := s2.List(ctx, task.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("%d tasks after reopen, want %d", len(after), len(before))
	}
	for i := range before {
		if d := sameTask(before[i], after[i]); d != "" {
			t.Errorf("task %d changed across reopen: %s", i, d)
		}
	}
	if h, _ := s2.StateHash(ctx); h != hashBefore {
		t.Errorf("state hash changed across reopen")
	}
	// New work continues the numbering and the sequence.
	c, created, err := s2.Create(ctx, task.NewTask{Title: "Third", Priority: task.PriorityLow})
	if err != nil || !created || c.ID != "T3" {
		t.Fatalf("third task = %+v created %v err %v; want T3", c, created, err)
	}
	if s2.NodeID() != "node-a" {
		t.Errorf("node id = %q after reopen", s2.NodeID())
	}
	ops, _ := s2.Ops(ctx, 0, 100)
	for i, op := range ops {
		if op.Seq != int64(i+1) || op.NodeID != "node-a" {
			t.Errorf("op %d = node %q seq %d; want node-a seq %d", i, op.NodeID, op.Seq, i+1)
		}
	}
}

func TestInMemoryDatabaseWorks(t *testing.T) {
	s := open(t, ":memory:", Options{})
	got, created, err := s.Create(context.Background(), task.NewTask{Title: "x", Priority: task.PriorityLow})
	if err != nil || !created || got.ID != "T1" {
		t.Fatalf("got %+v created %v err %v", got, created, err)
	}
}

// ---- the op log -----------------------------------------------------------------

func TestEachWriteAppendsExactlyOneFactAndNoChangeAppendsNothing(t *testing.T) {
	s, _ := freshStore(t)
	ctx := context.Background()
	a, _, _ := s.Create(ctx, task.NewTask{Title: "a", Priority: task.PriorityLow}) // op 1
	s.Create(ctx, task.NewTask{Title: "A", Priority: task.PriorityHigh})           // duplicate: nothing
	same := task.StatusOpen
	s.Update(ctx, a.ID, task.Changes{Status: &same}) // no change: nothing
	done := task.StatusDone
	s.Update(ctx, a.ID, task.Changes{Status: &done}) // op 2
	s.AddNote(ctx, a.ID, "n")                        // op 3
	s.AddNote(ctx, a.ID, "  ")                       // invalid: nothing
	s.Update(ctx, "T9", task.Changes{Status: &done}) // not found: nothing

	ops, err := s.Ops(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, op := range ops {
		kinds = append(kinds, op.Kind)
	}
	want := []string{KindTaskCreated, KindTaskUpdated, KindNoteAdded}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("ops = %v, want %v", kinds, want)
	}
	if got := string(ops[0].Payload); got != `{"id":"T1","title":"a","site":"","priority":"low"}` {
		t.Errorf("created payload = %s: the id must be IN the op", got)
	}
	if got := string(ops[1].Payload); got != `{"id":"T1","status":"done"}` {
		t.Errorf("updated payload = %s: only the changed field belongs in it", got)
	}
}

func TestOpsRecordTheTimeAndTheReplica(t *testing.T) {
	s, clk := freshStore(t)
	ctx := context.Background()
	s.Create(ctx, task.NewTask{Title: "a", Priority: task.PriorityLow})
	clk.tick(2*time.Hour + 5*time.Second)
	s.AddNote(ctx, "T1", "n")
	ops, _ := s.Ops(ctx, 0, 10)
	if len(ops) != 2 || !ops[0].TS.Equal(epoch) || !ops[1].TS.Equal(epoch.Add(2*time.Hour+5*time.Second)) {
		t.Fatalf("ops = %+v", ops)
	}
	if ops[0].NodeID != "node-a" || ops[0].Pos != 1 || ops[1].Pos != 2 {
		t.Errorf("ops = %+v", ops)
	}
}

func TestLastOpsAndPaging(t *testing.T) {
	s, _ := freshStore(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		s.Create(ctx, task.NewTask{Title: fmt.Sprintf("task %d", i), Priority: task.PriorityLow})
	}
	last, _ := s.LastOps(ctx, 2)
	if len(last) != 2 || last[0].Seq != 4 || last[1].Seq != 5 {
		t.Errorf("LastOps(2) = %+v", last)
	}
	if all, _ := s.LastOps(ctx, 50); len(all) != 5 || all[0].Seq != 1 {
		t.Errorf("LastOps(50) returned %d ops", len(all))
	}
	if none, _ := s.LastOps(ctx, 0); len(none) != 0 {
		t.Errorf("LastOps(0) = %+v", none)
	}
	page, _ := s.Ops(ctx, 2, 2)
	if len(page) != 2 || page[0].Pos != 3 || page[1].Pos != 4 {
		t.Errorf("Ops(after 2, limit 2) = %+v", page)
	}
}

func TestTheLogCannotBeEditedOrShortened(t *testing.T) {
	s, _ := freshStore(t)
	ctx := context.Background()
	s.Create(ctx, task.NewTask{Title: "a", Priority: task.PriorityLow})

	for name, q := range map[string]string{
		"update payload": `UPDATE ops SET payload = '{}' WHERE pos = 1`,
		"update ts":      `UPDATE ops SET ts = 0`,
		"delete one":     `DELETE FROM ops WHERE pos = 1`,
		"delete all":     `DELETE FROM ops`,
	} {
		_, err := s.db.ExecContext(ctx, q)
		if err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: err = %v; want an append-only refusal", name, err)
		}
	}
	// A second op may not reuse an identity either.
	_, err := s.db.ExecContext(ctx, `INSERT INTO ops (node_id, seq, ts, kind, payload) VALUES ('node-a', 1, 0, 'task.created', '{}')`)
	if err == nil {
		t.Error("an op with an existing (node, seq) was accepted")
	}
	// ...and a payload must be JSON.
	_, err = s.db.ExecContext(ctx, `INSERT INTO ops (node_id, seq, ts, kind, payload) VALUES ('node-a', 2, 0, 'task.created', 'not json')`)
	if err == nil {
		t.Error("a payload that is not JSON was accepted")
	}
	if n := countOps(t, s); n != 1 {
		t.Errorf("%d ops after all that, want 1", n)
	}
}

func countOps(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM ops`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ---- atomicity ---------------------------------------------------------------------

// The op and its effect on the view are one transaction. Make the second half
// fail and check the first half vanished: no op without its effect, no gap in
// the replica's sequence numbers.
func TestAFailedWriteLeavesNeitherTheOpNorTheEffect(t *testing.T) {
	s, _ := freshStore(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER sabotage BEFORE INSERT ON tasks BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Create(ctx, task.NewTask{Title: "doomed", Priority: task.PriorityLow}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want the sabotage", err)
	}
	if n := countOps(t, s); n != 0 {
		t.Fatalf("%d ops survived a failed write", n)
	}
	if all, _ := s.List(ctx, task.Filter{}); len(all) != 0 {
		t.Fatalf("view has %d tasks", len(all))
	}

	s.db.ExecContext(ctx, `DROP TRIGGER sabotage`)
	got, created, err := s.Create(ctx, task.NewTask{Title: "fine", Priority: task.PriorityLow})
	if err != nil || !created || got.ID != "T1" {
		t.Fatalf("after the failure: %+v created %v err %v; the ID must still be T1", got, created, err)
	}
	ops, _ := s.Ops(ctx, 0, 10)
	if len(ops) != 1 || ops[0].Seq != 1 || ops[0].Pos != 1 {
		t.Errorf("ops = %+v; sequence and position must have no gap", ops)
	}
}

// ---- rebuild: the day's headline requirement -------------------------------------------

func snapshot(t *testing.T, s *Store) (string, []task.Task) {
	t.Helper()
	h, err := s.StateHash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	l, err := s.List(context.Background(), task.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	return h, l
}

func TestDeleteTheViewRebuildItFromTheLogGetIdenticalState(t *testing.T) {
	ctx := context.Background()
	for seed := int64(1); seed <= 10; seed++ {
		s, clk := freshStore(t)
		workload(t, s, clk, seed, 250)
		hash0, list0 := snapshot(t, s)
		opsBefore := countOps(t, s)
		if len(list0) == 0 {
			t.Fatalf("seed %d: the workload made no tasks; the test would prove nothing", seed)
		}

		for name, destroy := range map[string]string{
			"drop both tables": `DROP TABLE task_notes; DROP TABLE tasks;`,
			"empty both":       `DELETE FROM task_notes; DELETE FROM tasks;`,
			"drop only tasks":  `DROP TABLE tasks;`,
		} {
			if _, err := s.db.ExecContext(ctx, destroy); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			rep, err := s.Rebuild(ctx)
			if err != nil {
				t.Fatalf("seed %d %s: Rebuild: %v", seed, name, err)
			}
			hash1, list1 := snapshot(t, s)
			if hash1 != hash0 || rep.Hash != hash0 {
				t.Fatalf("seed %d %s: hash %s after rebuild, want %s", seed, name, hash1, hash0)
			}
			if len(list1) != len(list0) || rep.Tasks != len(list0) || rep.Ops != opsBefore {
				t.Fatalf("seed %d %s: report %+v, %d tasks (want %d), %d ops (want %d)", seed, name, rep, len(list1), len(list0), rep.Ops, opsBefore)
			}
			for i := range list0 {
				if d := sameTask(list0[i], list1[i]); d != "" {
					t.Fatalf("seed %d %s: task %d: %s", seed, name, i, d)
				}
			}
		}
		if n := countOps(t, s); n != opsBefore {
			t.Fatalf("seed %d: rebuild changed the log (%d -> %d ops)", seed, opsBefore, n)
		}
	}
}

func TestRebuildIsRepeatableAndTheStoreKeepsWorkingAfterIt(t *testing.T) {
	s, clk := freshStore(t)
	ctx := context.Background()
	workload(t, s, clk, 7, 120)
	r1, err := s.Rebuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r2, _ := s.Rebuild(ctx)
	if r1 != r2 {
		t.Errorf("two rebuilds differ: %+v vs %+v", r1, r2)
	}
	got, created, err := s.Create(ctx, task.NewTask{Title: "brand new after rebuild", Priority: task.PriorityLow})
	if err != nil || !created {
		t.Fatalf("Create after rebuild: %+v %v %v", got, created, err)
	}
	if _, err := s.Verify(ctx); err != nil {
		t.Errorf("Verify after rebuild and a new write: %v", err)
	}
}

func TestVerifyAcceptsAHealthyStoreAndCatchesATamperedView(t *testing.T) {
	s, clk := freshStore(t)
	ctx := context.Background()
	workload(t, s, clk, 3, 150)
	rep, err := s.Verify(ctx)
	if err != nil || rep.LiveHash != rep.ReplayHash || rep.Ops == 0 {
		t.Fatalf("healthy store: %+v, %v", rep, err)
	}

	for name, q := range map[string]string{
		"status edited by hand":  `UPDATE tasks SET status = CASE status WHEN 'done' THEN 'open' ELSE 'done' END WHERE id = 'T1'`,
		"title edited by hand":   `UPDATE tasks SET title = 'tampered' WHERE id = 'T1'`,
		"a task deleted by hand": `DELETE FROM tasks WHERE id = 'T1'`,
		"a ghost task inserted":  `INSERT INTO tasks (id, n, title, title_key, site, site_key, priority, status, created_at, updated_at) VALUES ('T999', 999, 'ghost', 'ghost', '', '', 'low', 'open', 1, 1)`,
		"a note removed":         `DELETE FROM task_notes WHERE (task_id, n) IN (SELECT task_id, n FROM task_notes LIMIT 1)`,
	} {
		s2, clk2 := freshStore(t)
		workload(t, s2, clk2, 3, 150)
		before, _ := s2.StateHash(ctx)
		if res, err := s2.db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", name, err)
		} else if n, _ := res.RowsAffected(); n == 0 {
			t.Fatalf("%s: the tampering changed nothing; pick a different one", name)
		}
		if _, err := s2.Verify(ctx); !errors.Is(err, ErrDiverged) {
			t.Errorf("%s: Verify = %v, want ErrDiverged", name, err)
		}
		// The log is the truth, so a rebuild repairs it exactly.
		if _, err := s2.Rebuild(ctx); err != nil {
			t.Fatalf("%s: Rebuild: %v", name, err)
		}
		if after, _ := s2.StateHash(ctx); after != before {
			t.Errorf("%s: rebuild did not restore the original state", name)
		}
		if _, err := s2.Verify(ctx); err != nil {
			t.Errorf("%s: Verify after repair: %v", name, err)
		}
	}
}

func TestVerifyDoesNotTouchTheLiveView(t *testing.T) {
	s, clk := freshStore(t)
	workload(t, s, clk, 5, 100)
	h0, _ := snapshot(t, s)
	ops0 := countOps(t, s)
	s.db.Exec(`UPDATE tasks SET status = CASE status WHEN 'done' THEN 'open' ELSE 'done' END WHERE id = 'T1'`) // make it diverge
	h1, _ := s.StateHash(context.Background())
	s.Verify(context.Background())
	if h2, _ := s.StateHash(context.Background()); h2 != h1 {
		t.Error("Verify changed the live view")
	}
	if countOps(t, s) != ops0 || h0 == "" {
		t.Error("Verify changed the log")
	}
}

// ---- opening: projection version, catch-up, node identity -------------------------------

func TestAChangedProjectionVersionRebuildsTheViewOnOpen(t *testing.T) {
	path := newDB(t)
	clk := &clock{t: epoch}
	s := open(t, path, Options{NodeID: "node-a", Now: clk.now})
	workload(t, s, clk, 11, 100)
	want, _ := snapshot(t, s)
	// Pretend this database was built by an older program whose view had a
	// different shape, and whose tables are now garbage to this one.
	s.db.Exec(`UPDATE meta SET value = '0' WHERE key = 'projection_version'`)
	s.db.Exec(`DELETE FROM task_notes`)
	s.db.Exec(`DELETE FROM tasks`)
	s.Close()

	s2 := open(t, path, Options{Now: clk.now})
	got, _ := snapshot(t, s2)
	if got != want {
		t.Fatal("opening a database with an old projection version did not rebuild the view")
	}
	if v, _, _ := getMeta(context.Background(), s2.db, "projection_version"); v != projectionVersion {
		t.Errorf("projection_version = %q", v)
	}
}

func TestOpenRebuildsAMissingView(t *testing.T) {
	path := newDB(t)
	s, clk := freshStore(t)
	_ = path
	workload(t, s, clk, 12, 80)
	want, _ := snapshot(t, s)
	s.db.Exec(`DROP TABLE task_notes`)
	s.db.Exec(`DROP TABLE tasks`)
	if err := s.ensureProjection(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := snapshot(t, s); got != want {
		t.Fatal("a missing view was not rebuilt")
	}
}

// Ops can reach the log without going through Create/Update/AddNote (later:
// from sync). Opening applies the ones the view has not seen.
func TestOpenAppliesOpsTheViewHasNotSeen(t *testing.T) {
	path := newDB(t)
	clk := &clock{t: epoch}
	s := open(t, path, Options{NodeID: "node-a", Now: clk.now})
	s.Create(context.Background(), task.NewTask{Title: "mine", Priority: task.PriorityLow})
	if _, err := s.db.Exec(`INSERT INTO ops (node_id, seq, ts, kind, payload) VALUES
		('node-b', 1, ?, 'task.created', '{"id":"B1","title":"from another replica","site":"","priority":"high"}'),
		('node-b', 2, ?, 'task.note_added', '{"id":"B1","text":"hello from b"}')`, epoch.UnixNano(), epoch.UnixNano()); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2 := open(t, path, Options{Now: clk.now})
	got, err := s2.Get(context.Background(), "B1")
	if err != nil || got.Title != "from another replica" || len(got.Notes) != 1 || got.Notes[0].Text != "hello from b" {
		t.Fatalf("B1 = %+v, %v", got, err)
	}
	if rep, err := s2.Verify(context.Background()); err != nil || rep.Ops != 3 {
		t.Errorf("Verify = %+v, %v", rep, err)
	}
	// And this replica's own numbering is unaffected by the other's ops.
	mine, _, _ := s2.Create(context.Background(), task.NewTask{Title: "mine again", Priority: task.PriorityLow})
	ops, _ := s2.Ops(context.Background(), 0, 10)
	var mineSeq []int64
	for _, op := range ops {
		if op.NodeID == "node-a" {
			mineSeq = append(mineSeq, op.Seq)
		}
	}
	if len(mineSeq) != 2 || mineSeq[0] != 1 || mineSeq[1] != 2 || mine.Title == "" {
		t.Errorf("node-a seqs = %v", mineSeq)
	}
}

func TestNodeIdentity(t *testing.T) {
	ctx := context.Background()
	path := newDB(t)
	s, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	random := s.NodeID()
	if !strings.HasPrefix(random, "n-") || len(random) != 14 {
		t.Errorf("generated node id = %q", random)
	}
	s.Close()

	s2, err := Open(ctx, path, Options{}) // reopening keeps it
	if err != nil || s2.NodeID() != random {
		t.Fatalf("reopened node id = %q, %v; want %q", s2.NodeID(), err, random)
	}
	s2.Close()

	if _, err := Open(ctx, path, Options{NodeID: "somebody-else"}); !errors.Is(err, ErrNodeMismatch) {
		t.Errorf("a different node id: err = %v, want ErrNodeMismatch", err)
	}
	if s3, err := Open(ctx, path, Options{NodeID: random}); err != nil {
		t.Errorf("asking for the id it already has: %v", err)
	} else {
		s3.Close()
	}
	for _, bad := range []string{"has space", "semi;colon", strings.Repeat("x", 65), "ü"} {
		if _, err := Open(ctx, newDB(t), Options{NodeID: bad}); err == nil {
			t.Errorf("node id %q accepted", bad)
		}
	}
	if _, err := Open(ctx, "", Options{}); err == nil {
		t.Error("an empty path was accepted")
	}
}

// ---- a damaged log --------------------------------------------------------------------------

func TestACorruptLogIsRefusedLoudlyAndNothingIsHalfRebuilt(t *testing.T) {
	ctx := context.Background()
	cases := map[string]string{
		"unknown kind":                   `('x', 1, 4000000000000000000, 'task.exploded', '{}')`,
		"update of a missing task":       `('x', 1, 4000000000000000000, 'task.updated', '{"id":"T404","status":"done"}')`,
		"note on a missing task":         `('x', 1, 4000000000000000000, 'task.note_added', '{"id":"T404","text":"hi"}')`,
		"created twice":                  `('x', 1, 4000000000000000000, 'task.created', '{"id":"T1","title":"dup","site":"","priority":"low"}')`,
		"unknown payload field":          `('x', 1, 4000000000000000000, 'task.created', '{"id":"Z","title":"t","site":"","priority":"low","surprise":1}')`,
		"invalid priority":               `('x', 1, 4000000000000000000, 'task.created', '{"id":"Z","title":"t","site":"","priority":"urgent"}')`,
		"an update that changes nothing": `('x', 1, 4000000000000000000, 'task.updated', '{"id":"T1"}')`,
		"empty note":                     `('x', 1, 4000000000000000000, 'task.note_added', '{"id":"T1","text":"  "}')`,
		"payload is not an object":       `('x', 1, 4000000000000000000, 'task.created', '[1,2]')`,
	}
	// The damaged op gets a timestamp after the real one: the view is folded in
	// timestamp order, so this keeps it "position 2" in that order too.
	for name, values := range cases {
		s, _ := freshStore(t)
		s.Create(ctx, task.NewTask{Title: "good task", Priority: task.PriorityLow})
		before, _ := snapshot(t, s)
		if _, err := s.db.ExecContext(ctx, `INSERT INTO ops (node_id, seq, ts, kind, payload) VALUES `+values); err != nil {
			t.Fatalf("%s: %v", name, err)
		}

		_, err := s.Rebuild(ctx)
		var bad *CorruptOpError
		if !errors.Is(err, ErrCorruptLog) || !errors.As(err, &bad) || bad.Pos != 2 {
			t.Errorf("%s: Rebuild err = %v, want a CorruptOpError at position 2", name, err)
		}
		if after, _ := snapshot(t, s); after != before {
			t.Errorf("%s: a failed rebuild changed the existing view", name)
		}
		if _, err := s.Verify(ctx); !errors.Is(err, ErrCorruptLog) {
			t.Errorf("%s: Verify err = %v, want ErrCorruptLog", name, err)
		}
	}
}

func TestAGapInAReplicasSequenceIsDetected(t *testing.T) {
	s, _ := freshStore(t)
	ctx := context.Background()
	s.Create(ctx, task.NewTask{Title: "a", Priority: task.PriorityLow})
	// replica y has op 2 but never had op 1: something was lost.
	s.db.Exec(`INSERT INTO ops (node_id, seq, ts, kind, payload) VALUES ('y', 2, 0, 'task.note_added', '{"id":"T1","text":"n"}')`)
	_, err := s.Verify(ctx)
	if !errors.Is(err, ErrCorruptLog) || !strings.Contains(err.Error(), `"y"`) {
		t.Fatalf("err = %v, want a sequence-gap report naming replica y", err)
	}
}

func TestOpeningADatabaseWithACorruptLogFails(t *testing.T) {
	path := newDB(t)
	s := open(t, path, Options{NodeID: "node-a"})
	s.Create(context.Background(), task.NewTask{Title: "a", Priority: task.PriorityLow})
	s.db.Exec(`INSERT INTO ops (node_id, seq, ts, kind, payload) VALUES ('x', 1, 0, 'task.nonsense', '{}')`)
	s.Close()
	if _, err := Open(context.Background(), path, Options{}); !errors.Is(err, ErrCorruptLog) {
		t.Fatalf("Open err = %v, want ErrCorruptLog", err)
	}
}

// ---- hostile and awkward text ------------------------------------------------------------------

func TestTextIsStoredVerbatimWhateverItContains(t *testing.T) {
	s, _ := freshStore(t)
	ctx := context.Background()
	nasty := []string{
		`'; DROP TABLE tasks; --`,
		`Robert'); DELETE FROM ops; --`,
		"line one\nline two\ttabbed",
		`quotes "double" and 'single' and ` + "`backtick`",
		"የተበላሸ ፓምፕ በቦታ 4",
		"emoji 🚰 pump",
		`100% _wild_ [brackets] \backslash`,
	}
	for i, title := range nasty {
		got, _, err := s.Create(ctx, task.NewTask{Title: title, Site: fmt.Sprintf("S%d", i), Priority: task.PriorityLow})
		if err != nil {
			t.Fatalf("%q: %v", title, err)
		}
		if _, err := s.AddNote(ctx, got.ID, title); err != nil {
			t.Fatalf("note %q: %v", title, err)
		}
	}
	all, err := s.List(ctx, task.Filter{})
	if err != nil || len(all) != len(nasty) {
		t.Fatalf("list = %d tasks, %v", len(all), err)
	}
	for i, title := range nasty {
		want := strings.TrimSpace(title)
		if all[i].Title != want || len(all[i].Notes) != 1 || all[i].Notes[0].Text != want {
			t.Errorf("task %d = %q / %+v; want %q back exactly", i, all[i].Title, all[i].Notes, want)
		}
	}
	if countOps(t, s) != 2*len(nasty) {
		t.Error("the log lost or gained ops")
	}
	if _, err := s.Verify(ctx); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

func TestTimeZonesDoNotLeakIntoTheStoredState(t *testing.T) {
	eat := time.FixedZone("EAT", 3*3600)
	clkA := &clock{t: time.Date(2026, 10, 8, 12, 0, 0, 0, eat)}
	clkB := &clock{t: time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)} // the same instant
	a := open(t, newDB(t), Options{NodeID: "n", Now: clkA.now})
	b := open(t, newDB(t), Options{NodeID: "n", Now: clkB.now})
	for _, s := range []*Store{a, b} {
		s.Create(context.Background(), task.NewTask{Title: "x", Priority: task.PriorityLow})
		s.AddNote(context.Background(), "T1", "n")
	}
	ha, _ := a.StateHash(context.Background())
	hb, _ := b.StateHash(context.Background())
	if ha != hb {
		t.Error("the same instant written in two time zones gave different state")
	}
}

func TestStateHashIsDeterministicAndSensitive(t *testing.T) {
	run := func(note string) string {
		s, _ := freshStore(t)
		ctx := context.Background()
		s.Create(ctx, task.NewTask{Title: "x", Priority: task.PriorityLow})
		s.AddNote(ctx, "T1", note)
		h, _ := s.StateHash(ctx)
		return h
	}
	if run("same") != run("same") {
		t.Error("the same history gave different hashes")
	}
	if run("one") == run("two") {
		t.Error("different notes gave the same hash")
	}
	empty, _ := freshStore(t)
	h, err := empty.StateHash(context.Background())
	if err != nil || len(h) != 64 {
		t.Errorf("empty store hash = %q, %v", h, err)
	}
}

func TestInfo(t *testing.T) {
	s, clk := freshStore(t)
	workload(t, s, clk, 2, 60)
	info, err := s.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.NodeID != "node-a" || info.SchemaVersion != len(migrations) || info.ProjectionVersion != projectionVersion ||
		info.Ops != countOps(t, s) || info.AppliedPos != info.LastPos || info.Tasks == 0 || len(info.StateHash) != 64 {
		t.Errorf("info = %+v", info)
	}
}

func TestADeadContextStopsEverything(t *testing.T) {
	s, _ := freshStore(t)
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Rebuild(dead); err == nil {
		t.Error("Rebuild ran with a cancelled context")
	}
	if _, err := Open(dead, newDB(t), Options{}); err == nil {
		t.Error("Open ran with a cancelled context")
	}
}

var _ = sql.ErrNoRows
