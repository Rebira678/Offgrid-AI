package growth

import (
	"path/filepath"
	"testing"
	"time"
)

func BenchmarkProjectCalendar(b *testing.B) {
	s := sample(30, 50*mb, 100*mb)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Project(s, true)
	}
}

func BenchmarkProjectActiveDays(b *testing.B) {
	s := sample(30, 50*mb, 100*mb)
	s.ActiveDays = 20
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Project(s, false)
	}
}

func BenchmarkCheckAlarms(b *testing.B) {
	now := sample(30, 100*mb, 200*mb)
	hist := make([]Sample, 50)
	for i := range hist {
		hist[i] = Sample{
			At:        now.At.Add(-time.Duration(50-i) * 24 * time.Hour),
			DBBytes:   int64(i+1) * 2 * mb,
			BlobBytes: int64(i+1) * 4 * mb,
		}
	}
	limits := Limits{
		MaxOps:        500000,
		MaxDB:         1024 * mb,
		MaxBlobs:      8192 * mb,
		MaxDBPerDay:   5 * mb,
		MaxBlobPerDay: 200 * mb,
		YearDB:        1024 * mb,
		YearBl:        16384 * mb,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Check(now, hist, limits)
	}
}

func BenchmarkHistoryAdd(b *testing.B) {
	dir := b.TempDir()
	h := History{Path: filepath.Join(dir, "bench.growth.json")}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := Sample{At: base, Ops: 100, DBBytes: 10 * mb}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.At = base.Add(time.Duration(i*2) * time.Hour)
		s.Ops = int64(i + 1)
		if err := h.Add(s); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHistoryLoad(b *testing.B) {
	dir := b.TempDir()
	h := History{Path: filepath.Join(dir, "bench.load.json")}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 100; i++ {
		if err := h.Add(Sample{At: base.Add(time.Duration(i*2) * time.Hour), Ops: int64(i + 1)}); err != nil {
			b.Fatal(err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := h.Load(); err != nil {
			b.Fatal(err)
		}
	}
}
