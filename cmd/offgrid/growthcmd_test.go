package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"offgrid/internal/growth"
)

func TestGrowthReportsSizesAndAlarmsWithExitThree(t *testing.T) {
	db := dbPath(t)
	runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"Leaking pump"}`)
	if code, _, e := runCLI(t, "", "store", "-db", db, "attach", "-task", "T1", "-file", photoFile(t, "p.jpg", 3<<20)); code != 0 {
		t.Fatal(e)
	}
	code, out, e := runCLI(t, "", "store", "-db", db, "growth")
	if code != 0 || !strings.Contains(out, "blob folder        3.00 MiB in 1 files") || !strings.Contains(out, "alarms             none") {
		t.Fatalf("%d\n%s\n%s", code, out, e)
	}
	if !strings.Contains(out, "projection         withheld") || strings.Contains(out, "one year (") || !strings.Contains(out, "cost per op") {
		t.Fatalf("a one-day log must say the projection is a guess:\n%s", out)
	}
	code, out, _ = runCLI(t, "", "store", "-db", db, "growth", "-max-blobs", "2")
	if code != 3 || !strings.Contains(out, "ALARM              blob size: the blob folder is 3.0 MiB, over your limit of 2.0 MiB") {
		t.Fatalf("%d\n%s", code, out)
	}
	if code, out, _ = runCLI(t, "", "store", "-db", db, "growth", "-max-ops", "1"); code != 3 || !strings.Contains(out, "log length: the log holds 2 ops, over your limit of 1") {
		t.Fatalf("%d\n%s", code, out)
	}
	if code, _, _ = runCLI(t, "", "store", "-db", db, "growth", "-max-ops", "2"); code != 0 {
		t.Fatalf("at the ops limit must pass, got %d", code)
	}
	if code, _, _ = runCLI(t, "", "store", "-db", db, "growth", "-max-blobs", "3"); code != 0 {
		t.Fatalf("at the limit must pass, got %d", code)
	}
}

func TestGrowthJSONAndRecord(t *testing.T) {
	db := dbPath(t)
	runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"a"}`)
	code, out, e := runCLI(t, "", "store", "-db", db, "growth", "-json", "-record")
	if code != 0 {
		t.Fatal(e)
	}
	var v struct {
		Sample struct {
			Ops     int64 `json:"ops"`
			DBBytes int64 `json:"db_bytes"`
		} `json:"sample"`
		Alarms []any `json:"alarms"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil || v.Sample.Ops != 1 || v.Sample.DBBytes <= 0 {
		t.Fatalf("%v %s", err, out)
	}
	st, err := os.Stat(db + ".growth.json")
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("history file: %v %v", st, err)
	}
	if code, _, _ := runCLI(t, "", "store", "-db", db, "growth", "-nonsense"); code != 2 {
		t.Fatalf("bad flag exit %d", code)
	}
}

func TestGrowthOnAnEmptyLog(t *testing.T) {
	db := dbPath(t)
	runCLI(t, "", "exec", "-db", db, `{"intent":"list_tasks"}`)
	code, out, _ := runCLI(t, "", "store", "-db", db, "growth")
	if code != 0 || !strings.Contains(out, "ops in the log     0") || !strings.Contains(out, "nothing to project") {
		t.Fatalf("%d\n%s", code, out)
	}
}

func TestGrowthPrintsAProjectionOnceThereIsAWeekOfHistory(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	s := growth.Sample{Ops: 3000, OpBytes: 1 << 20, DBBytes: 30 << 20, BlobBytes: 60 << 20, Blobs: 20,
		First: t0, Last: t0.Add(30 * 24 * time.Hour), ActiveDays: 30}
	var b strings.Builder
	printGrowth(&b, s, growth.Project(s, true), growth.Project(s, false), nil)
	out := b.String()
	if !strings.Contains(out, "one year (calendar, 30.0 days): +36500 ops, database +365.00 MiB (to 395.00 MiB), blobs +730.00 MiB (to 790.00 MiB)") || strings.Contains(out, "withheld") {
		t.Fatal(out)
	}
}
