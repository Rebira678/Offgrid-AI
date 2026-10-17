package netfault

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseSchedule(t *testing.T) {
	good := []string{"ok:5s", "partition:1ms", "ok:5s,partition:3s", " ok : 5s , partition : 3s ", "ok:5s,"}
	for _, g := range good {
		if _, err := ParseSchedule(g); err != nil {
			t.Errorf("ParseSchedule(%q): %v", g, err)
		}
	}
	bad := []string{"", ",", "ok", "ok:", "ok:abc", "ok:0s", "ok:-1s", "up:5s", "partition:5", "5s:ok"}
	for _, b := range bad {
		if _, err := ParseSchedule(b); err == nil {
			t.Errorf("ParseSchedule(%q) accepted", b)
		}
	}
}

func TestSchedulePartitionedAt(t *testing.T) {
	s, err := ParseSchedule("ok:5s,partition:3s")
	if err != nil {
		t.Fatal(err)
	}
	sec := time.Second
	cases := []struct {
		at   time.Duration
		want bool
	}{
		{0, false}, {4999 * time.Millisecond, false},
		{5 * sec, true}, {7999 * time.Millisecond, true}, // boundary: the new step starts exactly at 5s
		{8 * sec, false}, // and the cycle repeats exactly at 8s
		{12 * sec, false}, {13 * sec, true}, {16 * sec, false},
		{8000 * sec, false}, {8005 * sec, true}, // many cycles later
		{-3 * sec, false}, // before the start counts as the start
	}
	for _, c := range cases {
		if got := s.PartitionedAt(c.at); got != c.want {
			t.Errorf("PartitionedAt(%s) = %v, want %v", c.at, got, c.want)
		}
	}
}

func TestScheduleThreeSteps(t *testing.T) {
	s, _ := ParseSchedule("partition:2s,ok:1s,partition:1s")
	// 0-2s down, 2-3s ok, 3-4s down; sampled every 500ms
	want := []bool{true, true, true, true, false, false, true, true}
	for i, w := range want {
		if got := s.PartitionedAt(time.Duration(i) * 500 * time.Millisecond); got != w {
			t.Errorf("at %dms: got %v want %v", i*500, got, w)
		}
	}
}

func TestScheduleSourceFollowsTheClock(t *testing.T) {
	s, _ := ParseSchedule("ok:2s,partition:2s")
	now := time.Unix(1000, 0)
	src := s.Source(func() time.Time { return now })
	check := func(want bool) {
		t.Helper()
		got, err := src()
		if err != nil || got != want {
			t.Errorf("at %v: got %v, %v want %v", now, got, err, want)
		}
	}
	check(false)
	now = now.Add(2500 * time.Millisecond)
	check(true)
	now = now.Add(2 * time.Second)
	check(false)
}

func TestFileSource(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fault")
	src := FileSource(p)

	if got, err := src(); err != nil || got {
		t.Errorf("missing file: %v, %v (want ok)", got, err)
	}
	os.WriteFile(p, []byte("partition\n"), 0o644)
	if got, err := src(); err != nil || !got {
		t.Errorf("partition: %v, %v", got, err)
	}
	os.WriteFile(p, []byte("  ok  "), 0o644)
	if got, err := src(); err != nil || got {
		t.Errorf("ok: %v, %v", got, err)
	}
	for _, junk := range []string{"", "partitioned", "OK", "true", "ok partition"} {
		os.WriteFile(p, []byte(junk), 0o644)
		if _, err := src(); err == nil {
			t.Errorf("content %q accepted: a typo must not silently leave the network up", junk)
		}
	}
}

func TestFileSourceUnreadableIsAnError(t *testing.T) {
	// A directory where the file should be: exists, cannot be read as a file.
	if _, err := FileSource(t.TempDir())(); err == nil {
		t.Error("unreadable control file treated as ok")
	}
}

func TestWriteStateRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fault")
	for _, want := range []bool{true, false, true} {
		if err := WriteState(p, want); err != nil {
			t.Fatal(err)
		}
		if got, err := FileSource(p)(); err != nil || got != want {
			t.Errorf("wrote %v, read %v, %v", want, got, err)
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(p))
	if len(entries) != 1 {
		t.Errorf("temporary files left behind: %v", entries)
	}
}

func TestWriteStateIntoMissingFolderFails(t *testing.T) {
	if err := WriteState(filepath.Join(t.TempDir(), "nope", "fault"), true); err == nil {
		t.Error("no error for a missing folder")
	}
}

// A reader polling while a writer flips the file must never see a torn
// (empty or partial) file. This is why WriteState renames instead of writing
// in place.
func TestWriteStateIsAtomicForConcurrentReaders(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fault")
	WriteState(p, false)
	src := FileSource(p)
	var bad atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := src(); err != nil {
				bad.Add(1)
			}
		}
	}()
	for i := 0; i < 1000; i++ {
		if err := WriteState(p, i%2 == 0); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	if bad.Load() != 0 {
		t.Errorf("a reader saw a torn control file %d times", bad.Load())
	}
}

func TestEither(t *testing.T) {
	yes := func() (bool, error) { return true, nil }
	no := func() (bool, error) { return false, nil }
	boom := func() (bool, error) { return false, os.ErrInvalid }
	cases := []struct {
		name string
		in   []Source
		want bool
		err  bool
	}{
		{"none", nil, false, false},
		{"nil entries ignored", []Source{nil, no}, false, false},
		{"one yes", []Source{no, yes}, true, false},
		{"all no", []Source{no, no}, false, false},
		{"error wins", []Source{yes, boom}, false, true},
	}
	for _, c := range cases {
		got, err := Either(c.in...)()
		if got != c.want || (err != nil) != c.err {
			t.Errorf("%s: got %v, %v", c.name, got, err)
		}
	}
}

func TestSwitchSource(t *testing.T) {
	var s Switch
	src := s.Source()
	if got, _ := src(); got {
		t.Error("new switch should be off")
	}
	s.Set(true)
	if got, _ := src(); !got {
		t.Error("switch did not turn on")
	}
}
