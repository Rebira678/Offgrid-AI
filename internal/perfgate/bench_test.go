package perfgate

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

func BenchmarkPValue10Samples(b *testing.B) {
	a := []float64{10, 12, 11, 13, 10, 14, 11, 12, 13, 10}
	c := []float64{15, 16, 15, 17, 16, 18, 17, 15, 16, 17}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		PValue(a, c)
	}
}

func BenchmarkPValue50Samples(b *testing.B) {
	a := make([]float64, 50)
	c := make([]float64, 50)
	for i := 0; i < 50; i++ {
		a[i] = float64(100 + i%7)
		c[i] = float64(150 + i%7)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		PValue(a, c)
	}
}

func BenchmarkParse500Lines(b *testing.B) {
	var sb strings.Builder
	sb.WriteString("goos: linux\ncpu: Benchmark CPU\npkg: offgrid/internal/crdt\n")
	for i := 0; i < 500; i++ {
		sb.WriteString(fmt.Sprintf("BenchmarkItem%d-8 \t 1000 \t %d ns/op \t %d B/op \t %d allocs/op\n", i%10, 100+i, 32+i%8, i%4))
	}
	content := sb.String()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = Parse(strings.NewReader(content))
	}
}

func TestParseFuzzingGarbage(t *testing.T) {
	rng := rand.New(rand.NewSource(12345))

	for i := 0; i < 500; i++ {
		buf := make([]byte, rng.Intn(1024))
		rng.Read(buf)

		// Parse must never panic, even on random binary noise
		_, _ = Parse(strings.NewReader(string(buf)))
	}
}

func TestStatisticalSymmetryAndConvergence(t *testing.T) {
	rng := rand.New(rand.NewSource(54321))

	for iter := 0; iter < 100; iter++ {
		n1 := 5 + rng.Intn(15)
		n2 := 5 + rng.Intn(15)

		s1 := make([]float64, n1)
		s2 := make([]float64, n2)

		for i := range s1 {
			s1[i] = rng.Float64() * 100
		}
		for i := range s2 {
			s2[i] = rng.Float64() * 100
		}

		p1 := PValue(s1, s2)
		p2 := PValue(s2, s1)

		diff := p1 - p2
		if diff < 0 {
			diff = -diff
		}
		if diff > 1e-12 {
			t.Fatalf("iter %d: asymmetry detected: PValue(s1, s2)=%v, PValue(s2, s1)=%v", iter, p1, p2)
		}
	}
}
