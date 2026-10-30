package perf

import (
	"bytes"
	"context"
	"encoding/xml"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"offgrid/internal/syncer"
	"offgrid/internal/sysinfo"
	"offgrid/internal/task"
)

// TestPerfEdgeConfigValidationAndDefaults tests boundary cases for Config.withDefaults().
func TestPerfEdgeConfigValidationAndDefaults(t *testing.T) {
	// Zero config should get all default values
	c := Config{}.withDefaults()
	if c.Repeats != 3 {
		t.Errorf("default repeats = %d, want 3", c.Repeats)
	}
	if len(c.Sizes) != 3 || c.Sizes[0] != 100 || c.Sizes[1] != 1000 || c.Sizes[2] != 10000 {
		t.Errorf("default sizes = %v", c.Sizes)
	}
	if len(c.Jobs) != 2 || c.Jobs[0] != 100 || c.Jobs[1] != 1000 {
		t.Errorf("default jobs = %v", c.Jobs)
	}
	if len(c.Workers) != 2 || c.Workers[0] != 1 || c.Workers[1] != 4 {
		t.Errorf("default workers = %v", c.Workers)
	}
	if c.Seed != 1 {
		t.Errorf("default seed = %d, want 1", c.Seed)
	}
	if c.Progress == nil {
		t.Error("default progress function is nil")
	}
	// Calling progress should not panic
	c.Progress("testing %s", "progress")

	// Negative values should be replaced by defaults
	neg := Config{
		Repeats: -5,
		Seed:    0,
	}.withDefaults()
	if neg.Repeats != 3 {
		t.Errorf("negative repeats corrected to %d, want 3", neg.Repeats)
	}
	if neg.Seed != 1 {
		t.Errorf("zero seed corrected to %d, want 1", neg.Seed)
	}

	// Preserved custom values
	customProgressCalled := false
	custom := Config{
		Repeats: 5,
		Sizes:   []int{50, 500},
		Jobs:    []int{25},
		Workers: []int{2, 8},
		Seed:    42,
		Progress: func(format string, args ...any) {
			customProgressCalled = true
		},
	}.withDefaults()

	if custom.Repeats != 5 || len(custom.Sizes) != 2 || custom.Seed != 42 {
		t.Errorf("custom config corrupted by defaults: %+v", custom)
	}
	custom.Progress("test")
	if !customProgressCalled {
		t.Error("custom progress was overwritten")
	}
}

// TestPerfEdgeSameStateDivergenceDetection proves that sameState catches any
// state difference between source and destination and refuses with an error.
func TestPerfEdgeSameStateDivergenceDetection(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	clkA := &fakeClock{epoch}
	clkB := &fakeClock{epoch}

	sA, err := openStore(ctx, filepath.Join(dir, "src.db"), "src", clkA)
	if err != nil {
		t.Fatal(err)
	}
	defer sA.Close()

	sB, err := openStore(ctx, filepath.Join(dir, "dst.db"), "dst", clkB)
	if err != nil {
		t.Fatal(err)
	}
	defer sB.Close()

	// Fill sA with 10 tasks
	if err := fill(ctx, sA, clkA, 10, 101); err != nil {
		t.Fatal(err)
	}

	// sB is empty -> must fail with divergence
	err = sameState(ctx, sA, sB)
	if err == nil || !strings.Contains(err.Error(), "the timing is meaningless") {
		t.Fatalf("expected 'meaningless' error on different stores, got: %v", err)
	}

	// Now pull all ops from sA to sB -> should match
	if err := pullAll(ctx, sB, sA, syncer.DefaultBatch); err != nil {
		t.Fatal(err)
	}
	if err := sameState(ctx, sA, sB); err != nil {
		t.Fatalf("sameState failed after full pull: %v", err)
	}

	// Now artificially diverge sB by adding an unshared note
	clkB.t = clkB.t.Add(time.Minute)
	allTasks, _ := sB.List(ctx, task.Filter{})
	if len(allTasks) > 0 {
		sB.AddNote(ctx, allTasks[0].ID, "rogue divergence note")
	}

	err = sameState(ctx, sA, sB)
	if err == nil || !strings.Contains(err.Error(), "the timing is meaningless") {
		t.Fatalf("expected divergence detection after note added to dst, got: %v", err)
	}
}

// TestPerfEdgeFilesystemFromMountsCornerCases tests parsing of /proc/mounts.
func TestPerfEdgeFilesystemFromMountsCornerCases(t *testing.T) {
	cases := []struct {
		name   string
		mounts string
		path   string
		wantFS string
	}{
		{
			name:   "empty mounts content",
			mounts: "",
			path:   "/var/data",
			wantFS: "",
		},
		{
			name:   "root mount fallback",
			mounts: "/dev/root / ext4 rw,relatime 0 0\n",
			path:   "/opt/myproject/db",
			wantFS: "ext4",
		},
		{
			name: "deepest prefix matching",
			mounts: strings.Join([]string{
				"/dev/sda1 / ext4 rw 0 0",
				"/dev/sdb1 /mnt ext4 rw 0 0",
				"/dev/sdc1 /mnt/fast nvme rw 0 0",
				"tmpfs /mnt/fast/ram tmpfs rw 0 0",
			}, "\n"),
			path:   "/mnt/fast/ram/offgrid.db",
			wantFS: "tmpfs",
		},
		{
			name: "exact directory match with no trailing slash",
			mounts: strings.Join([]string{
				"/dev/sda1 /home ext4 rw 0 0",
				"/dev/sdb1 /home/user/data btrfs rw 0 0",
			}, "\n"),
			path:   "/home/user/data",
			wantFS: "btrfs",
		},
		{
			name: "distinguish paths with shared prefix but different directories",
			mounts: strings.Join([]string{
				"/dev/sda1 / ext4 rw 0 0",
				"/dev/sdb1 /data ext4 rw 0 0",
				"/dev/sdc1 /data-backup zfs rw 0 0",
			}, "\n"),
			path:   "/data-backup/offgrid",
			wantFS: "zfs",
		},
		{
			name: "malformed lines ignored",
			mounts: strings.Join([]string{
				"# comment line",
				"short",
				"only two",
				"/dev/sda1 / validfs rw 0 0",
			}, "\n"),
			path:   "/some/path",
			wantFS: "validfs",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FilesystemFromMounts(tc.mounts, tc.path)
			if got != tc.wantFS {
				t.Errorf("FilesystemFromMounts() = %q, want %q", got, tc.wantFS)
			}
		})
	}
}

// TestPerfEdgeVmHWMParsingCornerCases tests parsing of resident memory high water mark.
func TestPerfEdgeVmHWMParsingCornerCases(t *testing.T) {
	cases := []struct {
		name   string
		status string
		want   float64
	}{
		{
			name:   "empty status",
			status: "",
			want:   0,
		},
		{
			name:   "normal VmHWM in kB",
			status: "Name: offgrid\nVmPeak: 45000 kB\nVmHWM: 24576 kB\nVmRSS: 20000 kB\n",
			want:   24.0, // 24576 / 1024
		},
		{
			name:   "tabs and multiple spaces",
			status: "VmHWM:\t\t  10240   kB\n",
			want:   10.0,
		},
		{
			name:   "fractional or zero value",
			status: "VmHWM: 512 kB\n",
			want:   0.5,
		},
		{
			name:   "non-numeric or missing value",
			status: "VmHWM: invalid kB\n",
			want:   0,
		},
		{
			name:   "only prefix without value",
			status: "VmHWM:\n",
			want:   0,
		},
		{
			name:   "similar key name does not match",
			status: "NotVmHWM: 99999 kB\nVmHWM_Extra: 88888 kB\n",
			want:   0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseVmHWMMiB(tc.status)
			if got != tc.want {
				t.Errorf("ParseVmHWMMiB() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPerfEdgeReplaceBlockStrictValidation tests ReplaceBlock boundary and error conditions.
func TestPerfEdgeReplaceBlockStrictValidation(t *testing.T) {
	start := "<!-- marker:start -->"
	end := "<!-- marker:end -->"

	// 1. Missing start marker
	_, err := ReplaceBlock("text with <!-- marker:end -->", start, end, "body")
	if err == nil || !strings.Contains(err.Error(), "must contain exactly one") {
		t.Errorf("expected error on missing start marker, got: %v", err)
	}

	// 2. Missing end marker
	_, err = ReplaceBlock("text with <!-- marker:start -->", start, end, "body")
	if err == nil || !strings.Contains(err.Error(), "must contain exactly one") {
		t.Errorf("expected error on missing end marker, got: %v", err)
	}

	// 3. Duplicate start markers
	_, err = ReplaceBlock("a "+start+" b "+start+" c "+end, start, end, "body")
	if err == nil || !strings.Contains(err.Error(), "must contain exactly one") {
		t.Errorf("expected error on duplicate start marker, got: %v", err)
	}

	// 4. Duplicate end markers
	_, err = ReplaceBlock("a "+start+" b "+end+" c "+end, start, end, "body")
	if err == nil || !strings.Contains(err.Error(), "must contain exactly one") {
		t.Errorf("expected error on duplicate end marker, got: %v", err)
	}

	// 5. Inverted markers (end before start)
	_, err = ReplaceBlock("a "+end+" b "+start+" c", start, end, "body")
	if err == nil || !strings.Contains(err.Error(), "comes before") {
		t.Errorf("expected error on inverted markers, got: %v", err)
	}

	// 6. Valid replacement with trailing newline guaranteed
	orig := "PRE\n" + start + "\nOLD\n" + end + "\nPOST"
	res, err := ReplaceBlock(orig, start, end, "NEW_BODY")
	if err != nil {
		t.Fatalf("ReplaceBlock failed: %v", err)
	}
	expected := "PRE\n" + start + "\nNEW_BODY\n" + end + "\nPOST"
	if res != expected {
		t.Errorf("result mismatch:\ngot:  %q\nwant: %q", res, expected)
	}

	// 7. Unicode content inside body and surrounding text
	unicodeBody := "🚀 Performance: 15,000 ops/s • メモリ: 22 MiB • አማርኛ"
	res, err = ReplaceBlock(orig, start, end, unicodeBody)
	if err != nil {
		t.Fatalf("ReplaceBlock with unicode: %v", err)
	}
	if !strings.Contains(res, unicodeBody) {
		t.Errorf("unicode body missing from result:\n%s", res)
	}
}

// TestPerfEdgeSpreadCalculationAndStability verifies that spread() handles
// odd, even, and boundary slices without modifying the input slice.
func TestPerfEdgeSpreadCalculationAndStability(t *testing.T) {
	// Empty slice
	if sp := spread(nil); sp != (Spread{}) {
		t.Errorf("spread(nil) = %+v, want empty Spread", sp)
	}
	if sp := spread([]float64{}); sp != (Spread{}) {
		t.Errorf("spread([]) = %+v, want empty Spread", sp)
	}

	// Single element
	sp := spread([]float64{42.5})
	if sp.Median != 42.5 || sp.Min != 42.5 || sp.Max != 42.5 {
		t.Errorf("spread([42.5]) = %+v, want all 42.5", sp)
	}

	// Even count: median is average of middle two
	spEven := spread([]float64{10.0, 20.0, 30.0, 40.0})
	if spEven.Median != 25.0 || spEven.Min != 10.0 || spEven.Max != 40.0 {
		t.Errorf("spread(even) = %+v, want med=25, min=10, max=40", spEven)
	}

	// Odd count: median is middle element
	spOdd := spread([]float64{50.0, 10.0, 30.0})
	if spOdd.Median != 30.0 || spOdd.Min != 10.0 || spOdd.Max != 50.0 {
		t.Errorf("spread(odd) = %+v, want med=30, min=10, max=50", spOdd)
	}

	// Verify original slice was not mutated
	orig := []float64{99.0, 1.0, 50.0}
	_ = spread(orig)
	if orig[0] != 99.0 || orig[1] != 1.0 || orig[2] != 50.0 {
		t.Errorf("spread mutated original slice: %v", orig)
	}
}

// TestPerfEdgeWriteSVGXMLEntityEscaping verifies that WriteSVG cleanly escapes
// XML special characters like &, <, >, ", ' in hardware, OS, and labels.
func TestPerfEdgeWriteSVGXMLEntityEscaping(t *testing.T) {
	rep := Report{
		Meta: Meta{
			Hardware: sysinfo.Info{
				CPU:     `Intel Core i7 <vPro> & "AMD" 'Threadripper'`,
				OS:      `Linux <6.8.0> & "Ubuntu"`,
				Threads: 16,
				MemGiB:  32.0,
			},
			Dir: "/tmp/test & <special>",
		},
		Sync: []SyncRow{
			{Mode: "new replica, in process", Ops: 100, Seconds: Spread{Median: 0.05, Min: 0.04, Max: 0.06}, PerSec: 2000},
			{Mode: "new replica, HTTP on localhost", Ops: 100, Seconds: Spread{Median: 0.08, Min: 0.07, Max: 0.09}, PerSec: 1250},
		},
		Queue: []QueueRow{
			{Jobs: 100, Concurrency: 4, DrainPerSec: 500, Drain: Spread{Median: 0.2, Min: 0.18, Max: 0.22}},
		},
	}

	var b bytes.Buffer
	if err := WriteSVG(&b, rep); err != nil {
		t.Fatalf("WriteSVG: %v", err)
	}

	// Must be well-formed XML
	dec := xml.NewDecoder(bytes.NewReader(b.Bytes()))
	for {
		_, err := dec.Token()
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("WriteSVG produced invalid XML with special characters: %v\nOutput:\n%s", err, b.String())
		}
	}

	svgText := b.String()
	// Ensure raw unescaped < and & do not appear inside attributes or text nodes
	if strings.Contains(svgText, "<vPro>") || strings.Contains(svgText, `& "AMD"`) {
		t.Errorf("unescaped characters found in SVG output:\n%s", svgText)
	}
}

// TestPerfEdgeMemWatchGoroutineLifecycle verifies startMem and stop lifecycle.
func TestPerfEdgeMemWatchGoroutineLifecycle(t *testing.T) {
	w := startMem()
	if w == nil {
		t.Fatal("startMem returned nil")
	}

	// Allocate some temporary memory
	dummy := make([][]byte, 100)
	for i := range dummy {
		dummy[i] = make([]byte, 1024*100) // 100 KB each = 10 MB total
	}

	time.Sleep(25 * time.Millisecond) // Give sampler ticker time to tick
	m := w.stop()

	if m.HeapMiB <= 0 {
		t.Errorf("HeapMiB after allocations = %v, want > 0", m.HeapMiB)
	}

	// Clearing dummy reference
	dummy = nil
	_ = dummy
}

// TestPerfEdgeNumberFormattingSignificanceAndRates tests formatting boundary cases.
func TestPerfEdgeNumberFormattingSignificanceAndRates(t *testing.T) {
	// commas()
	if got := commas(0); got != "0" {
		t.Errorf("commas(0) = %q, want 0", got)
	}
	if got := commas(1); got != "1" {
		t.Errorf("commas(1) = %q, want 1", got)
	}
	if got := commas(999); got != "999" {
		t.Errorf("commas(999) = %q, want 999", got)
	}
	if got := commas(1000); got != "1,000" {
		t.Errorf("commas(1000) = %q, want 1,000", got)
	}
	if got := commas(1000000000); got != "1,000,000,000" {
		t.Errorf("commas(1B) = %q, want 1,000,000,000", got)
	}

	// sig()
	if got := sig(0.00001); got != "0.0000" {
		t.Errorf("sig(0.00001) = %q, want 0.0000", got)
	}
	if got := sig(0.05432); got != "0.054" {
		t.Errorf("sig(0.05432) = %q, want 0.054", got)
	}
	if got := sig(1.2345); got != "1.23" {
		t.Errorf("sig(1.2345) = %q, want 1.23", got)
	}
	if got := sig(15.678); got != "15.7" {
		t.Errorf("sig(15.678) = %q, want 15.7", got)
	}
	if got := sig(150.25); got != "150" {
		t.Errorf("sig(150.25) = %q, want 150", got)
	}

	// dur()
	if got := dur(0.000001); got != "1 µs" {
		t.Errorf("dur(1µs) = %q, want 1 µs", got)
	}
	if got := dur(0.0000004); got != "0 µs" {
		t.Errorf("dur(0.4µs) = %q, want 0 µs", got)
	}
	if got := dur(0.005); got != "5 ms" {
		t.Errorf("dur(5ms) = %q, want 5 ms", got)
	}
	if got := dur(1.5); got != "1.50 s" {
		t.Errorf("dur(1.5s) = %q, want 1.50 s", got)
	}
	if got := dur(45.0); got != "45.0 s" {
		t.Errorf("dur(45s) = %q, want 45.0 s", got)
	}

	// orNA()
	if got := orNA(""); got != "unknown" {
		t.Errorf("orNA('') = %q, want 'unknown'", got)
	}
	if got := orNA("AMD"); got != "AMD" {
		t.Errorf("orNA('AMD') = %q, want 'AMD'", got)
	}
}

