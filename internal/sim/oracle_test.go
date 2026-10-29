package sim

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
)

type tclock struct{ t time.Time }

func (c *tclock) now() time.Time { return c.t }

func rep(t *testing.T, node string, clk *tclock) *sqlstore.Store {
	t.Helper()
	s, err := sqlstore.Open(context.Background(), filepath.Join(t.TempDir(), node+".db"), sqlstore.Options{NodeID: node, Now: clk.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func pullAll(t *testing.T, dst, src *sqlstore.Store) {
	t.Helper()
	ctx := context.Background()
	have, _ := dst.Summary(ctx)
	ops, _, err := src.OpsSince(ctx, have, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dst.ApplyRemote(ctx, ops); err != nil {
		t.Fatal(err)
	}
}

func opsOf(t *testing.T, s *sqlstore.Store) []sqlstore.Op {
	t.Helper()
	ops, _, err := s.OpsSince(context.Background(), sqlstore.Summary{}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return ops
}

// a deletes a task, b edits it without having seen the delete: add-wins says it lives.
func deleteVersusEdit(t *testing.T) (a *sqlstore.Store, edit sqlstore.Op) {
	ctx := context.Background()
	clk := &tclock{epoch}
	a, b := rep(t, "a", clk), rep(t, "b", clk)
	tk, _, err := a.Create(ctx, task.NewTask{Title: "pump", Priority: task.PriorityLow})
	if err != nil {
		t.Fatal(err)
	}
	pullAll(t, b, a)
	clk.t = clk.t.Add(time.Minute)
	if err := a.DeleteTask(ctx, tk.ID); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Minute)
	hi := task.PriorityHigh
	if _, _, err := b.Update(ctx, tk.ID, task.Changes{Priority: &hi}); err != nil {
		t.Fatal(err)
	}
	pullAll(t, a, b)
	pullAll(t, b, a)
	for _, op := range opsOf(t, a) {
		if op.NodeID == "b" && op.Kind == sqlstore.KindTaskUpdated {
			edit = op
		}
	}
	return a, edit
}

func TestTheOracleAgreesWithAStoreThatKeepsATaskAnUnseenEditTouched(t *testing.T) {
	a, _ := deleteVersusEdit(t)
	if f := checkAgainstTheLog(context.Background(), a, opsOf(t, a)); f != nil {
		t.Fatal(f)
	}
}

// Hand the oracle a log without the edit: by that log the task is deleted, and
// the store (which has the edit) keeps it. The oracle must say so.
func TestTheOracleCatchesAViewThatDisagreesWithTheLog(t *testing.T) {
	a, edit := deleteVersusEdit(t)
	var without []sqlstore.Op
	for _, op := range opsOf(t, a) {
		if !(op.NodeID == edit.NodeID && op.Seq == edit.Seq) {
			without = append(without, op)
		}
	}
	f := checkAgainstTheLog(context.Background(), a, without)
	if f == nil || f.Class != "delete-rule" || !strings.Contains(f.Msg, "deleted") {
		t.Fatalf("%+v", f)
	}
}

// Two devices resolve one suggestion differently: the first in fold order wins.
func TestTheOracleChecksTheFirstResolutionOfASuggestion(t *testing.T) {
	ctx := context.Background()
	clk := &tclock{epoch}
	a, b := rep(t, "a", clk), rep(t, "b", clk)
	tk, _, _ := a.Create(ctx, task.NewTask{Title: "pump", Priority: task.PriorityLow})
	hi := task.PriorityHigh
	sg, _, err := a.Suggest(ctx, task.Draft{Task: tk.ID, Priority: &hi, By: "m"})
	if err != nil {
		t.Fatal(err)
	}
	pullAll(t, b, a)
	clk.t = clk.t.Add(time.Minute)
	if _, err := a.Resolve(ctx, sg.ID, true); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Minute)
	if _, err := b.Resolve(ctx, sg.ID, false); err != nil {
		t.Fatal(err)
	}
	pullAll(t, a, b)
	pullAll(t, b, a)
	for _, s := range []*sqlstore.Store{a, b} {
		if f := checkAgainstTheLog(ctx, s, opsOf(t, s)); f != nil {
			t.Fatal(f)
		}
	}
	// without the log's first resolution the oracle expects "pending", the view says otherwise
	var cut []sqlstore.Op
	for _, op := range opsOf(t, a) {
		if op.Kind != sqlstore.KindSuggestionResolved {
			cut = append(cut, op)
		}
	}
	if f := checkAgainstTheLog(ctx, a, cut); f == nil || f.Class != "suggestion-rule" {
		t.Fatalf("%+v", f)
	}
}
