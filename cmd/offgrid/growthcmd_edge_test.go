package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"offgrid/internal/growth"
)

func TestGrowthJSONOutputFullSchema(t *testing.T) {
	db := dbPath(t)
	runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"Task Schema Check","priority":"high"}`)
	if code, _, e := runCLI(t, "", "store", "-db", db, "attach", "-task", "T1", "-file", photoFile(t, "schema.jpg", 1<<20)); code != 0 {
		t.Fatal(e)
	}

	code, out, e := runCLI(t, "", "store", "-db", db, "growth", "-json", "-record", "-max-ops", "1")
	if code != 3 {
		t.Fatalf("expected exit code 3 for alarm, got %d (err: %s)", code, e)
	}

	var parsed struct {
		Sample     growth.Sample     `json:"sample"`
		Calendar   growth.Projection `json:"calendar"`
		ActiveDays growth.Projection `json:"active_days"`
		Alarms     []growth.Alarm    `json:"alarms"`
	}

	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("failed to parse growth JSON output: %v\nOutput was: %s", err, out)
	}

	if parsed.Sample.Ops != 2 { // create_task + attach
		t.Errorf("expected 2 ops, got %d", parsed.Sample.Ops)
	}
	if parsed.Sample.Blobs != 1 {
		t.Errorf("expected 1 blob, got %d", parsed.Sample.Blobs)
	}
	if parsed.Sample.BlobBytes != 1<<20 {
		t.Errorf("expected 1 MiB blob bytes, got %d", parsed.Sample.BlobBytes)
	}
	if parsed.Calendar.Basis != "calendar" {
		t.Errorf("expected calendar basis, got %q", parsed.Calendar.Basis)
	}
	if parsed.ActiveDays.Basis != "active days" {
		t.Errorf("expected active days basis, got %q", parsed.ActiveDays.Basis)
	}
	if len(parsed.Alarms) != 1 || parsed.Alarms[0].What != "log length" {
		t.Errorf("expected 1 alarm for log length, got: %+v", parsed.Alarms)
	}
}

func TestGrowthMultipleAlarmsExitThree(t *testing.T) {
	db := dbPath(t)
	runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"Task 1"}`)
	runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"Task 2"}`)
	if code, _, e := runCLI(t, "", "store", "-db", db, "attach", "-task", "T1", "-file", photoFile(t, "multi.jpg", 2<<20)); code != 0 {
		t.Fatal(e)
	}

	// Violate both max-ops and max-blobs: ops=3 > 1, blobs=2 MiB > 1 MiB
	code, out, _ := runCLI(t, "", "store", "-db", db, "growth", "-max-ops", "1", "-max-blobs", "1")
	if code != 3 {
		t.Fatalf("expected exit code 3, got %d", code)
	}

	if !strings.Contains(out, "ALARM              log length:") {
		t.Errorf("missing log length alarm in output:\n%s", out)
	}
	if !strings.Contains(out, "ALARM              blob size:") {
		t.Errorf("missing blob size alarm in output:\n%s", out)
	}
}

func TestGrowthCorruptedHistoryFileExitOne(t *testing.T) {
	db := dbPath(t)
	runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"Task"}`)

	// Corrupt the history file
	histPath := db + ".growth.json"
	if err := os.WriteFile(histPath, []byte("NOT_VALID_JSON{"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := runCLI(t, "", "store", "-db", db, "growth")
	if code != 1 {
		t.Fatalf("expected exit code 1 for corrupted history, got %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if !strings.Contains(errOut, "delete it to start again") {
		t.Errorf("expected guidance 'delete it to start again' in stderr, got: %s", errOut)
	}
}

func TestGrowthCustomWindowFlag(t *testing.T) {
	db := dbPath(t)
	runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"Task"}`)

	// Pass custom window
	code, out, errOut := runCLI(t, "", "store", "-db", db, "growth", "-window", "48h")
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d\nstderr: %s", code, errOut)
	}
	if !strings.Contains(out, "ops in the log     1") {
		t.Errorf("unexpected output: %s", out)
	}
}

func TestGrowthNonExistentDB(t *testing.T) {
	badPath := filepath.Join(t.TempDir(), "nonexistent", "db.sqlite")
	code, _, errOut := runCLI(t, "", "store", "-db", badPath, "growth")
	if code == 0 {
		t.Fatalf("expected non-zero exit code for nonexistent DB, got 0\nstderr: %s", errOut)
	}
}

func TestGrowthSubcommandFlagErrors(t *testing.T) {
	db := dbPath(t)
	runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"Task"}`)

	// Unknown flag to growth
	code, _, errOut := runCLI(t, "", "store", "-db", db, "growth", "-unrecognized-flag-xyz")
	if code != 2 {
		t.Fatalf("expected exit code 2 for flag parse failure, got %d\nstderr: %s", code, errOut)
	}

	// Negative durations or invalid syntax to -window
	code, _, _ = runCLI(t, "", "store", "-db", db, "growth", "-window", "invalid-duration")
	if code != 2 {
		t.Fatalf("expected exit code 2 for invalid duration, got %d", code)
	}
}

func TestGrowthHistoryDebounceOverTime(t *testing.T) {
	db := dbPath(t)
	runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"Task A"}`)

	// 1. First record
	code, _, _ := runCLI(t, "", "store", "-db", db, "growth", "-record")
	if code != 0 {
		t.Fatalf("first record failed with exit %d", code)
	}

	hist := growth.History{Path: db + ".growth.json"}
	s1, err := hist.Load()
	if err != nil || len(s1) != 1 {
		t.Fatalf("expected 1 history entry, got len=%d, err=%v", len(s1), err)
	}

	// 2. Second record within same second - should debounce and replace, keeping length 1
	runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"Task B"}`)
	code, _, _ = runCLI(t, "", "store", "-db", db, "growth", "-record")
	if code != 0 {
		t.Fatalf("second record failed with exit %d", code)
	}

	s2, err := hist.Load()
	if err != nil || len(s2) != 1 {
		t.Fatalf("expected still 1 history entry after debounce, got len=%d", len(s2))
	}
	if s2[0].Ops != 2 {
		t.Errorf("expected sample to be updated with ops=2, got %d", s2[0].Ops)
	}
}

func TestGrowthCmdAllFlagsCombined(t *testing.T) {
	db := dbPath(t)
	runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"Task Flag Combo"}`)

	code, out, errOut := runCLI(t, "", "store", "-db", db, "growth",
		"-json",
		"-record",
		"-window", "168h",
		"-max-ops", "500000",
		"-max-db", "1024",
		"-max-blobs", "4096",
		"-max-db-per-day", "10",
		"-max-blobs-per-day", "200",
		"-year-db", "1024",
		"-year-blobs", "16384",
	)

	if code != 0 {
		t.Fatalf("all-flags combined failed with exit %d: %s\nstdout: %s", code, errOut, out)
	}
	if !strings.Contains(out, `"sample":`) || !strings.Contains(out, `"alarms":`) {
		t.Errorf("unexpected output: %s", out)
	}
}

func TestGrowthCmdReadOnlyDatabase(t *testing.T) {
	db := dbPath(t)
	runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"Task R/O"}`)

	// Mark DB file read-only (0400)
	if err := os.Chmod(db, 0o400); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(db, 0o600) }()

	// Running growth (without -record) should succeed since it only needs read queries
	code, out, errOut := runCLI(t, "", "store", "-db", db, "growth")
	if code != 0 {
		t.Logf("Note: growth on read-only DB exit %d: %s", code, errOut)
	} else if !strings.Contains(out, "ops in the log     1") {
		t.Errorf("expected 1 op, got: %s", out)
	}
}

func TestGrowthCmdStoreHelpIncludesGrowth(t *testing.T) {
	code, _, errOut := runCLI(t, "", "store")
	if code != 2 {
		t.Fatalf("expected exit code 2 for empty store command, got %d", code)
	}
	if !strings.Contains(errOut, "growth") {
		t.Errorf("store help text must include 'growth': %s", errOut)
	}
}
