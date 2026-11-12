package sqlstore

import (
	"context"
	"fmt"
	"testing"

	"offgrid/internal/identity"
)

// Run on the hardware you care about (the Pi, the old laptop), not just here:
//
//	go test -run '^$' -bench 'Sig|Signed' -benchmem ./internal/store/sqlstore
//
// docs/day-36.md records the numbers with the machine they came from.

func benchKey(b *testing.B) *identity.Identity {
	b.Helper()
	id, err := identity.Generate()
	if err != nil {
		b.Fatal(err)
	}
	return id
}

// BenchmarkSignOp is the cost of one signature: what every write pays.
func BenchmarkSignOp(b *testing.B) {
	id := benchKey(b)
	msg := sigMessage(GenesisHash)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := id.Sign(msg); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkVerifyOp is the cost of one verification on one core: what every received op pays.
func BenchmarkVerifyOp(b *testing.B) {
	id := benchKey(b)
	msg := sigMessage(GenesisHash)
	sig, _ := id.Sign(msg)
	pub := id.PublicKey()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := identity.Verify(pub, msg, sig); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkVerifyPool is the pool (GOMAXPROCS workers) over 1,000 signatures: how long a big batch waits.
func BenchmarkVerifyPool1000(b *testing.B) {
	id := benchKey(b)
	jobs := make([]sigJob, 1000)
	for i := range jobs {
		msg := sigMessage(fmt.Sprintf("%064d", i))
		sig, _ := id.Sign(msg)
		jobs[i] = sigJob{idx: i, key: id.PublicKey(), msg: msg, sig: sig}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := verifyPool(jobs); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRecord measures a local write, unsigned and signed, so the price of signing is the difference.
func BenchmarkRecord(b *testing.B) {
	for _, signed := range []bool{false, true} {
		b.Run(fmt.Sprintf("signed=%v", signed), func(b *testing.B) {
			opt := Options{NodeID: "bench"}
			if signed {
				opt.Signer = benchKey(b)
			}
			s, err := Open(context.Background(), newBenchDB(b), opt)
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, _, err := s.Create(context.Background(), newTask(fmt.Sprintf("task %d", i))); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkApplyRemote1000 takes in 1,000 ops from another replica, unsigned and signed.
func BenchmarkApplyRemote1000(b *testing.B) {
	for _, signed := range []bool{false, true} {
		b.Run(fmt.Sprintf("signed=%v", signed), func(b *testing.B) {
			ctx := context.Background()
			opt := Options{NodeID: "sender"}
			if signed {
				opt.Signer = benchKey(b)
			}
			src, err := Open(ctx, newBenchDB(b), opt)
			if err != nil {
				b.Fatal(err)
			}
			defer src.Close()
			for i := 0; i < 1000; i++ {
				if _, _, err := src.Create(ctx, newTask(fmt.Sprintf("task %d", i))); err != nil {
					b.Fatal(err)
				}
			}
			ops, _, err := src.OpsSince(ctx, Summary{}, 1000)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				dst, err := Open(ctx, newBenchDB(b), Options{NodeID: "receiver"})
				if err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				if _, err := dst.ApplyRemote(ctx, ops); err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				dst.Close()
				b.StartTimer()
			}
		})
	}
}
