package perf

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"path/filepath"
	"strings"
	"testing"
)

func small() Config {
	return Config{Repeats: 1, Sizes: []int{40, 120}, Jobs: []int{15}, Workers: []int{1, 3}}
}

func TestARunMeasuresEverythingAndReportsIt(t *testing.T) {
	rep, err := Run(context.Background(), small())
	if err != nil {
		t.Fatal(err)
	}
	// store: append + (open, rebuild) per size
	if len(rep.Store) != 1+2*2 {
		t.Fatalf("%d store rows", len(rep.Store))
	}
	// sync: 3 rows per size
	if len(rep.Sync) != 3*2 {
		t.Fatalf("%d sync rows", len(rep.Sync))
	}
	if len(rep.Queue) != 2 {
		t.Fatalf("%d queue rows", len(rep.Queue))
	}
	for _, s := range rep.Store {
		if strings.HasPrefix(s.Name, "open") {
			if s.PerSec != 0 {
				t.Errorf("opening does not replay the log, so it has no ops/s: %+v", s)
			}
			s.PerSec = 1
		}
		if s.Seconds.Median <= 0 || s.PerSec <= 0 || s.DBMiB <= 0 || s.Mem.HeapMiB <= 0 {
			t.Errorf("empty store row: %+v", s)
		}
	}
	for _, s := range rep.Sync {
		if s.Seconds.Median <= 0 || s.PerSec <= 0 || s.Mem.HeapMiB <= 0 {
			t.Errorf("empty sync row: %+v", s)
		}
		if strings.Contains(s.Mode, "HTTP") && s.Requests < 2 {
			t.Errorf("HTTP row made %d requests: %+v", s.Requests, s)
		}
	}
	for i, q := range rep.Queue {
		if q.Drain.Median <= 0 || q.DrainPerSec <= 0 || q.Jobs != 15 || q.Concurrency != []int{1, 3}[i] {
			t.Errorf("queue row: %+v", q)
		}
	}
	if rep.Meta.Go == "" || rep.Meta.Dir == "" || rep.Meta.Seed != 1 || rep.Meta.Repeats != 1 || rep.Meta.Hardware.Threads < 1 {
		t.Errorf("meta: %+v", rep.Meta)
	}

	var md bytes.Buffer
	if err := WriteMarkdown(&md, rep); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"**Hardware:**", "**Store**", "**Sync**", "**Queue**", "rebuild the view from the log", "new replica, HTTP on localhost", "10 new ops on a 120-op log, HTTP", "Jobs/s drained", "No model is involved"} {
		if !strings.Contains(md.String(), want) {
			t.Errorf("markdown lacks %q:\n%s", want, md.String())
		}
	}
	var js bytes.Buffer
	if err := WriteJSON(&js, rep); err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(js.Bytes(), &back); err != nil || len(back.Sync) != len(rep.Sync) || back.Meta.Seed != 1 {
		t.Fatalf("json round trip: %v", err)
	}
}

func TestARunInAGivenDirectoryLeavesNoDatabasesBehind(t *testing.T) {
	dir := t.TempDir()
	cfg := small()
	cfg.Dir = dir
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	left, _ := filepath.Glob(filepath.Join(dir, "*.db*"))
	// the source replicas of the sync phase are closed but their files stay; nothing else may
	for _, f := range left {
		if !strings.Contains(filepath.Base(f), "src") && !strings.Contains(filepath.Base(f), "dst") && !strings.Contains(filepath.Base(f), "inc") {
			t.Errorf("left behind: %s", f)
		}
	}
}

func TestACancelledRunStopsWithTheContextError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, small()); err == nil {
		t.Fatal("a cancelled run reported success")
	}
}

// The workload is the same for the same seed, and writes exactly the ops asked for.
func TestTheWorkloadIsDeterministicAndExactlyTheSizeAsked(t *testing.T) {
	ctx := context.Background()
	hash := func(seed int64, n int) (string, int64) {
		clk := &fakeClock{epoch}
		s, err := openStore(ctx, filepath.Join(t.TempDir(), "x.db"), "n", clk)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if err := fill(ctx, s, clk, n, seed); err != nil {
			t.Fatal(err)
		}
		h, _ := s.StateHash(ctx)
		sum, _ := s.Summary(ctx)
		return h, sum["n"]
	}
	h1, n1 := hash(7, 150)
	h2, n2 := hash(7, 150)
	h3, _ := hash(8, 150)
	if h1 != h2 || n1 != 150 || n2 != 150 {
		t.Fatalf("same seed: %s %s, ops %d %d", h1[:8], h2[:8], n1, n2)
	}
	if h1 == h3 {
		t.Fatal("different seeds made the same state")
	}
}

// A timing is only worth reporting if the replica really caught up.
func TestSameStateRefusesReplicasThatDiffer(t *testing.T) {
	ctx := context.Background()
	mk := func(n int) *fakeClock { return &fakeClock{epoch} }
	a, err := openStore(ctx, filepath.Join(t.TempDir(), "a.db"), "a", mk(1))
	if err != nil {
		t.Fatal(err)
	}
	b, err := openStore(ctx, filepath.Join(t.TempDir(), "b.db"), "b", mk(1))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	defer b.Close()
	ca, cb := &fakeClock{epoch}, &fakeClock{epoch}
	a2, _ := openStore(ctx, filepath.Join(t.TempDir(), "a2.db"), "a2", ca)
	b2, _ := openStore(ctx, filepath.Join(t.TempDir(), "b2.db"), "b2", cb)
	defer a2.Close()
	defer b2.Close()
	if err := fill(ctx, a2, ca, 5, 1); err != nil {
		t.Fatal(err)
	}
	if err := sameState(ctx, a2, b2); err == nil || !strings.Contains(err.Error(), "meaningless") {
		t.Fatalf("err = %v", err)
	}
	if err := pullAll(ctx, b2, a2, 2); err != nil {
		t.Fatal(err)
	}
	if err := sameState(ctx, a2, b2); err != nil {
		t.Fatal(err)
	}
}

func TestSpreadIsMedianMinMax(t *testing.T) {
	for _, c := range []struct {
		in   []float64
		want Spread
	}{
		{nil, Spread{}},
		{[]float64{3}, Spread{3, 3, 3}},
		{[]float64{5, 1, 3}, Spread{3, 1, 5}},
		{[]float64{4, 1, 2, 10}, Spread{3, 1, 10}},
	} {
		if got := spread(c.in); got != c.want {
			t.Errorf("spread(%v) = %+v, want %+v", c.in, got, c.want)
		}
	}
	in := []float64{3, 1, 2}
	spread(in)
	if in[0] != 3 {
		t.Error("spread sorted its input")
	}
}

func TestProcStatusAndMountsParsing(t *testing.T) {
	if got := ParseVmHWMMiB("Name:\tx\nVmPeak:\t 9999 kB\nVmHWM:\t   20480 kB\nVmRSS:\t 100 kB\n"); got != 20 {
		t.Errorf("VmHWM = %v", got)
	}
	if ParseVmHWMMiB("nothing here") != 0 || ParseVmHWMMiB("VmHWM: x kB") != 0 {
		t.Error("garbage parsed as a number")
	}
	mounts := "overlay / overlay rw 0 0\ntmpfs /dev/shm tmpfs rw 0 0\n/dev/sda1 /home ext4 rw 0 0\n/dev/sda2 /home/me/data btrfs rw 0 0\nbroken\n"
	for path, want := range map[string]string{
		"/tmp/x":            "overlay",
		"/dev/shm/offgrid":  "tmpfs",
		"/dev/shmx/y":       "overlay", // a different directory that merely starts the same
		"/home/me":          "ext4",
		"/home/me/data/db":  "btrfs",
		"/home/me/database": "ext4",
	} {
		if got := FilesystemFromMounts(mounts, path); got != want {
			t.Errorf("%s: %q, want %q", path, got, want)
		}
	}
	if FilesystemFromMounts("", "/x") != "" {
		t.Error("no mounts, some answer")
	}
}

func TestNumberFormatting(t *testing.T) {
	for in, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 12345: "12,345", 1234567: "1,234,567"} {
		if got := commas(in); got != want {
			t.Errorf("commas(%d) = %q", in, got)
		}
	}
	for v, want := range map[float64]string{123.4: "123", 12.34: "12.3", 1.234: "1.23", 0.1234: "0.123", 0.001234: "0.0012"} {
		if got := sig(v); got != want {
			t.Errorf("sig(%v) = %q, want %q", v, got, want)
		}
	}
	for v, want := range map[float64]string{0: "-", -1: "-", 12345.6: "12,346", 99.5: "100", 12.4: "12", 3.14: "3.1"} {
		if got := rate(v); got != want {
			t.Errorf("rate(%v) = %q, want %q", v, got, want)
		}
	}
}

func TestReplaceBlockReplacesBetweenMarkersAndRefusesWhenTheyAreWrong(t *testing.T) {
	const s, e = "<!-- a:start -->", "<!-- a:end -->"
	text := "before\n" + s + "\nold\nold2\n" + e + "\nafter\n"
	got, err := ReplaceBlock(text, s, e, "new")
	if err != nil || got != "before\n"+s+"\nnew\n"+e+"\nafter\n" {
		t.Fatalf("%q %v", got, err)
	}
	again, _ := ReplaceBlock(got, s, e, "new\n")
	if again != got {
		t.Fatal("replacing twice changed the text")
	}
	for name, bad := range map[string]string{
		"no markers": "just text",
		"no end":     s + "\nx",
		"two starts": s + s + e,
		"reversed":   e + "\n" + s,
	} {
		if out, err := ReplaceBlock(bad, s, e, "x"); err == nil {
			t.Errorf("%s: accepted, gave %q", name, out)
		}
	}
}

func TestMarkdownSaysWhetherRSSIsPerRow(t *testing.T) {
	for _, per := range []bool{true, false} {
		var b bytes.Buffer
		WriteMarkdown(&b, Report{Meta: Meta{RSSPerRun: per, Repeats: 3, Seed: 1}})
		if got := strings.Contains(b.String(), "counter reset before each row"); got != per {
			t.Errorf("RSSPerRun=%v: %s", per, b.String())
		}
		if !strings.Contains(b.String(), "unknown") {
			t.Error("missing hardware should say unknown, not be blank")
		}
	}
}

func TestTheChartIsWellFormedNamesTheMachineAndDrawsEveryBar(t *testing.T) {
	rep, err := Run(context.Background(), small())
	if err != nil {
		t.Fatal(err)
	}
	rep.Meta.Hardware.CPU = `Evil <CPU> & "Co"`
	var b bytes.Buffer
	if err := WriteSVG(&b, rep); err != nil {
		t.Fatal(err)
	}
	dec := xml.NewDecoder(&b)
	rects, texts := 0, 0
	var all strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("not well-formed XML: %v", err)
		}
		switch e := tok.(type) {
		case xml.StartElement:
			switch e.Name.Local {
			case "rect":
				rects++
			case "text":
				texts++
			}
		case xml.CharData:
			all.Write(e)
		}
	}
	// 1 background + 4 sync bars (2 sizes x in-process, HTTP) + 2 queue bars + 3 memory bars + 2 legend swatches
	if rects != 1+4+2+3+2 {
		t.Errorf("%d rects", rects)
	}
	for _, want := range []string{`Evil <CPU> & "Co"`, "A new replica catches up", "Queue drain", "Peak memory", "120 ops, over HTTP", "jobs/s", "Reproduce it yourself: make bench", "Network is localhost, not WiFi."} {
		if !strings.Contains(all.String(), want) {
			t.Errorf("no %q in the picture's text", want)
		}
	}
}

func TestTheChartOfAnEmptyReportSaysSoInsteadOfBreaking(t *testing.T) {
	var b bytes.Buffer
	if err := WriteSVG(&b, Report{}); err != nil || strings.Count(b.String(), "no data") != 3 {
		t.Fatalf("%v\n%s", err, b.String())
	}
}

func TestDurationsReadLikeAPersonWouldSayThem(t *testing.T) {
	for in, want := range map[float64]string{25: "25.0 s", 2.141: "2.14 s", 0.186: "186 ms", 0.0044: "4 ms", 0.0000031: "3 µs"} {
		if got := dur(in); got != want {
			t.Errorf("dur(%v) = %q, want %q", in, got, want)
		}
	}
}
