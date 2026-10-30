// Package perf measures what Offgrid costs on a machine, without any model:
// how fast the store writes and rebuilds, how long a replica takes to catch up
// by sync (in process and over a real HTTP connection on localhost), how fast
// the durable queue drains, and how much memory each of those needs.
//
// The workloads are deterministic (a seed fixes every task title and every
// edit), every figure is the median of several runs with min and max kept, and
// the report names the hardware, the Go version and the directory the files
// lived in, so anyone can rerun it and compare like with like.
//
// It does not measure language models: `offgrid bench` does that, and needs
// Ollama.
package perf

import (
	"context"
	"fmt"
	"math/rand"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"offgrid/internal/drain"
	"offgrid/internal/jobqueue"
	"offgrid/internal/store/sqlstore"
	"offgrid/internal/syncer"
	"offgrid/internal/sysinfo"
	"offgrid/internal/task"
)

// Config says what to measure. The zero value is the full default run.
type Config struct {
	Dir      string // where database files go; "" makes (and removes) a temp directory
	Repeats  int    // runs per figure; the median is reported (default 3)
	Sizes    []int  // log sizes for store and sync (default 100, 1000, 10000)
	Jobs     []int  // queue sizes (default 100, 1000)
	Workers  []int  // drain concurrency levels (default 1, 4)
	Seed     int64  // fixes every generated title and edit (default 1)
	Progress func(format string, args ...any)
}

func (c Config) withDefaults() Config {
	if c.Repeats < 1 {
		c.Repeats = 3
	}
	if len(c.Sizes) == 0 {
		c.Sizes = []int{100, 1000, 10000}
	}
	if len(c.Jobs) == 0 {
		c.Jobs = []int{100, 1000}
	}
	if len(c.Workers) == 0 {
		c.Workers = []int{1, 4}
	}
	if c.Seed == 0 {
		c.Seed = 1
	}
	if c.Progress == nil {
		c.Progress = func(string, ...any) {}
	}
	return c
}

// Meta is what is needed to interpret a result later.
type Meta struct {
	Date      string       `json:"date"`
	Hardware  sysinfo.Info `json:"hardware"`
	Go        string       `json:"go"`
	Dir       string       `json:"dir"`
	DirFS     string       `json:"dir_filesystem,omitempty"`
	Governor  string       `json:"cpu_governor,omitempty"`
	Repeats   int          `json:"repeats"`
	Seed      int64        `json:"seed"`
	RSSPerRun bool         `json:"rss_peak_per_phase"` // false: the RSS column is a running maximum
}

// Spread is a figure measured several times.
type Spread struct {
	Median float64 `json:"median"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
}

// Mem is the memory a phase needed.
type Mem struct {
	HeapMiB float64 `json:"peak_go_heap_mib"`
	RSSMiB  float64 `json:"peak_rss_mib"`
}

// StoreRow is one store measurement.
type StoreRow struct {
	Name    string  `json:"name"`
	Ops     int     `json:"ops"`
	Seconds Spread  `json:"seconds"`
	PerSec  float64 `json:"ops_per_sec"`
	DBMiB   float64 `json:"db_mib"`
	Mem     Mem     `json:"memory"`
}

// SyncRow is one sync measurement.
type SyncRow struct {
	Mode     string  `json:"mode"`
	Ops      int     `json:"ops"`
	Seconds  Spread  `json:"seconds"`
	PerSec   float64 `json:"ops_per_sec"`
	Requests int     `json:"requests,omitempty"`
	Mem      Mem     `json:"memory"`
}

// QueueRow is one queue measurement.
type QueueRow struct {
	Jobs        int     `json:"jobs"`
	Concurrency int     `json:"concurrency"`
	Enqueue     Spread  `json:"enqueue_seconds"`
	Drain       Spread  `json:"drain_seconds"`
	DrainPerSec float64 `json:"drain_jobs_per_sec"`
	Mem         Mem     `json:"memory"`
}

// Report is everything one run measured.
type Report struct {
	Meta  Meta       `json:"meta"`
	Store []StoreRow `json:"store"`
	Sync  []SyncRow  `json:"sync"`
	Queue []QueueRow `json:"queue"`
}

// Run measures everything in cfg. It stops early only if ctx ends or a
// measurement itself fails (an error here is a bug, not a slow machine).
func Run(ctx context.Context, cfg Config) (Report, error) {
	cfg = cfg.withDefaults()
	dir := cfg.Dir
	if dir == "" {
		d, err := os.MkdirTemp("", "offgrid-perf-")
		if err != nil {
			return Report{}, err
		}
		defer os.RemoveAll(d)
		dir = d
	} else if err := os.MkdirAll(dir, 0o755); err != nil {
		return Report{}, err
	}
	rep := Report{Meta: Meta{
		Date:     time.Now().Format("2006-01-02"),
		Hardware: sysinfo.Detect(),
		Go:       runtime.Version(),
		Dir:      dir,
		DirFS:    filesystemOf(dir),
		Governor: readTrim("/sys/devices/system/cpu/cpu0/cpufreq/scaling_governor"),
		Repeats:  cfg.Repeats,
		Seed:     cfg.Seed,
	}}
	rep.Meta.RSSPerRun = resetPeakRSS()
	r := &runner{cfg: cfg, dir: dir}

	var err error
	if rep.Store, err = r.store(ctx); err != nil {
		return rep, err
	}
	if rep.Sync, err = r.sync(ctx); err != nil {
		return rep, err
	}
	if rep.Queue, err = r.queue(ctx); err != nil {
		return rep, err
	}
	return rep, nil
}

type runner struct {
	cfg Config
	dir string
	seq int
}

func (r *runner) path(name string) string {
	r.seq++
	return filepath.Join(r.dir, fmt.Sprintf("%s-%d.db", name, r.seq))
}

// ---- the workload ----------------------------------------------------------

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

var epoch = time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)

// fill writes exactly n ops into s: a mix of new tasks, notes and edits, the
// same for the same seed. The store's clock advances one second per op.
func fill(ctx context.Context, s *sqlstore.Store, clk *fakeClock, n int, seed int64) error {
	rng := rand.New(rand.NewSource(seed))
	var ids []string
	done := 0
	statuses := []task.Status{task.StatusOpen, task.StatusInProgress, task.StatusDone}
	prios := []task.Priority{task.PriorityLow, task.PriorityMedium, task.PriorityHigh}
	for done < n {
		clk.t = clk.t.Add(time.Second)
		switch p := rng.Intn(10); {
		case len(ids) == 0 || p < 3:
			t, created, err := s.Create(ctx, task.NewTask{Title: fmt.Sprintf("task %d", done), Site: strconv.Itoa(rng.Intn(9)), Priority: prios[rng.Intn(3)]})
			if err != nil {
				return err
			}
			ids = append(ids, t.ID)
			if created {
				done++
			}
		case p < 6:
			if _, err := s.AddNote(ctx, ids[rng.Intn(len(ids))], fmt.Sprintf("note %d", done)); err != nil {
				return err
			}
			done++
		default:
			st := statuses[rng.Intn(3)]
			_, changed, err := s.Update(ctx, ids[rng.Intn(len(ids))], task.Changes{Status: &st})
			if err != nil {
				return err
			}
			if changed {
				done++
			}
		}
	}
	return nil
}

func openStore(ctx context.Context, path, node string, clk *fakeClock) (*sqlstore.Store, error) {
	return sqlstore.Open(ctx, path, sqlstore.Options{NodeID: node, Now: clk.now})
}

// ---- store -------------------------------------------------------------------

func (r *runner) store(ctx context.Context) ([]StoreRow, error) {
	var rows []StoreRow
	// append: ops written one durable transaction at a time
	n := r.cfg.Sizes[0]
	if len(r.cfg.Sizes) > 1 {
		n = r.cfg.Sizes[1]
	}
	r.cfg.Progress("store: append %d ops x%d", n, r.cfg.Repeats)
	var secs []float64
	var mem Mem
	var dbMiB float64
	for i := 0; i < r.cfg.Repeats; i++ {
		path := r.path("append")
		clk := &fakeClock{epoch}
		s, err := openStore(ctx, path, "bench", clk)
		if err != nil {
			return nil, err
		}
		m := startMem()
		t0 := time.Now()
		err = fill(ctx, s, clk, n, r.cfg.Seed)
		d := time.Since(t0)
		mem = maxMem(mem, m.stop())
		s.Close()
		if err != nil {
			return nil, err
		}
		dbMiB = fileMiB(path)
		secs = append(secs, d.Seconds())
		os.Remove(path)
	}
	sp := spread(secs)
	rows = append(rows, StoreRow{Name: "append (one durable write per op)", Ops: n, Seconds: sp, PerSec: float64(n) / sp.Median, DBMiB: dbMiB, Mem: mem})

	// rebuild and reopen on logs of each size
	for _, n := range r.cfg.Sizes {
		if err := ctx.Err(); err != nil {
			return rows, err
		}
		path := r.path("log")
		clk := &fakeClock{epoch}
		s, err := openStore(ctx, path, "bench", clk)
		if err != nil {
			return nil, err
		}
		if err := fill(ctx, s, clk, n, r.cfg.Seed); err != nil {
			return nil, err
		}
		s.Close()
		dbMiB := fileMiB(path)

		r.cfg.Progress("store: rebuild and reopen a %d-op log x%d", n, r.cfg.Repeats)
		var rb, op []float64
		var rbMem, opMem Mem
		for i := 0; i < r.cfg.Repeats; i++ {
			m := startMem()
			t0 := time.Now()
			s, err := openStore(ctx, path, "bench", clk)
			op = append(op, time.Since(t0).Seconds())
			opMem = maxMem(opMem, m.stop())
			if err != nil {
				return nil, err
			}
			m = startMem()
			t0 = time.Now()
			_, err = s.Rebuild(ctx)
			rb = append(rb, time.Since(t0).Seconds())
			rbMem = maxMem(rbMem, m.stop())
			s.Close()
			if err != nil {
				return nil, err
			}
		}
		rows = append(rows,
			StoreRow{Name: "open an existing log", Ops: n, Seconds: spread(op), DBMiB: dbMiB, Mem: opMem},
			StoreRow{Name: "rebuild the view from the log", Ops: n, Seconds: spread(rb), PerSec: float64(n) / spread(rb).Median, DBMiB: dbMiB, Mem: rbMem})
		os.Remove(path)
	}
	return rows, nil
}

// ---- sync --------------------------------------------------------------------

var benchKey = []byte("offgrid-perf-benchmark-key-not-secret")

func (r *runner) sync(ctx context.Context) ([]SyncRow, error) {
	var rows []SyncRow
	for _, n := range r.cfg.Sizes {
		if err := ctx.Err(); err != nil {
			return rows, err
		}
		rRows, err := r.syncSize(ctx, n)
		if err != nil {
			return nil, err
		}
		rows = append(rows, rRows...)
	}
	return rows, nil
}

func (r *runner) syncSize(ctx context.Context, n int) ([]SyncRow, error) {
	// the source replica: n ops, built once, not timed
	srcClk := &fakeClock{epoch}
	src, err := openStore(ctx, r.path("src"), "source", srcClk)
	if err != nil {
		return nil, err
	}
	defer src.Close()

	if err := fill(ctx, src, srcClk, n, r.cfg.Seed); err != nil {
		return nil, err
	}
	end := srcClk.t
	fresh := func(name string) (*sqlstore.Store, error) {
		return openStore(ctx, r.path(name), "dest", &fakeClock{end})
	}

	var rows []SyncRow

	// 1. a new replica catches up in process: Summary / OpsSince / ApplyRemote in batches
	r.cfg.Progress("sync: %d ops, in process x%d", n, r.cfg.Repeats)
	var secs []float64
	var mem Mem
	for i := 0; i < r.cfg.Repeats; i++ {
		dst, err := fresh("dst")
		if err != nil {
			return nil, err
		}
		m := startMem()
		t0 := time.Now()
		err = pullAll(ctx, dst, src, syncer.DefaultBatch)
		d := time.Since(t0)
		mem = maxMem(mem, m.stop())
		if err == nil {
			err = sameState(ctx, src, dst)
		}
		dst.Close()
		if err != nil {
			return nil, err
		}
		secs = append(secs, d.Seconds())
	}
	sp := spread(secs)
	rows = append(rows, SyncRow{Mode: "new replica, in process", Ops: n, Seconds: sp, PerSec: float64(n) / sp.Median, Mem: mem})

	// 2. the same over HTTP on localhost, HMAC-signed, through the real client and server
	r.cfg.Progress("sync: %d ops, HTTP on localhost x%d", n, r.cfg.Repeats)
	srv := httptest.NewServer(syncer.Handler(src, syncer.ServerOptions{Key: benchKey}))
	defer srv.Close()

	secs, mem = nil, Mem{}
	requests := 0
	for i := 0; i < r.cfg.Repeats; i++ {
		dst, err := fresh("dst")
		if err != nil {
			return nil, err
		}
		cl := &syncer.Client{Peer: srv.URL, Key: benchKey}
		m := startMem()
		t0 := time.Now()
		rep, err := cl.Once(ctx, dst)
		d := time.Since(t0)
		mem = maxMem(mem, m.stop())
		requests = rep.Requests
		if err == nil && rep.Pulled != n {
			err = fmt.Errorf("pulled %d ops, want %d", rep.Pulled, n)
		}
		if err == nil {
			err = sameState(ctx, src, dst)
		}
		dst.Close()
		if err != nil {
			return nil, err
		}
		secs = append(secs, d.Seconds())
	}
	sp = spread(secs)
	rows = append(rows, SyncRow{Mode: "new replica, HTTP on localhost", Ops: n, Seconds: sp, PerSec: float64(n) / sp.Median, Requests: requests, Mem: mem})

	// 3. the everyday case: both already in step, 10 new ops arrive
	r.cfg.Progress("sync: 10 new ops on top of %d, HTTP x%d", n, r.cfg.Repeats)
	dst, err := fresh("inc")
	if err != nil {
		return nil, err
	}
	defer dst.Close()

	cl := &syncer.Client{Peer: srv.URL, Key: benchKey}
	if _, err := cl.Once(ctx, dst); err != nil {
		return nil, err
	}
	secs, mem, requests = nil, Mem{}, 0
	for i := 0; i < r.cfg.Repeats; i++ {
		if err := fill(ctx, src, srcClk, 10, r.cfg.Seed+int64(i)+1); err != nil {
			return nil, err
		}
		m := startMem()
		t0 := time.Now()
		rep, err := cl.Once(ctx, dst)
		d := time.Since(t0)
		mem = maxMem(mem, m.stop())
		requests = rep.Requests
		if err == nil && rep.Pulled != 10 {
			err = fmt.Errorf("pulled %d ops, want 10", rep.Pulled)
		}
		if err != nil {
			return nil, err
		}
		secs = append(secs, d.Seconds())
	}
	sp = spread(secs)
	rows = append(rows, SyncRow{Mode: fmt.Sprintf("10 new ops on a %s-op log, HTTP", commas(n)), Ops: 10, Seconds: sp, PerSec: 10 / sp.Median, Requests: requests, Mem: mem})
	return rows, nil
}

func pullAll(ctx context.Context, dst, src *sqlstore.Store, batch int) error {
	for {
		have, err := dst.Summary(ctx)
		if err != nil {
			return err
		}
		ops, more, err := src.OpsSince(ctx, have, batch)
		if err != nil {
			return err
		}
		if _, err := dst.ApplyRemote(ctx, ops); err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
}

// sameState is the check that a timing means anything: the replica that caught
// up holds exactly what the source holds.
func sameState(ctx context.Context, a, b *sqlstore.Store) error {
	ha, err := a.StateHash(ctx)
	if err != nil {
		return err
	}
	hb, err := b.StateHash(ctx)
	if err != nil {
		return err
	}
	if ha != hb {
		return fmt.Errorf("the replica that caught up differs from its source (%s vs %s): the timing is meaningless", ha[:12], hb[:12])
	}
	return nil
}

// ---- queue -------------------------------------------------------------------

func (r *runner) queue(ctx context.Context) ([]QueueRow, error) {
	var rows []QueueRow
	for _, n := range r.cfg.Jobs {
		for _, workers := range r.cfg.Workers {
			if err := ctx.Err(); err != nil {
				return rows, err
			}
			r.cfg.Progress("queue: enqueue and drain %d jobs with %d at a time x%d", n, workers, r.cfg.Repeats)
			var enq, drn []float64
			var mem Mem
			for i := 0; i < r.cfg.Repeats; i++ {
				e, d, m, err := r.drainOnce(ctx, n, workers)
				if err != nil {
					return nil, err
				}
				enq, drn, mem = append(enq, e), append(drn, d), maxMem(mem, m)
			}
			sp := spread(drn)
			rows = append(rows, QueueRow{Jobs: n, Concurrency: workers, Enqueue: spread(enq), Drain: sp, DrainPerSec: float64(n) / sp.Median, Mem: mem})
		}
	}
	return rows, nil
}

func (r *runner) drainOnce(ctx context.Context, n, workers int) (enq, drn float64, mem Mem, err error) {
	path := r.path("queue")
	db, err := sqlstore.OpenDB(path)
	if err != nil {
		return 0, 0, mem, err
	}
	defer func() { db.Close(); os.Remove(path) }()
	q, err := jobqueue.New(ctx, db, jobqueue.Config{})
	if err != nil {
		return 0, 0, mem, err
	}
	m := startMem()
	t0 := time.Now()
	for i := 0; i < n; i++ {
		if _, _, err := q.Enqueue(ctx, fmt.Sprintf("bench-%d", i), "noop", []byte(`{}`), jobqueue.EnqueueOptions{}); err != nil {
			m.stop()
			return 0, 0, mem, err
		}
	}
	enq = time.Since(t0).Seconds()

	var ran atomic.Int64
	w, err := drain.New(drain.Config{
		Queue:        q,
		Specs:        map[string]drain.Spec{"noop": {Handle: func(context.Context, jobqueue.Job, drain.Progress) (string, error) { ran.Add(1); return "ok", nil }}},
		Concurrency:  workers,
		Poll:         2 * time.Millisecond,
		ExitWhenIdle: true,
	})
	if err != nil {
		m.stop()
		return 0, 0, mem, err
	}
	t0 = time.Now()
	err = w.Run(ctx)
	drn = time.Since(t0).Seconds()
	mem = m.stop()
	if err != nil {
		return 0, 0, mem, err
	}
	if int(ran.Load()) != n || w.Summary().Done != n {
		return 0, 0, mem, fmt.Errorf("drained %d of %d jobs (done %d): the timing is meaningless", ran.Load(), n, w.Summary().Done)
	}
	return enq, drn, mem, nil
}

// ---- measuring ---------------------------------------------------------------

// spread is the median, min and max of xs.
func spread(xs []float64) Spread {
	if len(xs) == 0 {
		return Spread{}
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	m := s[len(s)/2]
	if len(s)%2 == 0 {
		m = (s[len(s)/2-1] + s[len(s)/2]) / 2
	}
	return Spread{Median: m, Min: s[0], Max: s[len(s)-1]}
}

type memWatch struct {
	stopc chan struct{}
	done  chan Mem
	once  sync.Once
	mem   Mem
}

// startMem begins sampling the Go heap every 10 ms and resets the process's
// peak RSS (where the system allows), so stop() reports this phase only.
func startMem() *memWatch {
	runtime.GC()
	resetPeakRSS()
	w := &memWatch{stopc: make(chan struct{}), done: make(chan Mem, 1)}
	var peak uint64
	var mu sync.Mutex
	sample := func() {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		mu.Lock()
		if ms.HeapAlloc > peak {
			peak = ms.HeapAlloc
		}
		mu.Unlock()
	}
	sample()
	go func() {
		tk := time.NewTicker(10 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-tk.C:
				sample()
			case <-w.stopc:
				sample()
				mu.Lock()
				p := peak
				mu.Unlock()
				w.done <- Mem{HeapMiB: float64(p) / (1 << 20), RSSMiB: peakRSSMiB()}
				return
			}
		}
	}()
	return w
}

func (w *memWatch) stop() Mem {
	w.once.Do(func() {
		close(w.stopc)
		w.mem = <-w.done
	})
	return w.mem
}

func maxMem(a, b Mem) Mem {
	if b.HeapMiB > a.HeapMiB {
		a.HeapMiB = b.HeapMiB
	}
	if b.RSSMiB > a.RSSMiB {
		a.RSSMiB = b.RSSMiB
	}
	return a
}

// resetPeakRSS asks Linux to restart the process's peak-RSS counter (VmHWM).
// It returns false where that is not possible, in which case the RSS figure is
// a running maximum over the whole run and the report says so.
func resetPeakRSS() bool {
	return os.WriteFile("/proc/self/clear_refs", []byte("5"), 0o200) == nil
}

func peakRSSMiB() float64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	return ParseVmHWMMiB(string(b))
}

// ParseVmHWMMiB reads the peak resident set size from /proc/<pid>/status.
func ParseVmHWMMiB(status string) float64 {
	for _, line := range strings.Split(status, "\n") {
		if strings.HasPrefix(line, "VmHWM:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				if kib, err := strconv.ParseFloat(f[1], 64); err == nil {
					return kib / 1024
				}
			}
		}
	}
	return 0
}

func fileMiB(path string) float64 {
	var total int64
	for _, suffix := range []string{"", "-wal"} {
		if st, err := os.Stat(path + suffix); err == nil {
			total += st.Size()
		}
	}
	return float64(total) / (1 << 20)
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// filesystemOf names the filesystem type holding dir, from /proc/mounts (the
// longest mount point that is a prefix of dir). Empty if unknown.
func filesystemOf(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	b, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return ""
	}
	return FilesystemFromMounts(string(b), abs)
}

// FilesystemFromMounts picks the filesystem type for path from /proc/mounts content.
func FilesystemFromMounts(mounts, path string) string {
	best, fs := -1, ""
	for _, line := range strings.Split(mounts, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		mp := f[1]
		if path == mp || strings.HasPrefix(path, strings.TrimSuffix(mp, "/")+"/") {
			if len(mp) > best {
				best, fs = len(mp), f[2]
			}
		}
	}
	return fs
}

func commas(n int) string {
	s := strconv.Itoa(n)
	minIdx := 0
	if n < 0 {
		minIdx = 1
	}
	for i := len(s) - 3; i > minIdx; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
