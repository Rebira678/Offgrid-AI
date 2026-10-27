package syncer

import (
	"context"
	"testing"
	"time"
)

func TestSyncerSyncsAnswersOverHTTP(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{t: t0}

	storeA := newStore(t, "node-lidia", clk)
	storeB := newStore(t, "node-beka", clk)

	// Lidia creates a task and records a Tier 2 answer
	clk.t = clk.t.Add(time.Second)
	add(t, storeA, "Repair solar panel tracker")

	clk.t = clk.t.Add(time.Second)
	ok, err := storeA.RecordAnswer(ctx, "k-sync-1", "How to align tracker?", "Re-calibrate azimuth sensor at noon.", "qwen2.5:7b", "local")
	if err != nil || !ok {
		t.Fatalf("record answer failed on A: ok=%v err=%v", ok, err)
	}

	// Beka records a different answer on node-beka
	clk.t = clk.t.Add(time.Second)
	ok, err = storeB.RecordAnswer(ctx, "k-sync-2", "Battery check", "Battery charge at 94%.", "deepseek-r1:70b", "cloud")
	if err != nil || !ok {
		t.Fatalf("record answer failed on B: ok=%v err=%v", ok, err)
	}

	// Setup HTTP sync server on Beka's node; Lidia is the sync client
	client, _ := serve(t, storeB, clk, key)
	client.Batch = 1 // Force batching across HTTP round-trips

	rep, err := client.Once(ctx, storeA)
	if err != nil {
		t.Fatalf("HTTP sync failed: %v", err)
	}
	if rep.Pushed != 2 || rep.Pulled != 1 {
		t.Fatalf("unexpected sync report: %+v (want 2 pushed, 1 pulled)", rep)
	}

	// Verify both nodes hold both answers
	for _, s := range []*struct {
		name  string
		store any
	}{
		{"A", storeA},
		{"B", storeB},
	} {
		var a1, a2 string
		if s.name == "A" {
			ans1, found1, _ := storeA.Answer(ctx, "k-sync-1")
			ans2, found2, _ := storeA.Answer(ctx, "k-sync-2")
			if !found1 || !found2 {
				t.Fatalf("store %s missing answers", s.name)
			}
			a1, a2 = ans1.Text, ans2.Text
		} else {
			ans1, found1, _ := storeB.Answer(ctx, "k-sync-1")
			ans2, found2, _ := storeB.Answer(ctx, "k-sync-2")
			if !found1 || !found2 {
				t.Fatalf("store %s missing answers", s.name)
			}
			a1, a2 = ans1.Text, ans2.Text
		}
		if a1 != "Re-calibrate azimuth sensor at noon." || a2 != "Battery charge at 94%." {
			t.Fatalf("store %s content mismatch: a1=%q a2=%q", s.name, a1, a2)
		}
	}

	// Cryptographic state hash must be 100% equal on both nodes
	hashA := hash(t, storeA)
	hashB := hash(t, storeB)
	if hashA != hashB {
		t.Fatalf("state hash divergence after HTTP sync: A=%s B=%s", hashA, hashB)
	}
}
