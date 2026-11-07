package doctor

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"offgrid/internal/hlc"
	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
)

var t0 = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

// opts is a healthy machine: every seam says "fine".
func opts(db string) Options {
	return Options{
		DBPath:     db,
		Version:    "test",
		Now:        func() time.Time { return t0 },
		ExePath:    func() (string, error) { return "", errors.New("unknown") },
		FreeBytes:  func(string) (uint64, error) { return 100 << 30, nil },
		TryListen:  func(string) error { return nil },
		NTP:        func(context.Context) (bool, bool) { return true, true },
		RTCPresent: func() (bool, bool) { return true, true },
		BlockNames: func() []string { return []string{"nvme0n1"} },
	}
}

func level(cs []Check, name string) (Level, string, bool) {
	worst, detail, found := Pass, "", false
	for _, c := range cs {
		if c.Name == name {
			if !found || c.Level > worst {
				worst, detail = c.Level, c.Detail
			}
			found = true
		}
	}
	return worst, detail, found
}

func mustLevel(t *testing.T, cs []Check, name string, want Level) string {
	t.Helper()
	got, detail, ok := level(cs, name)
	if !ok {
		t.Fatalf("no %q check in %+v", name, cs)
	}
	if got != want {
		t.Errorf("%s: %s, want %s (%s)", name, got, want, detail)
	}
	return detail
}

// newDB makes a database whose ops are stamped from the given clock.
func newDB(t *testing.T, now time.Time, titles ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tasks.db")
	st, err := sqlstore.Open(context.Background(), path, sqlstore.Options{NodeID: "node-t", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	for _, title := range titles {
		if _, _, err := st.Create(context.Background(), task.NewTask{Title: title, Priority: task.PriorityLow}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHealthyMachineAndDatabasePass(t *testing.T) {
	db := newDB(t, t0, "a", "b")
	cs := Run(context.Background(), opts(db))
	for _, c := range cs {
		if c.Level != Pass {
			t.Errorf("%s %s: %s", c.Level, c.Name, c.Detail)
		}
	}
	for _, name := range []string{"version", "clock", "disk", "database", "chain"} {
		mustLevel(t, cs, name, Pass)
	}
	if d := mustLevel(t, cs, "database", Pass); !strings.Contains(d, "2 ops") {
		t.Errorf("database detail %q", d)
	}
}

func TestClockBehindTheLog(t *testing.T) {
	cases := []struct {
		name  string
		ahead time.Duration
		want  Level
	}{
		{"in step", 0, Pass},
		{"30s ahead is noise", 30 * time.Second, Pass},
		{"5 minutes ahead", 5 * time.Minute, Warn},
		{"one hour exactly", hlc.DefaultMaxDrift, Warn},
		{"a day ahead: peers will refuse new edits", 24 * time.Hour, Fail},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := newDB(t, t0.Add(c.ahead), "x")
			cs := Run(context.Background(), opts(db))
			d := mustLevel(t, cs, "clock", c.want)
			if c.want == Fail && !strings.Contains(d, "refuse") {
				t.Errorf("detail %q does not say what breaks", d)
			}
		})
	}
}

func TestClockBeforeTheBinaryWasInstalled(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "offgrid")
	if err := os.WriteFile(exe, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	built := t0.Add(48 * time.Hour) // the file claims to be from two days after "now"
	if err := os.Chtimes(exe, built, built); err != nil {
		t.Fatal(err)
	}
	o := opts("")
	o.ExePath = func() (string, error) { return exe, nil }
	mustLevel(t, Run(context.Background(), o), "clock", Fail)

	o.Now = func() time.Time { return built.Add(time.Hour) }
	mustLevel(t, Run(context.Background(), o), "clock", Pass)
}

func TestClockSources(t *testing.T) {
	o := opts("")
	o.NTP = func(context.Context) (bool, bool) { return false, true }
	o.RTCPresent = func() (bool, bool) { return false, true }
	cs := Run(context.Background(), o)
	n := 0
	for _, c := range cs {
		if c.Name == "clock" && c.Level == Warn {
			n++
		}
	}
	if n != 2 {
		t.Errorf("want a warning for no time server and one for no hardware clock, got %d: %+v", n, cs)
	}
	// Unknown (no timedatectl, no /sys) is silence, not a warning.
	o.NTP = func(context.Context) (bool, bool) { return false, false }
	o.RTCPresent = func() (bool, bool) { return false, false }
	mustLevel(t, Run(context.Background(), o), "clock", Pass)
}

func TestDiskThresholds(t *testing.T) {
	for _, c := range []struct {
		free uint64
		want Level
	}{
		{10 << 20, Fail},
		{49 << 20, Fail},
		{50 << 20, Warn},
		{499 << 20, Warn},
		{500 << 20, Pass},
	} {
		o := opts("")
		o.FreeBytes = func(string) (uint64, error) { return c.free, nil }
		mustLevel(t, Run(context.Background(), o), "disk", c.want)
	}
	o := opts("")
	o.FreeBytes = func(string) (uint64, error) { return 0, errors.New("boom") }
	mustLevel(t, Run(context.Background(), o), "disk", Warn)
}

func TestSDCardOnlyMachineIsWarned(t *testing.T) {
	for _, c := range []struct {
		blocks []string
		warn   bool
	}{
		{[]string{"mmcblk0", "loop0", "zram0"}, true},
		{[]string{"mmcblk0", "mmcblk0boot0", "mmcblk0boot1"}, true},
		{[]string{"mmcblk0", "sda"}, false},
		{[]string{"nvme0n1"}, false},
		{nil, false},
	} {
		o := opts("")
		o.BlockNames = func() []string { return c.blocks }
		_, _, found := level(Run(context.Background(), o), "storage")
		if found != c.warn {
			t.Errorf("%v: warned = %v, want %v", c.blocks, found, c.warn)
		}
	}
}

func TestPortInUse(t *testing.T) {
	o := opts("")
	o.Listen = []string{"127.0.0.1:1", "127.0.0.1:2"}
	o.TryListen = func(a string) error {
		if a == "127.0.0.1:2" {
			return errors.New("address already in use")
		}
		return nil
	}
	cs := Run(context.Background(), o)
	var passes, warns int
	for _, c := range cs {
		if c.Name == "port" {
			if c.Level == Pass {
				passes++
			}
			if c.Level == Warn && strings.Contains(c.Detail, "127.0.0.1:2") {
				warns++
			}
		}
	}
	if passes != 1 || warns != 1 {
		t.Errorf("passes %d warns %d: %+v", passes, warns, cs)
	}
}

func TestMissingDatabaseIsAFailureAndIsNotCreated(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "nope.db")
	d := mustLevel(t, Run(context.Background(), opts(db)), "database", Fail)
	if !strings.Contains(d, "no such database") {
		t.Errorf("detail %q", d)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("doctor created %d files", len(ents))
	}
}

func TestACorruptDatabaseFileIsFound(t *testing.T) {
	db := newDB(t, t0, "a", "b", "c", "d", "e")
	raw, err := os.ReadFile(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 3*4096 {
		t.Skipf("database only %d bytes", len(raw))
	}
	// Smash the middle of the file: the header still opens, a page does not.
	for i := 4096; i < 3*4096 && i < len(raw); i++ {
		raw[i] = 0xFF
	}
	if err := os.WriteFile(db, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	cs := Run(context.Background(), opts(db))
	got, d, _ := level(cs, "database")
	if got != Fail {
		t.Errorf("a damaged file passed: %s", d)
	}
}

func TestLeftoverWALAfterAKillIsNotAnError(t *testing.T) {
	db := newDB(t, t0, "a")
	// A second connection keeps a write-ahead log on disk the way a killed process would.
	raw, err := sql.Open("sqlite3", db)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetMaxOpenConns(1)
	if _, err := raw.Exec(`PRAGMA wal_autocheckpoint=0`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE IF NOT EXISTS scratch (x)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO scratch VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(db + "-wal"); err != nil || fi.Size() == 0 {
		t.Skip("no write-ahead log on disk to test with")
	}
	d := mustLevel(t, Run(context.Background(), opts(db)), "database", Pass)
	if !strings.Contains(d, "write-ahead log") {
		t.Errorf("detail %q should mention the log", d)
	}
}

func TestDeepCheckFindsAnEditedLog(t *testing.T) {
	db := newDB(t, t0, "a", "b")
	o := opts(db)
	o.Deep = true
	mustLevel(t, Run(context.Background(), o), "chain", Pass)

	// Edit an op the way the tamper demo does: change its payload behind the store's back.
	// The append-only triggers block UPDATE, so drop that one and put it back.
	raw, err := sql.Open("sqlite3", db)
	if err != nil {
		t.Fatal(err)
	}
	var trig string
	if err := raw.QueryRow(`SELECT name FROM sqlite_master WHERE type='trigger' AND tbl_name='ops' AND sql LIKE '%UPDATE%' LIMIT 1`).Scan(&trig); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP TRIGGER ` + trig); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE ops SET payload = replace(payload, '"a"', '"z"') WHERE seq = 1`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	cs := Run(context.Background(), o)
	d := mustLevel(t, cs, "chain", Fail)
	if !strings.Contains(d, "node-t") {
		t.Errorf("detail %q does not name the replica", d)
	}
	// Without -deep it is not looked for, and says so.
	o.Deep = false
	if _, d, _ := level(Run(context.Background(), o), "chain"); !strings.Contains(d, "not checked") {
		t.Errorf("shallow chain detail %q", d)
	}
}

func TestPrintAndSummarize(t *testing.T) {
	cs := []Check{
		mk("a", Pass, "fine", ""),
		mk("b", Warn, "meh", "do x"),
		mk("c", Fail, "bad", "do y"),
	}
	p, w, f := Summarize(cs)
	if p != 1 || w != 1 || f != 1 {
		t.Errorf("summary %d %d %d", p, w, f)
	}
	var buf bytes.Buffer
	Print(&buf, cs)
	out := buf.String()
	for _, want := range []string{"PASS  a", "WARN  b", "FAIL  c", "fix: do x", "fix: do y", "1 passed, 1 warnings, 1 failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "fix: ") && strings.Count(out, "fix: ") != 2 {
		t.Errorf("a passing check printed a fix:\n%s", out)
	}
}

func TestRealMachineSmoke(t *testing.T) {
	// No seams: it must run on whatever this machine is, and the version line must be real.
	cs := Run(context.Background(), Options{Version: "smoke"})
	d := mustLevel(t, cs, "version", Pass)
	if !strings.Contains(d, "sqlite 3.") {
		t.Errorf("version detail %q has no SQLite version", d)
	}
}
