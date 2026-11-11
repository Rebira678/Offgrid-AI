package sqlstore

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"offgrid/internal/identity"
	"offgrid/internal/task"
)

// fill appends n ops: tasks, notes and status changes in a realistic mix.
func fill(b *testing.B, s *Store, n int) {
	b.Helper()
	ctx := context.Background()
	done := task.StatusDone
	made := 0
	for ops := 0; ops < n; {
		t, created, err := s.Create(ctx, task.NewTask{Title: fmt.Sprintf("task %d", made), Site: fmt.Sprint(made % 9), Priority: task.PriorityMedium})
		if err != nil {
			b.Fatal(err)
		}
		made++
		if created {
			ops++
		}
		for k := 0; k < 3 && ops < n; k++ {
			if _, err := s.AddNote(ctx, t.ID, fmt.Sprintf("note %d", k)); err != nil {
				b.Fatal(err)
			}
			ops++
		}
		if ops < n {
			if _, changed, err := s.Update(ctx, t.ID, task.Changes{Status: &done}); err != nil {
				b.Fatal(err)
			} else if changed {
				ops++
			}
		}
	}
}

// BenchmarkAppend measures committed writes per second, each one an op plus its
// effect on the view in one durable transaction (synchronous=FULL).
func BenchmarkAppend(b *testing.B) {
	clk := &clock{t: epoch}
	s, err := Open(context.Background(), newBenchDB(b), Options{NodeID: "bench", Now: clk.now})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		clk.tick(time.Second)
		if _, _, err := s.Create(ctx, task.NewTask{Title: fmt.Sprintf("task %d", i), Priority: task.PriorityLow}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRebuild measures recomputing the whole view from a log of 1,000,
// 10,000 and 50,000 ops.
func BenchmarkRebuild(b *testing.B) {
	for _, n := range []int{1_000, 10_000, 50_000} {
		b.Run(fmt.Sprintf("%d-ops", n), func(b *testing.B) {
			clk := &clock{t: epoch}
			s, err := Open(context.Background(), newBenchDB(b), Options{NodeID: "bench", Now: clk.now})
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			fill(b, s, n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := s.Rebuild(context.Background()); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkFields measures reading the entire op log into per-field CRDT registers
// from logs of 1,000 and 10,000 ops.
func BenchmarkFields(b *testing.B) {
	for _, n := range []int{1_000, 10_000} {
		b.Run(fmt.Sprintf("%d-ops", n), func(b *testing.B) {
			clk := &clock{t: epoch}
			s, err := Open(context.Background(), newBenchDB(b), Options{NodeID: "bench", Now: clk.now})
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			fill(b, s, n)
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := s.Fields(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func newBenchDB(b *testing.B) string { return b.TempDir() + "/bench.db" }

// BenchmarkVerifyChain: the cost of recomputing every hash in a 10,000-op log.
func BenchmarkVerifyChain10k(b *testing.B) {
	ctx := context.Background()
	s, err := Open(ctx, b.TempDir()+"/bench.db", Options{NodeID: "bench"})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	fill(b, s, 10000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.VerifyChain(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSignOp measures time and allocations to ECDSA P-256 sign an op hash.
func BenchmarkSignOp(b *testing.B) {
	id, err := identity.Generate()
	if err != nil {
		b.Fatal(err)
	}
	hashBytes := []byte("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sig, err := id.Sign(hashBytes)
		if err != nil || len(sig) != 64 {
			b.Fatalf("sign error: %v", err)
		}
	}
}

// BenchmarkVerifyOp measures time and allocations to verify an ECDSA P-256 signature.
func BenchmarkVerifyOp(b *testing.B) {
	id, err := identity.Generate()
	if err != nil {
		b.Fatal(err)
	}
	hashBytes := []byte("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
	sig, err := id.Sign(hashBytes)
	if err != nil {
		b.Fatal(err)
	}
	pub := id.PublicKey()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := identity.Verify(pub, hashBytes, sig); err != nil {
			b.Fatalf("verify error: %v", err)
		}
	}
}

// BenchmarkVerifySignaturesParallel100 measures parallel verification of 100 ops.
func BenchmarkVerifySignaturesParallel100(b *testing.B) {
	ctx := context.Background()
	id, _ := identity.Generate()
	s, err := Open(ctx, b.TempDir()+"/bench_sig100.db", Options{
		NodeID:           "bench",
		Identity:         id,
		RequireSignedOps: true,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	fill(b, s, 100)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.VerifySignatures(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkVerifySignaturesParallel1000 measures parallel verification of 1,000 ops.
func BenchmarkVerifySignaturesParallel1000(b *testing.B) {
	ctx := context.Background()
	id, _ := identity.Generate()
	s, err := Open(ctx, b.TempDir()+"/bench_sig1000.db", Options{
		NodeID:           "bench",
		Identity:         id,
		RequireSignedOps: true,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	fill(b, s, 1000)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.VerifySignatures(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkApplyRemoteSignedOps measures ApplyRemote performance for a batch of 50 signed ops.
func BenchmarkApplyRemoteSignedOps(b *testing.B) {
	ctx := context.Background()
	idPeer, _ := identity.Generate()
	idLocal, _ := identity.Generate()

	// Prepare 50 signed ops
	ops := make([]Op, 50)
	prev := GenesisHash
	for i := 0; i < 50; i++ {
		op := Op{
			NodeID:   "peer-node",
			Seq:      int64(i + 1),
			TS:       time.Unix(1700000000+int64(i), 0).UTC(),
			Counter:  0,
			Kind:     KindTaskCreated,
			Payload:  []byte(fmt.Sprintf(`{"id":"T%d","title":"Bench Task %d","site":"","priority":"medium"}`, i+1, i+1)),
			PubKey:   idPeer.PublicKey(),
			DeviceID: idPeer.ID(),
			Alg:      identity.AlgECDSAP256,
		}
		op.Hash = OpHash(op, prev)
		sig, err := idPeer.Sign([]byte(op.Hash))
		if err != nil {
			b.Fatal(err)
		}
		op.Sig = sig
		prev = op.Hash
		ops[i] = op
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		dbFile := filepath.Join(b.TempDir(), fmt.Sprintf("bench_apply_%d.db", i))
		store, err := Open(ctx, dbFile, Options{
			NodeID:           "local-node",
			Identity:         idLocal,
			RequireSignedOps: true,
		})
		if err != nil {
			b.Fatal(err)
		}
		b.StartTimer()

		if _, err := store.ApplyRemote(ctx, ops); err != nil {
			b.Fatal(err)
		}

		b.StopTimer()
		store.Close()
		b.StartTimer()
	}
}
