package sim

import "testing"

func BenchmarkRun(b *testing.B) {
	for i := 0; i < b.N; i++ {
		if r := Run(int64(i+1), Config{}); r.Failure != nil {
			b.Fatal(r.Failure)
		}
	}
}
