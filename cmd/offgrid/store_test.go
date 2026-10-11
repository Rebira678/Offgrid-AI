package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func dbPath(t *testing.T) string { return filepath.Join(t.TempDir(), "offgrid.db") }

func runCLI(t *testing.T, stdin string, args ...string) (code int, out, errOut string) {
	t.Helper()
	var o, e strings.Builder
	code = runIn(args, strings.NewReader(stdin), &o, &e)
	return code, o.String(), e.String()
}

func TestTasksSurviveBetweenRunsWithDB(t *testing.T) {
	db := dbPath(t)
	code, out, errOut := runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"Leaking pump","site":"4","priority":"high"}`)
	if code != 0 || !strings.Contains(out, "Created T1") {
		t.Fatalf("first run: exit %d\n%s\n%s", code, out, errOut)
	}
	// A completely separate run sees it, and numbering continues.
	code, out, _ = runCLI(t, "", "exec", "-db", db, `{"intent":"list_tasks"}`, `{"intent":"create_task","title":"Replace filter"}`)
	if code != 0 || !strings.Contains(out, "T1 [high, open] Leaking pump (site 4)") || !strings.Contains(out, "Created T2") {
		t.Fatalf("second run:\n%s", out)
	}
	// Without -db nothing is kept.
	_, out, _ = runCLI(t, "", "exec", `{"intent":"list_tasks"}`)
	if !strings.Contains(out, "No matching tasks.") {
		t.Errorf("a run without -db saw tasks:\n%s", out)
	}
}

func TestAgentKeepsTasksWithDBAndUsesTheEnvironmentVariable(t *testing.T) {
	db := dbPath(t)
	t.Setenv("OFFGRID_DB", db) // no -db flag anywhere below
	if code, out, _ := runCLI(t, "", "agent", "-no-model", "new task: check the fuel line, site 2, high priority"); code != 0 || !strings.Contains(out, "Created T1") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	_, out, _ := runCLI(t, "", "agent", "-no-model", "what's still open?")
	if !strings.Contains(out, "check the fuel line") || !strings.Contains(out, "tier 0 (rules)") {
		t.Errorf("the second run did not see the first run's task:\n%s", out)
	}
	if _, err := os.Stat(db); err != nil {
		t.Errorf("OFFGRID_DB was not used: %v", err)
	}
}

func TestDBCreatesItsFolder(t *testing.T) {
	db := filepath.Join(t.TempDir(), "a", "b", "offgrid.db")
	if code, _, errOut := runCLI(t, "", "exec", "-db", db, `{"intent":"list_tasks"}`); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if _, err := os.Stat(db); err != nil {
		t.Errorf("database not created: %v", err)
	}
}

func TestSeedWithDBDoesNotDuplicateOnTheSecondRun(t *testing.T) {
	db := dbPath(t)
	for i := 0; i < 2; i++ {
		runCLI(t, "", "exec", "-db", db, "-seed", "-now", "2026-10-08T09:00", `{"intent":"list_tasks"}`)
	}
	_, out, _ := runCLI(t, "", "exec", "-db", db, "-now", "2026-10-08T09:00", `{"intent":"list_tasks"}`)
	if !strings.Contains(out, "3 tasks") {
		t.Errorf("seeding twice must not duplicate the tasks:\n%s", out)
	}
}

func TestLongSessionsRecordMovingTimeButFixedNowIsFixed(t *testing.T) {
	// With -now, every op records exactly that instant.
	db := dbPath(t)
	runCLI(t, "", "exec", "-db", db, "-now", "2026-10-08T09:00", `{"intent":"create_task","title":"a"}`, `{"intent":"add_note","task_ref":"a","note":"n"}`)
	_, out, _ := runCLI(t, "", "store", "-db", db, "ops")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("ops:\n%s", out)
	}
	var a, b struct {
		TS string `json:"ts"`
	}
	json.Unmarshal([]byte(lines[0]), &a)
	json.Unmarshal([]byte(lines[1]), &b)
	if a.TS == "" || a.TS != b.TS {
		t.Errorf("with -now both ops must carry the same instant: %q vs %q", a.TS, b.TS)
	}
}

func TestStoreInfoVerifyOpsOnAHealthyDatabase(t *testing.T) {
	db := dbPath(t)
	runCLI(t, "", "exec", "-db", db, "-now", "2026-10-08T09:00",
		`{"intent":"create_task","title":"Leaking pump"}`, `{"intent":"add_note","task_ref":"pump","note":"cracked"}`)

	code, out, _ := runCLI(t, "", "store", "-db", db, "info")
	for _, want := range []string{"node id", "schema version     2", "ops in the log     2", "tasks in the view  1", "state hash"} {
		if code != 0 || !strings.Contains(out, want) {
			t.Errorf("info lacks %q (exit %d):\n%s", want, code, out)
		}
	}
	code, out, _ = runCLI(t, "", "store", "-db", db, "verify")
	if code != 0 || !strings.Contains(out, "ok: replayed 2 ops") {
		t.Errorf("verify: exit %d\n%s", code, out)
	}
	code, out, _ = runCLI(t, "", "store", "-db", db, "-n", "1", "ops")
	if code != 0 || strings.Count(strings.TrimSpace(out), "\n") != 0 || !strings.Contains(out, `"kind":"task.note_added"`) {
		t.Errorf("ops -n 1: exit %d\n%s", code, out)
	}
}

// The headline requirement, through the real command line: damage the view,
// see verify notice, rebuild from the log, see the state come back.
func TestStoreRebuildRepairsADeletedView(t *testing.T) {
	db := dbPath(t)
	runCLI(t, "", "exec", "-db", db, "-now", "2026-10-08T09:00",
		`{"intent":"create_task","title":"Leaking pump","site":"4","priority":"high"}`,
		`{"intent":"create_task","title":"Replace filter"}`,
		`{"intent":"add_note","task_ref":"pump","note":"cracked"}`,
		`{"intent":"update_task","task_ref":"filter","status":"done"}`)
	_, before, _ := runCLI(t, "", "store", "-db", db, "info")

	raw, err := sql.Open("sqlite3", db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP TABLE task_notes; DROP TABLE tasks;`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	// Opening notices a missing view and rebuilds it from the log by itself.
	_, mid, _ := runCLI(t, "", "store", "-db", db, "info")
	if hashLine(mid) != hashLine(before) || hashLine(mid) == "" {
		t.Fatalf("opening a database whose view was deleted did not restore it:\nbefore: %s\nafter:  %s", hashLine(before), hashLine(mid))
	}

	// An edited row, though, is something Open cannot notice. Verify can.
	raw, _ = sql.Open("sqlite3", db)
	raw.Exec(`UPDATE tasks SET title = 'tampered' WHERE id = 'T1'`)
	raw.Close()
	code, _, errOut := runCLI(t, "", "store", "-db", db, "verify")
	if code != 1 || !strings.Contains(errOut, "MISMATCH") || !strings.Contains(errOut, "rebuild") {
		t.Fatalf("verify of a tampered view: exit %d\n%s", code, errOut)
	}
	code, out, _ := runCLI(t, "", "store", "-db", db, "rebuild")
	if code != 0 || !strings.Contains(out, "rebuilt the tasks view from 4 ops: 2 tasks") || !strings.Contains(out, "CHANGED") {
		t.Fatalf("rebuild: exit %d\n%s", code, out)
	}
	_, after, _ := runCLI(t, "", "store", "-db", db, "info")
	if hashLine(before) != hashLine(after) || hashLine(after) == "" {
		t.Errorf("state hash after rebuild differs:\nbefore: %s\nafter:  %s", hashLine(before), hashLine(after))
	}
	if code, _, _ := runCLI(t, "", "store", "-db", db, "verify"); code != 0 {
		t.Errorf("verify after rebuild: exit %d", code)
	}
	// Rebuilding a healthy database changes nothing.
	_, out, _ = runCLI(t, "", "store", "-db", db, "rebuild")
	if !strings.Contains(out, "identical") {
		t.Errorf("a healthy rebuild should say identical:\n%s", out)
	}
	// The tasks are really back.
	_, out, _ = runCLI(t, "", "exec", "-db", db, "-now", "2026-10-08T09:00", `{"intent":"list_tasks","status":"done"}`)
	if !strings.Contains(out, "Replace filter") {
		t.Errorf("tasks missing after rebuild:\n%s", out)
	}
}

func hashLine(info string) string {
	for _, l := range strings.Split(info, "\n") {
		if strings.HasPrefix(l, "state hash") {
			return strings.TrimSpace(strings.TrimPrefix(l, "state hash"))
		}
	}
	return ""
}

func TestStoreCommandErrors(t *testing.T) {
	db := dbPath(t)
	cases := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"no subcommand", []string{"store"}, 2, "usage"},
		{"unknown subcommand", []string{"store", "-db", db, "explode"}, 2, "unknown store command"},
		{"no database given", []string{"store", "info"}, 2, "no database given"},
		{"database missing", []string{"store", "-db", db, "info"}, 1, "no database at"},
		{"extra argument", []string{"store", "-db", db, "info", "now"}, 2, "usage"},
	}
	t.Setenv("OFFGRID_DB", "")
	for _, c := range cases {
		code, _, errOut := runCLI(t, "", c.args...)
		if code != c.code || !strings.Contains(errOut, c.want) {
			t.Errorf("%s: exit %d, stderr %q; want exit %d containing %q", c.name, code, errOut, c.code, c.want)
		}
	}
	if _, err := os.Stat(db); err == nil {
		t.Error("a failed `store info` created a database")
	}
}

func TestStoreRefusesADatabaseWithACorruptLog(t *testing.T) {
	db := dbPath(t)
	runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"a"}`)
	raw, _ := sql.Open("sqlite3", db)
	raw.Exec(`INSERT INTO ops (node_id, seq, ts, kind, payload) VALUES ('x', 1, 0, 'task.exploded', '{}')`)
	raw.Close()
	code, _, errOut := runCLI(t, "", "store", "-db", db, "info")
	if code != 1 || !strings.Contains(errOut, "corrupt") || !strings.Contains(errOut, "never edited automatically") {
		t.Errorf("exit %d\n%s", code, errOut)
	}
	// exec refuses too: it must not carry on with a log it cannot trust.
	if code, _, errOut := runCLI(t, "", "exec", "-db", db, `{"intent":"list_tasks"}`); code != 1 || !strings.Contains(errOut, "corrupt") {
		t.Errorf("exec on a corrupt log: exit %d\n%s", code, errOut)
	}
}

func TestUsageListsTheStoreCommandAndDB(t *testing.T) {
	_, out, _ := runCLI(t, "", "help")
	for _, want := range []string{"offgrid store", "-db FILE", "OFFGRID_DB"} {
		if !strings.Contains(out, want) {
			t.Errorf("usage lacks %q", want)
		}
	}
}

// The Day 4 demo script exercises every tool and every refusal. Run it against
// memory and against SQLite with the same pinned clock: the output must be
// identical, character for character. This is the whole stack (tools,
// resolver, dedupe, summary, time ranges) agreeing across both stores.
func TestTheDay4DemoScriptGivesIdenticalOutputOnSQLiteAndInMemory(t *testing.T) {
	script := filepath.Join("..", "..", "examples", "day4-commands.jsonl")
	if _, err := os.Stat(script); err != nil {
		t.Skip("examples/ not present")
	}
	_, mem, _ := runCLI(t, "", "exec", "-now", "2026-10-08T09:00", "-script", script)
	code, persisted, errOut := runCLI(t, "", "exec", "-now", "2026-10-08T09:00", "-db", dbPath(t), "-script", script)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if mem != persisted {
		ml, pl := strings.Split(mem, "\n"), strings.Split(persisted, "\n")
		for i := range ml {
			if i >= len(pl) || ml[i] != pl[i] {
				t.Fatalf("outputs differ at line %d:\n  memory: %q\n  sqlite: %q", i+1, ml[i], pl[min(i, len(pl)-1)])
			}
		}
		t.Fatal("outputs differ in length")
	}
	if !strings.Contains(mem, "10 accepted, 10 refused") {
		t.Errorf("the demo did not run as expected:\n%s", mem)
	}
}

// The same for the Day 5 messages through the agent, rules only.
func TestTheDay5DemoGivesIdenticalOutputOnSQLiteAndInMemory(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", "day5-messages.txt"))
	if err != nil {
		t.Skip("examples/ not present")
	}
	strip := func(s string) string { // latencies differ run to run; everything else must match
		var keep []string
		for _, l := range strings.Split(s, "\n") {
			if i := strings.Index(l, ", "); strings.HasPrefix(strings.TrimSpace(l), "[") && i > 0 {
				if j := strings.LastIndex(l, "] "); j > i {
					l = l[:i] + l[j:]
				}
			}
			if strings.HasPrefix(l, "tier") || strings.HasPrefix(l, "none") || strings.HasPrefix(l, "TIER") {
				continue // the latency table
			}
			keep = append(keep, l)
		}
		return strings.Join(keep, "\n")
	}
	_, mem, _ := runCLI(t, string(data), "agent", "-no-model", "-seed", "-now", "2026-10-08T09:00")
	_, persisted, _ := runCLI(t, string(data), "agent", "-no-model", "-seed", "-now", "2026-10-08T09:00", "-db", dbPath(t))
	if a, b := strip(mem), strip(persisted); a != b {
		t.Errorf("outputs differ:\n--- memory\n%s\n--- sqlite\n%s", a, b)
	}
}
