package doctor

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestDoctorDeepCheckDetectsDivergedView(t *testing.T) {
	db := newDB(t, t0, "pump check")
	o := opts(db)
	o.Deep = true
	mustLevel(t, Run(context.Background(), o), "view", Pass)

	// Mutate only the materialized view, keeping the op log and hash chain intact.
	raw, err := sql.Open("sqlite3", db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE tasks SET title = 'tampered view'`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	raw.Close()

	cs := Run(context.Background(), o)
	mustLevel(t, cs, "chain", Pass)
	d := mustLevel(t, cs, "view", Fail)
	if !strings.Contains(d, "materialized view does not match") {
		t.Errorf("expected diverged view failure detail, got %q", d)
	}
}

func TestDoctorCancelledContextNeverReportsIntegrityPass(t *testing.T) {
	db := newDB(t, t0, "valve inspection")
	o := opts(db)
	o.Deep = true

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cs := Run(ctx, o)
	got, detail, ok := level(cs, "database")
	if !ok {
		t.Fatal("missing database check")
	}
	if got != Fail {
		t.Errorf("cancelled context database check = %s (%q), want Fail", got, detail)
	}
	if strings.Contains(detail, "SQLite integrity check passed") {
		t.Errorf("cancelled context falsely reported integrity check passed: %q", detail)
	}
}

func TestDoctorEmptyDatabaseDeepCheck(t *testing.T) {
	db := newDB(t, t0)
	o := opts(db)
	o.Deep = true

	cs := Run(context.Background(), o)
	for _, c := range cs {
		if c.Level != Pass {
			t.Errorf("unexpected %s check %s: %s", c.Level, c.Name, c.Detail)
		}
	}
	if d := mustLevel(t, cs, "chain", Pass); !strings.Contains(d, "0 ops") {
		t.Errorf("expected 0 ops chain detail, got %q", d)
	}
	if d := mustLevel(t, cs, "view", Pass); !strings.Contains(d, "0 ops") {
		t.Errorf("expected 0 ops view detail, got %q", d)
	}
}
