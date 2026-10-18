package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"offgrid/internal/obs"
)

func runTop(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(append([]string{"top"}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

func readSnaps(t *testing.T, dir string) map[string]obs.Snapshot {
	t.Helper()
	snaps, problems := obs.ReadSnapshots(dir)
	if len(problems) != 0 {
		t.Fatalf("snapshot problems: %v", problems)
	}
	m := map[string]obs.Snapshot{}
	for _, s := range snaps {
		m[s.Proc] = s
	}
	return m
}

// The Day 13 "done when", through the real commands: a person saves two
// requests offline, looks at the screen, drains, looks again.
func TestTopShowsTheQueueBeforeAndAfterADrain(t *testing.T) {
	useFakeModels(t)
	db, queue := fill(t)

	code, out, e := runTop(t, "-queue", queue, "-once", "-color", "never")
	if code != 0 || e != "" {
		t.Fatalf("exit %d: %s", code, e)
	}
	for _, want := range []string{
		"OFFGRID", "CONNECTION", "QUEUE", "TIERS", "RECENT",
		"queued 2", "running 0", "done 0",
		"OFFLINE", "not probed", // forced by -net offline, and said so
		"deferred",        // both messages were routed to the queue, by tier
		"○ agent stopped", // the fill process has exited
		"last known: agent stopped",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("before the drain, the screen is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "● agent") {
		t.Errorf("a finished process must not be shown as running:\n%s", out)
	}

	if code, o, e := runDrain(t, "-db", db, "-queue", queue, "-net", "offline", "-once", "-poll", "10ms"); code != 0 {
		t.Fatalf("drain: %d %s %s", code, e, o)
	}
	_, out, _ = runTop(t, "-queue", queue, "-once", "-color", "never")
	for _, want := range []string{"queued 0", "done 2", "deep_think", "interpret", "○ drain stopped", "#1 deep_think done", "#2 interpret done"} {
		if !strings.Contains(out, want) {
			t.Errorf("after the drain, the screen is missing %q:\n%s", want, out)
		}
	}
	// the counters agree with the database
	d := readSnaps(t, filepath.Join(filepath.Dir(queue), "metrics"))["drain"]
	if d.Jobs["deep_think"].Done != 1 || d.Jobs["interpret"].Done != 1 || !d.Stopped {
		t.Errorf("%+v", d.Jobs)
	}
	if d.Queue == nil || d.Queue.Done != 2 || d.Queue.Queued != 0 {
		t.Errorf("the final snapshot must carry the final queue depth: %+v", d.Queue)
	}
	if l := d.JobLatency["deep_think"]; l.N != 1 {
		t.Errorf("latency %+v", l)
	}
}

func TestTopOnlyReadsTheQueue(t *testing.T) {
	_, queue := fill(t)
	sum := func() [32]byte {
		b, err := os.ReadFile(queue)
		if err != nil {
			t.Fatal(err)
		}
		return sha256.Sum256(b)
	}
	// SQLite may add its own -shm/-wal sidecar files when a WAL database is
	// read; anything else appearing would be a bug.
	list := func() string {
		ents, _ := os.ReadDir(filepath.Dir(queue))
		var names []string
		for _, e := range ents {
			if n := e.Name(); !strings.HasSuffix(n, "-shm") && !strings.HasSuffix(n, "-wal") {
				names = append(names, n)
			}
		}
		return strings.Join(names, ",")
	}
	before, files := sum(), list()
	for i := 0; i < 3; i++ {
		if code, _, e := runTop(t, "-queue", queue, "-once"); code != 0 {
			t.Fatalf("%d %s", code, e)
		}
	}
	if sum() != before {
		t.Error("top changed the queue file")
	}
	if got := list(); got != files {
		t.Errorf("top left files behind: %s -> %s", files, got)
	}
	if v := queueKinds(t, queue); !strings.Contains(v, "queued") {
		t.Errorf("the queue is still usable: %s", v)
	}
}

func TestUseColor(t *testing.T) {
	cases := []struct {
		mode     string
		tty      bool
		noColor  string
		want     bool
		wantFail bool
	}{
		{"auto", true, "", true, false},
		{"auto", false, "", false, false},
		{"auto", true, "1", false, false}, // NO_COLOR wins in auto mode
		{"always", false, "1", true, false},
		{"never", true, "", false, false},
		{"rainbow", true, "", false, true},
	}
	for _, c := range cases {
		got, err := useColor(c.mode, c.tty, c.noColor)
		if got != c.want || (err != nil) != c.wantFail {
			t.Errorf("%+v: got %v, %v", c, got, err)
		}
	}
}

func TestTopDoesNotCreateAMissingQueueInAnExistingFolder(t *testing.T) {
	t.Setenv("OFFGRID_QUEUE", "")
	t.Setenv("OFFGRID_DB", "")
	q := filepath.Join(t.TempDir(), "queue.db") // the folder exists, the file does not
	runTop(t, "-queue", q, "-once")
	runTop(t, "-queue", q, "-once")
	if _, err := os.Stat(q); err == nil {
		t.Error("looking at a queue that is not there must not make one")
	}
}

func TestTopWithoutAQueueSaysSoInsteadOfCreatingOne(t *testing.T) {
	t.Setenv("OFFGRID_QUEUE", "")
	t.Setenv("OFFGRID_DB", "")
	q := filepath.Join(t.TempDir(), "nope", "queue.db")
	code, out, _ := runTop(t, "-queue", q, "-once")
	if code != 0 || !strings.Contains(out, "queue unreadable: no queue yet at ") {
		t.Errorf("%d\n%s", code, out)
	}
	if _, err := os.Stat(q); err == nil {
		t.Error("a viewer must never create the queue")
	}
	if _, err := os.Stat(filepath.Dir(q)); err == nil {
		t.Error("nor its folder")
	}
}

func TestTopFlagsAnUnreadableSnapshotButKeepsDrawing(t *testing.T) {
	_, queue := fill(t)
	dir := filepath.Join(filepath.Dir(queue), "metrics")
	os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{oops"), 0o644)
	_, out, _ := runTop(t, "-queue", queue, "-once")
	if !strings.Contains(out, "! broken.json: not a snapshot") || !strings.Contains(out, "queued 2") {
		t.Errorf("\n%s", out)
	}
}

func TestTopShowsAProcessThatWentSilent(t *testing.T) {
	_, queue := fill(t)
	dir := filepath.Join(filepath.Dir(queue), "metrics")
	// a process that was alive but has not written for a long time (a crash)
	b, _ := os.ReadFile(filepath.Join(dir, "agent.json"))
	var s obs.Snapshot
	json.Unmarshal(b, &s)
	s.Stopped = false
	s.Net = "ONLINE"
	s.At = s.At.Add(-time.Hour)
	WriteSnap(t, dir, s)
	_, out, _ := runTop(t, "-queue", queue, "-once")
	if !strings.Contains(out, "○ agent silent 1h00m") || !strings.Contains(out, "last known: agent went silent") {
		t.Errorf("a crashed process must be shown as silent, not as live:\n%s", out)
	}
}

func WriteSnap(t *testing.T, dir string, s obs.Snapshot) {
	t.Helper()
	if err := obs.WriteSnapshot(obs.SnapshotPath(dir, s.Proc), s); err != nil {
		t.Fatal(err)
	}
}

func TestTopColourOptions(t *testing.T) {
	_, queue := fill(t)
	_, plain, _ := runTop(t, "-queue", queue, "-once")
	if strings.Contains(plain, "\x1b") {
		t.Error("auto colour off when output is not a terminal")
	}
	_, colour, _ := runTop(t, "-queue", queue, "-once", "-color", "always")
	if !strings.Contains(colour, "\x1b[") {
		t.Error("-color always")
	}
	_, never, _ := runTop(t, "-queue", queue, "-once", "-color", "never")
	if strings.Contains(never, "\x1b") {
		t.Error("-color never")
	}
}

func TestTopWidth(t *testing.T) {
	_, queue := fill(t)
	_, out, _ := runTop(t, "-queue", queue, "-once", "-width", "100")
	if w := len([]rune(strings.Split(out, "\n")[1])); w != 100 {
		t.Errorf("width %d", w)
	}
}

func TestTopUsageErrors(t *testing.T) {
	t.Setenv("OFFGRID_QUEUE", "")
	t.Setenv("OFFGRID_DB", "")
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"-once"}, "no queue file and no metrics directory"},
		{[]string{"-queue", "q.db", "-interval", "10ms"}, "-interval must be at least 100ms"},
		{[]string{"-queue", "q.db", "-width", "10"}, "-width must be at least 64"},
		{[]string{"-queue", "q.db", "-width", "63"}, "-width must be at least 64"},
		{[]string{"-queue", "q.db", "-interval", "99ms"}, "-interval must be at least 100ms"},
		{[]string{"-queue", "q.db", "-color", "purple"}, "-color must be auto, always or never"},
		{[]string{"-queue", "q.db", "extra"}, `unexpected argument "extra"`},
		{[]string{"-nonsense"}, "flag provided but not defined"},
	}
	for _, c := range cases {
		code, _, e := runTop(t, c.args...)
		if code != 2 || !strings.Contains(e, c.want) {
			t.Errorf("%v: exit %d, stderr %q, want %q", c.args, code, e, c.want)
		}
	}
}

func TestTopUsesTheEnvironmentForTheQueueAndMetricsOverride(t *testing.T) {
	_, queue := fill(t)
	t.Setenv("OFFGRID_QUEUE", queue)
	_, out, _ := runTop(t, "-once")
	if !strings.Contains(out, "queued 2") {
		t.Errorf("$OFFGRID_QUEUE ignored:\n%s", out)
	}
	// -metrics points somewhere else entirely: no processes there
	_, out, _ = runTop(t, "-once", "-metrics", t.TempDir())
	if !strings.Contains(out, "no process is publishing") || !strings.Contains(out, "queued 2") {
		t.Errorf("\n%s", out)
	}
	// metrics only, no queue
	t.Setenv("OFFGRID_QUEUE", "")
	_, out, _ = runTop(t, "-once", "-metrics", filepath.Join(filepath.Dir(queue), "metrics"))
	if !strings.Contains(out, "queue not read") && !strings.Contains(out, "no queue file given") {
		t.Errorf("\n%s", out)
	}
}

func TestTopHelp(t *testing.T) {
	_, _, e := runTop(t, "-h")
	if !strings.Contains(e, "one screen") && !strings.Contains(e, "whole system on one screen") {
		t.Errorf("%s", e)
	}
	var out bytes.Buffer
	usage(&out)
	if !strings.Contains(out.String(), "offgrid top") {
		t.Error("usage must list top")
	}
}
