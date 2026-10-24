package syncer

import (
	"context"
	"net/http"
	"testing"
	"time"

	"offgrid/internal/task"
)

func BenchmarkSignRequest(b *testing.B) {
	body := []byte(`{"have":{"node-a":100,"node-b":50},"limit":500}`)
	unix := time.Now().Unix()
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = SignRequest(key, "POST", PathOps, "node-bench", unix, body)
	}
}

func BenchmarkSignResponse(b *testing.B) {
	reqSig := "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
	body := []byte(`{"applied":10,"duplicates":2,"rebuilt":false}`)
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = SignResponse(key, reqSig, 200, body)
	}
}

func BenchmarkVerifyRequest(b *testing.B) {
	body := []byte(`{"have":{"node-a":100},"limit":500}`)
	now := time.Now()
	unix := now.Unix()
	sig := SignRequest(key, "POST", PathOps, "node-bench", unix, body)

	req, _ := http.NewRequest("POST", "http://localhost:7878"+PathOps, nil)
	req.Header.Set(HeaderNode, "node-bench")
	req.Header.Set(HeaderTime, "1729760000")
	req.Header.Set(HeaderSig, sig)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, _ = verifyRequest(key, req, body, now, 5*time.Minute)
	}
}

func BenchmarkSyncRoundTrip(b *testing.B) {
	ctx := context.Background()
	clk := &fakeClock{t0}

	sA := newStore(b, "node-a", clk)
	sB := newStore(b, "node-b", clk)

	// Pre-populate replica A with tasks
	for i := 0; i < 20; i++ {
		add(b, sA, "Pre-populated task")
	}

	cl, _ := serve(b, sB, clk, key)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		// Create 1 new task on A per iteration
		clk.t = clk.t.Add(time.Second)
		_, _, _ = sA.Create(ctx, task.NewTask{Title: "Benchmark iteration task"})

		_, err := cl.Once(ctx, sA)
		if err != nil {
			b.Fatal(err)
		}
	}
}
