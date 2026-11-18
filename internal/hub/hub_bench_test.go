package hub

import (
	"context"
	"testing"
	"time"

	"offgrid/internal/identity"
)

func BenchmarkLimiterAllow(b *testing.B) {
	l := NewLimiter(1000, 1000, time.Now)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		l.Allow()
	}
}

func BenchmarkHubLookup(b *testing.B) {
	id, err := identity.Generate()
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	h, err := New(ctx, Options{Dir: b.TempDir(), Identity: id})
	if err != nil {
		b.Fatal(err)
	}
	defer h.Close()

	if err := h.AddGroup(ctx, "bench-group", id.ID()); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		g := h.lookup("bench-group")
		if g == nil {
			b.Fatal("group not found")
		}
	}
}
