package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"offgrid/internal/connectivity"
	"offgrid/internal/drain"
	"offgrid/internal/jobqueue"
	"offgrid/internal/router"
)

func TestSnapshotRoundTripThroughAFile(t *testing.T) {
	m, c := newM()
	m.Probe(connectivity.Result{OK: true, Latency: 5 * time.Millisecond}, connectivity.Good, connectivity.Online)
	m.Drain(ev(drain.EvClaimed, 1, "interpret", t0))
	c.Add(time.Second)
	m.Drain(ev(drain.EvDone, 1, "interpret", c.Now()))
	m.Route(router.Result{Text: "hi", Route: router.RouteDeferred})
	m.Queue(jobqueue.Stats{Queued: 2})
	want := m.Snapshot("drain", 42, t0, time.Second)

	dir := t.TempDir()
	if err := WriteSnapshot(SnapshotPath(dir, "drain"), want); err != nil {
		t.Fatal(err)
	}
	got, problems := ReadSnapshots(dir)
	if len(problems) != 0 || len(got) != 1 {
		t.Fatalf("%v %v", got, problems)
	}
	a, _ := json.Marshal(want)
	b, _ := json.Marshal(got[0])
	if !bytes.Equal(a, b) {
		t.Errorf("round trip changed the snapshot:\n%s\n%s", a, b)
	}
	if got[0].Version != SnapshotVersion || got[0].PID != 42 || got[0].Proc != "drain" {
		t.Errorf("%+v", got[0])
	}
}

func TestWriteSnapshotLeavesNoTempFilesAndReplacesAtomically(t *testing.T) {
	dir := t.TempDir()
	path := SnapshotPath(dir, "drain")
	for i := 0; i < 5; i++ {
		if err := WriteSnapshot(path, Snapshot{Version: SnapshotVersion, Proc: "drain", PID: i}); err != nil {
			t.Fatal(err)
		}
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 || ents[0].Name() != "drain.json" {
		t.Errorf("directory holds %v", ents)
	}
	s, _ := ReadSnapshots(dir)
	if len(s) != 1 || s[0].PID != 4 {
		t.Errorf("the newest write must win: %+v", s)
	}
}

func TestWriteSnapshotCreatesTheDirectoryAndReportsFailure(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	if err := WriteSnapshot(SnapshotPath(dir, "x"), Snapshot{Version: SnapshotVersion}); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "afile")
	os.WriteFile(file, []byte("x"), 0o644)
	if err := WriteSnapshot(filepath.Join(file, "x.json"), Snapshot{}); err == nil {
		t.Error("writing below a regular file must fail loudly")
	}
}

func TestReadSnapshotsSkipsBadFilesButSaysSo(t *testing.T) {
	dir := t.TempDir()
	WriteSnapshot(SnapshotPath(dir, "good"), Snapshot{Version: SnapshotVersion, Proc: "good"})
	os.WriteFile(filepath.Join(dir, "garbage.json"), []byte("{not json"), 0o644)
	os.WriteFile(filepath.Join(dir, "old.json"), []byte(`{"version":99,"proc":"old"}`), 0o644)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignored"), 0o644)
	snaps, problems := ReadSnapshots(dir)
	if len(snaps) != 1 || snaps[0].Proc != "good" {
		t.Errorf("snaps %+v", snaps)
	}
	if len(problems) != 2 || !strings.Contains(strings.Join(problems, "|"), "garbage.json") || !strings.Contains(strings.Join(problems, "|"), "version 99") {
		t.Errorf("problems %v", problems)
	}
	if s, p := ReadSnapshots(filepath.Join(dir, "missing")); len(s) != 0 || len(p) != 0 {
		t.Errorf("a missing directory is just no snapshots: %v %v", s, p)
	}
}

func TestFreshness(t *testing.T) {
	now := t0.Add(10 * time.Second)
	cases := []struct {
		name string
		s    Snapshot
		want bool
	}{
		{"just written", Snapshot{At: now, Interval: time.Second}, true},
		{"3 intervals old", Snapshot{At: now.Add(-3 * time.Second), Interval: time.Second}, true},
		{"4 intervals old", Snapshot{At: now.Add(-4 * time.Second), Interval: time.Second}, false},
		{"stopped is never fresh", Snapshot{At: now, Interval: time.Second, Stopped: true}, false},
		{"no interval means 1s", Snapshot{At: now.Add(-2 * time.Second)}, true},
		{"no interval, old", Snapshot{At: now.Add(-9 * time.Second)}, false},
		{"slow publisher is judged by its own interval", Snapshot{At: now.Add(-20 * time.Second), Interval: 10 * time.Second}, true},
		{"clock skew (written in the future) is not negative age", Snapshot{At: now.Add(time.Hour), Interval: time.Second}, true},
	}
	for _, c := range cases {
		if got := c.s.Fresh(now); got != c.want {
			t.Errorf("%s: Fresh = %v, want %v", c.name, got, c.want)
		}
	}
	if a := (Snapshot{At: now.Add(time.Hour)}).Age(now); a != 0 {
		t.Errorf("age %v", a)
	}
}

func TestPublishWritesOftenAndMarksItselfStoppedAtTheEnd(t *testing.T) {
	m := NewMetrics()
	dir := t.TempDir()
	var polls int32
	var mu sync.Mutex
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		Publish(ctx, m, dir, "drain", 10*time.Millisecond, func(context.Context) (jobqueue.Stats, error) {
			mu.Lock()
			polls++
			n := polls
			mu.Unlock()
			return jobqueue.Stats{Queued: int(n)}, nil
		}, nil)
	}()
	deadline := time.Now().Add(3 * time.Second)
	var first Snapshot
	for time.Now().Before(deadline) {
		if s, _ := ReadSnapshots(dir); len(s) == 1 && s[0].Queue != nil && s[0].Queue.Queued >= 3 {
			first = s[0]
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if first.Proc == "" {
		t.Fatal("no snapshot with a polled queue depth appeared")
	}
	if first.Stopped || first.Interval != 10*time.Millisecond || first.PID != os.Getpid() {
		t.Errorf("%+v", first)
	}
	cancel()
	<-done
	s, _ := ReadSnapshots(dir)
	if len(s) != 1 || !s[0].Stopped {
		t.Errorf("the last snapshot must say the process stopped: %+v", s)
	}
}

func TestPublishReportsWriteFailuresAndKeepsGoing(t *testing.T) {
	file := filepath.Join(t.TempDir(), "afile")
	os.WriteFile(file, []byte("x"), 0o644)
	var mu sync.Mutex
	var errs int
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	Publish(ctx, NewMetrics(), filepath.Join(file, "sub"), "drain", 10*time.Millisecond, nil, func(error) { mu.Lock(); errs++; mu.Unlock() })
	mu.Lock()
	defer mu.Unlock()
	if errs < 3 {
		t.Errorf("only %d errors reported: a failing write must not stop the loop", errs)
	}
}

func TestPublishReportsQueueReadFailure(t *testing.T) {
	var got error
	var mu sync.Mutex
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	Publish(ctx, NewMetrics(), t.TempDir(), "drain", 10*time.Millisecond,
		func(context.Context) (jobqueue.Stats, error) { return jobqueue.Stats{}, errors.New("db locked") },
		func(err error) { mu.Lock(); got = err; mu.Unlock() })
	mu.Lock()
	defer mu.Unlock()
	if got == nil || !strings.Contains(got.Error(), "db locked") {
		t.Errorf("%v", got)
	}
}
