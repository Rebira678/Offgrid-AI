package task_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"offgrid/internal/task"
	"offgrid/internal/task/storetest"
)

func TestMemStoreMeetsTheStoreContract(t *testing.T) {
	storetest.Run(t, func(now func() time.Time) task.Store { return task.NewMemStore(now) })
}

func TestParseEnums(t *testing.T) {
	for _, s := range []string{"open", "in_progress", "done"} {
		if got, err := task.ParseStatus(s); err != nil || string(got) != s {
			t.Errorf("ParseStatus(%q) = %q, %v", s, got, err)
		}
	}
	for _, s := range []string{"", "Open", "finished", "in progress"} {
		if _, err := task.ParseStatus(s); err == nil {
			t.Errorf("ParseStatus(%q) accepted", s)
		}
	}
	for _, s := range []string{"low", "medium", "high"} {
		if got, err := task.ParsePriority(s); err != nil || string(got) != s {
			t.Errorf("ParsePriority(%q) = %q, %v", s, got, err)
		}
	}
	for _, s := range []string{"", "HIGH", "urgent"} {
		if _, err := task.ParsePriority(s); err == nil {
			t.Errorf("ParsePriority(%q) accepted", s)
		}
	}
	if !(task.PriorityHigh.Rank() > task.PriorityMedium.Rank() &&
		task.PriorityMedium.Rank() > task.PriorityLow.Rank() &&
		task.PriorityLow.Rank() > task.Priority("").Rank()) {
		t.Error("priority ranks are not ordered high > medium > low > unknown")
	}
}

// ---- time ranges ----------------------------------------------------------

func TestRangeForPlainDays(t *testing.T) {
	now := time.Date(2026, 10, 8, 15, 30, 0, 0, time.UTC) // a Thursday
	cases := []struct {
		when       string
		start, end string
	}{
		{"today", "2026-10-08", "2026-10-09"},
		{"yesterday", "2026-10-07", "2026-10-08"},
		{"this_week", "2026-10-05", "2026-10-12"}, // Monday to the next Monday
	}
	for _, c := range cases {
		r, err := task.RangeFor(c.when, now)
		if err != nil {
			t.Fatal(err)
		}
		if got := r.Start.Format("2006-01-02"); got != c.start {
			t.Errorf("%s start = %s, want %s", c.when, got, c.start)
		}
		if got := r.End.Format("2006-01-02"); got != c.end {
			t.Errorf("%s end = %s, want %s", c.when, got, c.end)
		}
	}
}

func TestWeekStartsOnMondayEvenOnSundayAndMonday(t *testing.T) {
	for day, want := range map[int]string{
		5:  "2026-10-05", // Monday
		11: "2026-10-05", // Sunday belongs to the week that began on Monday the 5th
		12: "2026-10-12", // the next Monday starts a new week
	} {
		now := time.Date(2026, 10, day, 12, 0, 0, 0, time.UTC)
		r, _ := task.RangeFor("this_week", now)
		if got := r.Start.Format("2006-01-02"); got != want {
			t.Errorf("on Oct %d the week starts %s, want %s", day, got, want)
		}
	}
}

func TestRangesAreHalfOpenAndTile(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	today, _ := task.RangeFor("today", now)
	yest, _ := task.RangeFor("yesterday", now)

	midnight := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	if !today.Contains(midnight) || yest.Contains(midnight) {
		t.Error("midnight must belong to today only")
	}
	justBefore := midnight.Add(-time.Nanosecond)
	if today.Contains(justBefore) || !yest.Contains(justBefore) {
		t.Error("the last instant before midnight must belong to yesterday only")
	}
	if !yest.End.Equal(today.Start) {
		t.Error("yesterday must end exactly where today starts")
	}
	if today.Contains(today.End) {
		t.Error("End is exclusive")
	}
}

func TestRangeSurvivesDaylightSavingChange(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no timezone database on this machine")
	}
	// US clocks went forward on 2026-03-08: that day has only 23 hours.
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, ny)
	today, _ := task.RangeFor("today", now)
	if d := today.End.Sub(today.Start); d != 23*time.Hour {
		t.Fatalf("the spring-forward day is %v long, want 23h", d)
	}
	if got := today.End.Format("2006-01-02 15:04"); got != "2026-03-09 00:00" {
		t.Errorf("today ends at %s, want 2026-03-09 00:00 (adding 24h would give 01:00)", got)
	}
	yest, _ := task.RangeFor("yesterday", now)
	if got := yest.Start.Format("2006-01-02 15:04"); got != "2026-03-07 00:00" {
		t.Errorf("yesterday starts at %s", got)
	}
	week, _ := task.RangeFor("this_week", now) // Sunday the 8th; the week began Monday the 2nd
	if got := week.Start.Format("2006-01-02"); got != "2026-03-02" {
		t.Errorf("week starts %s, want 2026-03-02", got)
	}
}

func TestRangeForRejectsUnknownWords(t *testing.T) {
	for _, w := range []string{"", "tomorrow", "Today", "last_week"} {
		if _, err := task.RangeFor(w, time.Now()); err == nil {
			t.Errorf("RangeFor(%q) accepted", w)
		}
		if task.ValidWhen(w) {
			t.Errorf("ValidWhen(%q) = true", w)
		}
	}
	for _, w := range []string{"today", "yesterday", "this_week"} {
		if !task.ValidWhen(w) {
			t.Errorf("ValidWhen(%q) = false", w)
		}
	}
}

// ---- resolving a reference --------------------------------------------------

func sample() []task.Task {
	return []task.Task{
		{ID: "T1", Title: "Leaking pump", Status: task.StatusOpen},
		{ID: "T2", Title: "Pump inspection", Status: task.StatusOpen},
		{ID: "T3", Title: "Replace filter", Status: task.StatusInProgress},
		{ID: "T4", Title: "Old valve repair", Status: task.StatusDone},
		{ID: "T5", Title: "Valve repair", Status: task.StatusDone},
		{ID: "T10", Title: "Calibrate gauge", Status: task.StatusOpen},
	}
}

func TestResolve(t *testing.T) {
	cases := []struct {
		ref  string
		want string // task ID, "" for not found, "ambiguous"
	}{
		{"T1", "T1"}, {"t1", "T1"}, {"#1", "T1"}, {"#T3", "T3"}, {"3", "T3"}, {" T10 ", "T10"},
		{"T-3", "T3"}, {"t 3", "T3"}, {"T_10", "T10"}, {"T-99", ""}, {"-3", ""},
		{"T99", ""}, {"99", ""}, {"#99", ""},
		{"leaking pump", "T1"},
		{"LEAKING   PUMP", "T1"},
		{"filter", "T3"},      // one task contains it
		{"pump", "ambiguous"}, // T1 and T2 both contain it
		{"gauge", "T10"},
		{"valve repair", "T5"}, // exact match beats "Old valve repair", which only contains it
		{"valve", "ambiguous"}, // only finished tasks match, and there are two
		{"nothing like this", ""},
		{"", ""},
	}
	for _, c := range cases {
		got, err := task.Resolve(sample(), c.ref)
		switch c.want {
		case "":
			if !errors.Is(err, task.ErrNotFound) {
				t.Errorf("Resolve(%q) = %+v, %v; want ErrNotFound", c.ref, got, err)
			}
		case "ambiguous":
			var amb *task.AmbiguousError
			if !errors.Is(err, task.ErrAmbiguous) || !errors.As(err, &amb) || len(amb.Candidates) < 2 {
				t.Errorf("Resolve(%q) = %+v, %v; want an ambiguity error", c.ref, got, err)
			}
		default:
			if err != nil || got.ID != c.want {
				t.Errorf("Resolve(%q) = %q, %v; want %s", c.ref, got.ID, err, c.want)
			}
		}
	}
}

func TestResolvePrefersUnfinishedTasks(t *testing.T) {
	tasks := []task.Task{
		{ID: "T1", Title: "Fix pump seal", Status: task.StatusDone},
		{ID: "T2", Title: "Fix pump seal again", Status: task.StatusOpen},
	}
	got, err := task.Resolve(tasks, "pump seal")
	if err != nil || got.ID != "T2" {
		t.Fatalf("got %q, %v; want the open task T2", got.ID, err)
	}
	// The finished task is still reachable when nothing unfinished matches.
	got, err = task.Resolve(tasks[:1], "pump seal")
	if err != nil || got.ID != "T1" {
		t.Fatalf("got %q, %v; want the finished task T1", got.ID, err)
	}
}

func TestAnIDShapedReferenceNeverFallsBackToATitleSearch(t *testing.T) {
	tasks := []task.Task{{ID: "T1", Title: "Check 9 valves", Status: task.StatusOpen}}
	if _, err := task.Resolve(tasks, "9"); !errors.Is(err, task.ErrNotFound) {
		t.Fatalf("error = %v; \"9\" is an ID, not a title fragment", err)
	}
}

func TestAmbiguityErrorNamesTheCandidates(t *testing.T) {
	_, err := task.Resolve(sample(), "pump")
	msg := err.Error()
	for _, want := range []string{`"pump"`, "2 tasks", "T1", "Leaking pump", "T2", "Pump inspection", "using its ID"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
}
