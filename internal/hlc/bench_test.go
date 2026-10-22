package hlc

import (
	"testing"
)

func BenchmarkNow(b *testing.B) {
	c := New(Config{})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.Now()
	}
}

func BenchmarkNowParallel(b *testing.B) {
	c := New(Config{})
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = c.Now()
		}
	})
}

func BenchmarkUpdate(b *testing.B) {
	c := New(Config{})
	remote := Timestamp{Wall: 1000, Counter: 5}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = c.Update(remote)
	}
}

func BenchmarkString(b *testing.B) {
	ts := Timestamp{Wall: 1700000000000000000, Counter: 42}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ts.String()
	}
}

func BenchmarkParse(b *testing.B) {
	s := "2023-11-14T22:13:20Z#42"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = Parse(s)
	}
}
