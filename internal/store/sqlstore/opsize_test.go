package sqlstore

import (
	"context"
	"fmt"
	"testing"
	"time"

	"offgrid/internal/task"
)

// opCost fills a database with a plausible crew's mix (a task, a status change and a note per three
// ops) and returns the database file bytes per op. This is the number a growth projection multiplies by
// the pilot's real ops per day, and a budget on it catches a change that quietly makes every op bigger.
func opCost(t *testing.T, signed bool, n int) (perOp, payloadPerOp float64) {
	t.Helper()
	ctx := context.Background()
	clk := &clock{t: epoch}
	opt := Options{NodeID: "crew", Now: clk.now}
	if signed {
		opt.Signer = newKey(t)
	}
	s := open(t, newDB(t), opt)
	if signed {
		if err := s.Found(ctx, "laptop"); err != nil {
			t.Fatal(err)
		}
	}
	done := task.StatusDone
	for i := 0; i < n/3; i++ {
		clk.tick(time.Minute)
		tk, _, err := s.Create(ctx, task.NewTask{Title: fmt.Sprintf("Replace the pump seal on line %d", i), Priority: task.PriorityHigh})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.AddNote(ctx, tk.ID, "Checked the housing, seal is worn, ordered part 4471"); err != nil {
			t.Fatal(err)
		}
		if _, _, err = s.Update(ctx, tk.ID, task.Changes{Status: &done}); err != nil {
			t.Fatal(err)
		}
	}
	g, err := s.Growth(ctx, clk.now())
	if err != nil {
		t.Fatal(err)
	}
	return float64(g.DBBytes) / float64(g.Ops), float64(g.OpBytes) / float64(g.Ops)
}

func TestWhatOneOpCostsOnDisk(t *testing.T) {
	for _, c := range []struct {
		name   string
		signed bool
		budget float64
	}{{"unsigned", false, 700}, {"signed", true, 950}} {
		per, pay := opCost(t, c.signed, 3000)
		t.Logf("%s: %.0f bytes of database per op (payload %.0f)", c.name, per, pay)
		if per > c.budget {
			t.Errorf("%s: %.0f bytes per op is over the budget of %.0f: every op got bigger, and a crew-year grows with it", c.name, per, c.budget)
		}
		if per < pay {
			t.Errorf("%s: %.0f bytes per op on disk cannot be below the payload %.0f", c.name, per, pay)
		}
	}
}
