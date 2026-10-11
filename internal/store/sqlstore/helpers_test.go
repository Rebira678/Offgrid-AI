package sqlstore

import (
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"offgrid/internal/task"
)

var epoch = time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)

// clock is a hand-set clock.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }
func (c *clock) tick(d time.Duration) {
	c.t = c.t.Add(d)
}

// newDB returns the path of a fresh database file.
func newDB(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(t.TempDir(), "db")
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "offgrid.db")
}

func open(t *testing.T, path string, opt Options) *Store {
	t.Helper()
	s, err := Open(context.Background(), path, opt)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// freshStore opens a new database with a hand-set clock.
func freshStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	clk := &clock{t: epoch}
	return open(t, newDB(t), Options{NodeID: "node-a", Now: clk.now}), clk
}

// workload runs n random operations against any store, advancing clk between
// them, and returns every result, so two stores can be compared step by step.
type step struct {
	desc    string
	tasks   []task.Task
	created bool
	changed bool
	err     error
}

var (
	titles     = []string{"Leaking pump", "leaking  PUMP", "Replace filter", "Check valve", "Fuel line", "የተበላሸ ፓምፕ", "It's \"quoted\""}
	sites      = []string{"", "4", "5", "A4", "a4"}
	priorities = []task.Priority{task.PriorityLow, task.PriorityMedium, task.PriorityHigh}
	statuses   = []task.Status{task.StatusOpen, task.StatusInProgress, task.StatusDone}
	notes      = []string{"seal cracked", "  waiting for parts ", "line one\nline two", " ", "ok"}
)

func workload(t *testing.T, s task.Store, clk *clock, seed int64, n int) []step {
	t.Helper()
	ctx := context.Background()
	rng := rand.New(rand.NewSource(seed))
	pick := func(max int) int { return rng.Intn(max) }
	var made []string // ids returned by creates so far
	var out []step

	for i := 0; i < n; i++ {
		clk.tick(time.Duration(1+pick(90)) * time.Minute) // some steps cross midnight
		var st step
		switch r := pick(10); {
		case r < 4:
			p := priorities[pick(3)]
			if pick(12) == 0 {
				p = "urgent" // invalid on purpose
			}
			nt := task.NewTask{Title: titles[pick(len(titles))], Site: sites[pick(len(sites))], Priority: p}
			if pick(15) == 0 {
				nt.Title = "   "
			}
			got, created, err := s.Create(ctx, nt)
			st = step{desc: "create", tasks: []task.Task{got}, created: created, err: err}
			if err == nil {
				made = append(made, got.ID)
			}
		case r < 6:
			id := "T99"
			if len(made) > 0 && pick(8) != 0 {
				id = made[pick(len(made))]
			}
			var c task.Changes
			if pick(2) == 0 {
				v := statuses[pick(3)]
				c.Status = &v
			}
			if pick(2) == 0 {
				v := priorities[pick(3)]
				c.Priority = &v
			}
			got, changed, err := s.Update(ctx, id, c)
			st = step{desc: "update " + id, tasks: []task.Task{got}, changed: changed, err: err}
		case r < 8:
			id := "T99"
			if len(made) > 0 && pick(8) != 0 {
				id = made[pick(len(made))]
			}
			got, err := s.AddNote(ctx, id, notes[pick(len(notes))])
			st = step{desc: "note " + id, tasks: []task.Task{got}, err: err}
		default:
			var f task.Filter
			switch pick(4) {
			case 0:
				f.Status = statuses[pick(3)]
			case 1:
				f.ExcludeDone = true
			}
			if pick(3) == 0 {
				f.Priority = priorities[pick(3)]
			}
			if pick(3) == 0 {
				f.Site = sites[1+pick(len(sites)-1)]
			}
			if pick(3) == 0 {
				rg, _ := task.RangeFor([]string{"today", "yesterday", "this_week"}[pick(3)], clk.t)
				f.Within = &rg
			}
			got, err := s.List(ctx, f)
			st = step{desc: "list", tasks: got, err: err}
		}
		out = append(out, st)
	}
	return out
}
