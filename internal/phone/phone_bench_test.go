package phone

import (
	"bytes"
	"context"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"offgrid/internal/identity"
	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
)

func BenchmarkSessionSealAndOpen(b *testing.B) {
	s := signer{key: bytes.Repeat([]byte{0x55}, MinSecret)}
	now := time.Now()
	const dev = "dev_1234567890abcdef"

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		tok := s.issue(dev, now, 30*24*time.Hour)
		c, err := s.verify(tok, now)
		if err != nil || c.Device != dev {
			b.Fatalf("verify failed: %v", err)
		}
	}
}

func BenchmarkLimiterAllowedConcurrent(b *testing.B) {
	l := newLimiter()
	now := time.Now()

	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			ip := fmt.Sprintf("192.168.1.%d", i%256)
			_ = l.allowed(ip, now)
			i++
		}
	})
}

func BenchmarkSyncBatchProcessing(b *testing.B) {
	f, _ := identity.Generate()
	st, err := sqlstore.Open(context.Background(), filepath.Join(b.TempDir(), "bench.db"), sqlstore.Options{
		NodeID: "site",
		Signer: f,
		Now:    time.Now,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	_ = st.Found(context.Background(), "site")

	secret := bytes.Repeat([]byte{0x77}, MinSecret)
	cfg := Config{Store: st, Secret: secret, Now: time.Now}
	hd, _, err := build(cfg)
	if err != nil {
		b.Fatal(err)
	}

	// Pair a bench phone
	inv, _ := st.CreateInvite(context.Background(), sqlstore.RoleMember, "BenchPhone", 15*time.Minute)
	phoneID, _ := identity.Generate()
	proof, _ := sqlstore.EnrollProof(inv.Token, phoneID, "BenchPhone")
	en, err := st.RedeemInvite(context.Background(), inv.Token, phoneID.PublicKey(), "BenchPhone", proof)
	if err != nil {
		b.Fatal(err)
	}
	sess := session{Device: en.Device, Label: en.Label}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		createID := fmt.Sprintf("bench-cmd-%d-01", i)
		noteID := fmt.Sprintf("bench-cmd-%d-02", i)
		refs := map[string]string{}

		cmd1 := Command{
			ID:       createID,
			Op:       "create",
			Title:    fmt.Sprintf("Bench Task %d", i),
			Priority: "medium",
			AgeMS:    100,
		}
		res1 := hd.apply(context.Background(), sess, cmd1, refs)
		if res1.Task != "" {
			refs[createID] = res1.Task
		}

		cmd2 := Command{
			ID:   noteID,
			Op:   "note",
			Task: "@" + createID,
			Text: "Log telemetry inspection",
		}
		_ = hd.apply(context.Background(), sess, cmd2, refs)
	}
}

func BenchmarkStateRead(b *testing.B) {
	f, _ := identity.Generate()
	st, err := sqlstore.Open(context.Background(), filepath.Join(b.TempDir(), "state.db"), sqlstore.Options{
		NodeID: "site",
		Signer: f,
		Now:    time.Now,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	_ = st.Found(context.Background(), "site")

	// Pre-populate 50 tasks
	for i := 0; i < 50; i++ {
		_, _, _ = st.Create(context.Background(), task.NewTask{
			Title:    fmt.Sprintf("Task %d", i),
			Priority: task.PriorityMedium,
		})
	}

	secret := bytes.Repeat([]byte{0x88}, MinSecret)
	cfg := Config{Store: st, Secret: secret, Now: time.Now}
	hd, _, err := build(cfg)
	if err != nil {
		b.Fatal(err)
	}
	sess := session{Device: "dev_bench", Label: "BenchPhone"}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/state", nil)
		hd.state(w, req, sess)
	}
}
