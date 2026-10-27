package idle

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestIdleEdgeParseProcStatExoticFormats(t *testing.T) {
	// Modern Linux /proc/stat with 10 fields (including guest and guest_nice)
	modern := `# comment line
cpu  1000 50 200 4000 100 20 10 0 5 2
cpu0 500 25 100 2000 50 10 5 0 2 1
cpu1 500 25 100 2000 50 10 5 0 3 1
`
	c, err := ParseProcStat(scan(modern))
	if err != nil {
		t.Fatalf("failed to parse modern /proc/stat: %v", err)
	}
	// Idle = idle(4000) + iowait(100) = 4100
	// Total = 1000 + 50 + 200 + 4000 + 100 + 20 + 10 + 0 = 5380 (fields up to 8)
	if c.Idle != 4100 || c.Total != 5380 {
		t.Fatalf("unexpected modern counters: %+v", c)
	}

	// Mixed tabs and multiple spaces
	tabbed := "\n\n   \t  \ncpu\t\t50   10 \t 20   100   10  0 0 0\n"
	c, err = ParseProcStat(scan(tabbed))
	if err != nil {
		t.Fatalf("failed to parse tabbed /proc/stat: %v", err)
	}
	if c.Idle != 110 || c.Total != 190 {
		t.Fatalf("unexpected tabbed counters: %+v", c)
	}

	// No aggregate cpu line (only per-core cpu0, cpu1)
	perCoreOnly := "cpu0 10 20 30 40 50 0 0 0\ncpu1 10 20 30 40 50 0 0 0\n"
	if _, err := ParseProcStat(scan(perCoreOnly)); err == nil {
		t.Fatal("expected error when aggregate cpu line is missing")
	}

	// Counter overflow / max uint64 boundary
	maxLine := fmt.Sprintf("cpu %d 0 0 1000 0 0 0 0\n", uint64(1<<63))
	c, err = ParseProcStat(scan(maxLine))
	if err != nil {
		t.Fatalf("failed to parse large uint64: %v", err)
	}
	if c.Total != uint64(1<<63)+1000 {
		t.Fatalf("unexpected total for large uint64: %d", c.Total)
	}
}

func TestIdleEdgeSamplerDefaults(t *testing.T) {
	// A completely zero-value Sampler
	var s Sampler
	// Should not panic, uses default ReadProcStat, time.Now, 0.5 MaxBusy, 2s Window
	// The first call should always return false (or true if /proc/stat unreadable)
	_ = s.Quiet()
	_ = s.Busy()
}

func TestIdleEdgeSamplerConcurrentAccess(t *testing.T) {
	now := time.Now()
	var total atomic.Uint64
	var idleTicks atomic.Uint64
	total.Store(1000)
	idleTicks.Store(800)

	s := &Sampler{
		Read: func() (Counters, error) {
			return Counters{
				Total: total.Load(),
				Idle:  idleTicks.Load(),
			}, nil
		},
		Now:     func() time.Time { return now },
		MaxBusy: 0.5,
		Window:  time.Millisecond,
	}

	const goroutines = 20
	const iterations = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				_ = s.Quiet()
				_ = s.Busy()
				if j%10 == 0 {
					total.Add(100)
					idleTicks.Add(60)
				}
			}
		}(i)
	}

	wg.Wait()
}

func TestIdleEdgeSamplerBoundaryMaxBusy(t *testing.T) {
	currTime := time.Unix(1000, 0)
	total := uint64(1000)
	idleVal := uint64(900)

	s := &Sampler{
		Read: func() (Counters, error) {
			return Counters{Total: total, Idle: idleVal}, nil
		},
		Now:     func() time.Time { return currTime },
		MaxBusy: 0.05, // Only 5% busy allowed
		Window:  time.Second,
	}

	// 1st look: baseline
	if s.Quiet() {
		t.Fatal("expected false on first baseline look")
	}

	// 2nd look: 10% busy (10 busy / 100 total) -> above 5% -> NOT quiet
	currTime = currTime.Add(2 * time.Second)
	total += 100
	idleVal += 90
	if s.Quiet() {
		t.Fatal("10% busy should NOT be quiet when MaxBusy is 5%")
	}

	// 3rd look: 3% busy (3 busy / 100 total) -> below 5% -> quiet!
	currTime = currTime.Add(2 * time.Second)
	total += 100
	idleVal += 97
	if !s.Quiet() {
		t.Fatal("3% busy SHOULD be quiet when MaxBusy is 5%")
	}
}

func TestIdleEdgeSamplerRecoveryAfterReset(t *testing.T) {
	currTime := time.Unix(1000, 0)
	total := uint64(10000)
	idleVal := uint64(5000)

	s := &Sampler{
		Read: func() (Counters, error) {
			return Counters{Total: total, Idle: idleVal}, nil
		},
		Now:     func() time.Time { return currTime },
		MaxBusy: 0.5,
		Window:  time.Second,
	}

	s.Quiet() // baseline

	// Busy period: 80% busy
	currTime = currTime.Add(2 * time.Second)
	total += 1000
	idleVal += 200
	if s.Quiet() {
		t.Fatal("expected busy")
	}

	// Sudden kernel counter reset (e.g. Total drops to 100, Idle drops to 50)
	currTime = currTime.Add(2 * time.Second)
	total = 100
	idleVal = 50
	if s.Quiet() {
		t.Fatal("verdict must be retained on reset look")
	}

	// Next look after reset: 10% busy relative to new baseline -> sampler MUST recover to quiet!
	currTime = currTime.Add(2 * time.Second)
	total += 100
	idleVal += 90
	if !s.Quiet() {
		t.Fatal("sampler failed to recover after counter reset")
	}
}

func TestIdleParseProcStat128CoresAndWindowsCRLF(t *testing.T) {
	var b strings.Builder
	b.WriteString("cpu  128000 6400 25600 512000 12800 2560 1280 0\r\n")
	for i := 0; i < 128; i++ {
		fmt.Fprintf(&b, "cpu%d 1000 50 200 4000 100 20 10 0\r\n", i)
	}
	b.WriteString("intr 123456789\r\nctxt 987654321\r\nbtime 1700000000\r\n")
	c, err := ParseProcStat(scan(b.String()))
	if err != nil {
		t.Fatalf("failed to parse 128-core CRLF stats: %v", err)
	}
	if c.Idle != 512000+12800 {
		t.Fatalf("unexpected idle ticks: %d", c.Idle)
	}
	if c.Total != 128000+6400+25600+512000+12800+2560+1280 {
		t.Fatalf("unexpected total ticks: %d", c.Total)
	}
}

func TestIdleSamplerBackwardsClockJump(t *testing.T) {
	currTime := time.Unix(5000, 0)
	total := uint64(1000)
	idleVal := uint64(500)
	reads := 0

	s := &Sampler{
		Read: func() (Counters, error) {
			reads++
			return Counters{Total: total, Idle: idleVal}, nil
		},
		Now:     func() time.Time { return currTime },
		MaxBusy: 0.5,
		Window:  2 * time.Second,
	}

	// 1. Initial look at t=5000
	s.Quiet()
	if reads != 1 {
		t.Fatalf("expected 1 read, got %d", reads)
	}

	// 2. Normal look within window (t=5001) -> cached verdict, no read
	currTime = currTime.Add(time.Second)
	s.Quiet()
	if reads != 1 {
		t.Fatalf("look within window triggered read unexpectedly: %d", reads)
	}

	// 3. NTP / virtualization steps clock backwards by 1 hour to t=1401!
	currTime = time.Unix(1401, 0)
	total += 200
	idleVal += 180 // 10% busy
	s.Quiet()

	// With backwards clock jump protection, this must NOT be trapped in a 1-hour freeze;
	// it must invoke Read again and refresh measurements immediately!
	if reads != 2 {
		t.Fatalf("backwards clock jump failed to trigger refresh, reads=%d", reads)
	}
}
