// Package storetest holds the behaviour every task.Store must have. Each
// implementation (the in-memory store today, SQLite on Day 6) runs the same
// tests, so swapping the store cannot quietly change what a tool does.
package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"offgrid/internal/task"
)

// Clock is a hand-set clock for the store under test.
type Clock struct{ T time.Time }

func (c *Clock) Now() time.Time { return c.T }

// Factory builds an empty store that reads time from now.
type Factory func(now func() time.Time) task.Store

// Run runs the contract against stores made by newStore.
func Run(t *testing.T, newStore Factory) {
	t.Helper()
	ctx := context.Background()
	start := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)

	setup := func(t *testing.T) (task.Store, *Clock) {
		t.Helper()
		clk := &Clock{T: start}
		return newStore(clk.Now), clk
	}
	create := func(t *testing.T, s task.Store, title, site string, p task.Priority) task.Task {
		t.Helper()
		got, _, err := s.Create(ctx, task.NewTask{Title: title, Site: site, Priority: p})
		if err != nil {
			t.Fatalf("Create(%q): %v", title, err)
		}
		return got
	}

	t.Run("create assigns id, open status and timestamps", func(t *testing.T) {
		s, _ := setup(t)
		got, created, err := s.Create(ctx, task.NewTask{Title: "  Leaking pump ", Site: "4", Priority: task.PriorityHigh})
		if err != nil || !created {
			t.Fatalf("Create = created %v, err %v", created, err)
		}
		if got.ID == "" || got.Title != "Leaking pump" || got.Status != task.StatusOpen ||
			got.Priority != task.PriorityHigh || got.Site != "4" {
			t.Fatalf("unexpected task: %+v", got)
		}
		if !got.CreatedAt.Equal(start) || !got.UpdatedAt.Equal(start) || !got.DoneAt.IsZero() {
			t.Fatalf("bad timestamps: %+v", got)
		}
	})

	t.Run("ids are unique", func(t *testing.T) {
		s, _ := setup(t)
		a := create(t, s, "one", "", task.PriorityLow)
		b := create(t, s, "two", "", task.PriorityLow)
		if a.ID == b.ID {
			t.Fatalf("both tasks got id %q", a.ID)
		}
	})

	t.Run("create rejects empty title and bad priority", func(t *testing.T) {
		s, _ := setup(t)
		for _, n := range []task.NewTask{
			{Title: "", Priority: task.PriorityLow},
			{Title: "   ", Priority: task.PriorityLow},
			{Title: "x", Priority: "urgent"},
			{Title: "x", Priority: ""},
		} {
			if _, _, err := s.Create(ctx, n); !errors.Is(err, task.ErrInvalid) {
				t.Errorf("Create(%+v) error = %v, want ErrInvalid", n, err)
			}
		}
		if all, _ := s.List(ctx, task.Filter{}); len(all) != 0 {
			t.Fatalf("rejected creates left %d tasks behind", len(all))
		}
	})

	t.Run("create is idempotent for an unfinished duplicate", func(t *testing.T) {
		s, _ := setup(t)
		first := create(t, s, "Leaking pump", "4", task.PriorityHigh)
		again, created, err := s.Create(ctx, task.NewTask{Title: "  leaking   PUMP", Site: "4", Priority: task.PriorityLow})
		if err != nil || created || again.ID != first.ID {
			t.Fatalf("duplicate = %+v created %v err %v; want the first task back", again, created, err)
		}
		if again.Priority != task.PriorityHigh {
			t.Errorf("a duplicate must not change the existing task, priority is now %s", again.Priority)
		}
		// A different site is a different task.
		other, created, _ := s.Create(ctx, task.NewTask{Title: "Leaking pump", Site: "5", Priority: task.PriorityHigh})
		if !created || other.ID == first.ID {
			t.Errorf("same title at another site must be a new task")
		}
		if all, _ := s.List(ctx, task.Filter{}); len(all) != 2 {
			t.Errorf("have %d tasks, want 2", len(all))
		}
	})

	t.Run("a finished task does not block a new one with the same title", func(t *testing.T) {
		s, _ := setup(t)
		first := create(t, s, "Replace filter", "2", task.PriorityMedium)
		done := task.StatusDone
		if _, _, err := s.Update(ctx, first.ID, task.Changes{Status: &done}); err != nil {
			t.Fatal(err)
		}
		again, created, err := s.Create(ctx, task.NewTask{Title: "Replace filter", Site: "2", Priority: task.PriorityMedium})
		if err != nil || !created || again.ID == first.ID {
			t.Fatalf("expected a fresh task, got %+v created %v err %v", again, created, err)
		}
	})

	t.Run("get", func(t *testing.T) {
		s, _ := setup(t)
		made := create(t, s, "a", "", task.PriorityLow)
		got, err := s.Get(ctx, made.ID)
		if err != nil || got.ID != made.ID {
			t.Fatalf("Get = %+v, %v", got, err)
		}
		if _, err := s.Get(ctx, "T999"); !errors.Is(err, task.ErrNotFound) {
			t.Fatalf("Get(missing) error = %v, want ErrNotFound", err)
		}
	})

	t.Run("update changes status and priority and tracks DoneAt", func(t *testing.T) {
		s, clk := setup(t)
		made := create(t, s, "a", "", task.PriorityLow)

		clk.T = start.Add(time.Hour)
		done, high := task.StatusDone, task.PriorityHigh
		got, changed, err := s.Update(ctx, made.ID, task.Changes{Status: &done, Priority: &high})
		if err != nil || !changed {
			t.Fatalf("Update = changed %v, err %v", changed, err)
		}
		if got.Status != task.StatusDone || got.Priority != task.PriorityHigh ||
			!got.DoneAt.Equal(clk.T) || !got.UpdatedAt.Equal(clk.T) {
			t.Fatalf("unexpected result: %+v", got)
		}

		clk.T = start.Add(2 * time.Hour)
		open := task.StatusOpen
		got, _, _ = s.Update(ctx, made.ID, task.Changes{Status: &open})
		if !got.DoneAt.IsZero() {
			t.Errorf("reopening must clear DoneAt, got %v", got.DoneAt)
		}
	})

	t.Run("update that changes nothing reports changed=false and keeps UpdatedAt", func(t *testing.T) {
		s, clk := setup(t)
		made := create(t, s, "a", "", task.PriorityLow)
		clk.T = start.Add(time.Hour)
		same := task.StatusOpen
		got, changed, err := s.Update(ctx, made.ID, task.Changes{Status: &same})
		if err != nil || changed {
			t.Fatalf("Update = changed %v, err %v; want unchanged", changed, err)
		}
		if !got.UpdatedAt.Equal(start) {
			t.Errorf("UpdatedAt moved to %v although nothing changed", got.UpdatedAt)
		}
	})

	t.Run("update rejects empty or invalid changes and unknown ids", func(t *testing.T) {
		s, _ := setup(t)
		made := create(t, s, "a", "", task.PriorityLow)
		bad := task.Status("finished")
		badP := task.Priority("urgent")
		ok := task.StatusDone
		for name, c := range map[string]task.Changes{
			"nothing":      {},
			"bad status":   {Status: &bad},
			"bad priority": {Priority: &badP},
		} {
			if _, _, err := s.Update(ctx, made.ID, c); !errors.Is(err, task.ErrInvalid) {
				t.Errorf("%s: error = %v, want ErrInvalid", name, err)
			}
		}
		if _, _, err := s.Update(ctx, "T999", task.Changes{Status: &ok}); !errors.Is(err, task.ErrNotFound) {
			t.Errorf("unknown id: error = %v, want ErrNotFound", err)
		}
		got, _ := s.Get(ctx, made.ID)
		if got.Status != task.StatusOpen {
			t.Errorf("rejected updates changed the task: %+v", got)
		}
	})

	t.Run("notes", func(t *testing.T) {
		s, clk := setup(t)
		made := create(t, s, "a", "", task.PriorityLow)
		clk.T = start.Add(30 * time.Minute)
		got, err := s.AddNote(ctx, made.ID, "  seal is cracked ")
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Notes) != 1 || got.Notes[0].Text != "seal is cracked" || !got.Notes[0].At.Equal(clk.T) {
			t.Fatalf("notes = %+v", got.Notes)
		}
		if !got.UpdatedAt.Equal(clk.T) {
			t.Errorf("a note must move UpdatedAt")
		}
		if _, err := s.AddNote(ctx, made.ID, "  "); !errors.Is(err, task.ErrInvalid) {
			t.Errorf("blank note error = %v, want ErrInvalid", err)
		}
		if _, err := s.AddNote(ctx, "T999", "x"); !errors.Is(err, task.ErrNotFound) {
			t.Errorf("unknown id error = %v, want ErrNotFound", err)
		}
	})

	t.Run("list filters and order", func(t *testing.T) {
		s, clk := setup(t)
		yesterday := start.AddDate(0, 0, -1)
		clk.T = yesterday
		a := create(t, s, "a", "4", task.PriorityHigh)
		clk.T = start
		b := create(t, s, "b", "5", task.PriorityLow)
		c := create(t, s, "c", "4", task.PriorityLow)
		done := task.StatusDone
		if _, _, err := s.Update(ctx, c.ID, task.Changes{Status: &done}); err != nil {
			t.Fatal(err)
		}

		ids := func(f task.Filter) []string {
			got, err := s.List(ctx, f)
			if err != nil {
				t.Fatal(err)
			}
			out := []string{}
			for _, x := range got {
				out = append(out, x.ID)
			}
			return out
		}
		eq := func(name string, got []string, want ...string) {
			t.Helper()
			if len(got) != len(want) {
				t.Errorf("%s = %v, want %v", name, got, want)
				return
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("%s = %v, want %v", name, got, want)
					return
				}
			}
		}
		eq("all, in creation order", ids(task.Filter{}), a.ID, b.ID, c.ID)
		eq("status done", ids(task.Filter{Status: task.StatusDone}), c.ID)
		eq("exclude done", ids(task.Filter{ExcludeDone: true}), a.ID, b.ID)
		eq("Status wins over ExcludeDone", ids(task.Filter{Status: task.StatusDone, ExcludeDone: true}), c.ID)
		eq("priority", ids(task.Filter{Priority: task.PriorityLow}), b.ID, c.ID)
		eq("site, ignoring case", ids(task.Filter{Site: "4"}), a.ID, c.ID)

		today, _ := task.RangeFor("today", start)
		yest, _ := task.RangeFor("yesterday", start)
		eq("created today", ids(task.Filter{Within: &today}), b.ID, c.ID)
		eq("created yesterday", ids(task.Filter{Within: &yest}), a.ID)
		eq("filters combine", ids(task.Filter{Within: &today, Site: "4"}), c.ID)

		empty, err := s.List(ctx, task.Filter{Site: "nowhere"})
		if err != nil || empty == nil || len(empty) != 0 {
			t.Errorf("empty result = %#v, %v; want an empty non-nil slice", empty, err)
		}
	})

	t.Run("returned tasks are copies", func(t *testing.T) {
		s, _ := setup(t)
		made := create(t, s, "a", "", task.PriorityLow)
		if _, err := s.AddNote(ctx, made.ID, "first"); err != nil {
			t.Fatal(err)
		}
		got, _ := s.Get(ctx, made.ID)
		got.Title = "tampered"
		got.Notes[0].Text = "tampered"
		again, _ := s.Get(ctx, made.ID)
		if again.Title != "a" || again.Notes[0].Text != "first" {
			t.Fatalf("a caller changed the stored task: %+v", again)
		}
		list, _ := s.List(ctx, task.Filter{})
		list[0].Notes[0].Text = "tampered"
		again, _ = s.Get(ctx, made.ID)
		if again.Notes[0].Text != "first" {
			t.Fatalf("List leaked the stored notes")
		}
	})

	t.Run("a cancelled context changes nothing", func(t *testing.T) {
		s, _ := setup(t)
		made := create(t, s, "a", "", task.PriorityLow)
		dead, cancel := context.WithCancel(ctx)
		cancel()
		done := task.StatusDone

		if _, _, err := s.Create(dead, task.NewTask{Title: "b", Priority: task.PriorityLow}); !errors.Is(err, context.Canceled) {
			t.Errorf("Create error = %v, want context.Canceled", err)
		}
		if _, _, err := s.Update(dead, made.ID, task.Changes{Status: &done}); !errors.Is(err, context.Canceled) {
			t.Errorf("Update error = %v, want context.Canceled", err)
		}
		if _, err := s.AddNote(dead, made.ID, "x"); !errors.Is(err, context.Canceled) {
			t.Errorf("AddNote error = %v, want context.Canceled", err)
		}
		if _, err := s.List(dead, task.Filter{}); !errors.Is(err, context.Canceled) {
			t.Errorf("List error = %v, want context.Canceled", err)
		}
		all, _ := s.List(ctx, task.Filter{})
		if len(all) != 1 || all[0].Status != task.StatusOpen || len(all[0].Notes) != 0 {
			t.Errorf("a cancelled call changed the store: %+v", all)
		}
	})
}
