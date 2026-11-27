package governor

import (
	"context"
	"testing"
	"time"

	"offgrid/internal/llm"
	"offgrid/internal/sysinfo"
)

func BenchmarkLimiterAcquireRelease(b *testing.B) {
	l := newLimiter("bench", 10, 10, time.Second)
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rel, err := l.acquire(ctx)
		if err != nil {
			b.Fatal(err)
		}
		rel()
	}
}

func BenchmarkGovernorWrapChat(b *testing.B) {
	cfg := Defaults(Full)
	cfg.LocalSlots = 100
	g := New(cfg, FixedPower(false), func() bool { return true })

	fastClient := &llm.Fake{
		Handler: func(ctx context.Context, req llm.Request) (string, error) {
			return "response", nil
		},
	}
	wrapped := g.Wrap(fastClient, false)
	req := llm.Request{Model: "m", Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}}}
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := wrapped.Chat(ctx, req, nil)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseProfile(b *testing.B) {
	info := sysinfo.Info{MemGiB: 16, Threads: 8}
	cases := []string{"light", "full", "auto", "  FULL  "}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := ParseProfile(cases[i%len(cases)], info)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSysfsPowerCheck(b *testing.B) {
	dir := b.TempDir()
	p := Sysfs{Root: dir}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = p.OnBattery()
	}
}
