package sqlstore

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

func TestOpHashCollisionResistanceAcross10kOps(t *testing.T) {
	const totalOps = 10000
	seen := make(map[string]int, totalOps)
	rng := rand.New(rand.NewSource(99))

	kinds := []string{KindTaskCreated, KindTaskUpdated, KindNoteAdded, KindNoteRemoved, KindTaskRemoved, KindSuggestionMade, KindSuggestionResolved}

	for i := 0; i < totalOps; i++ {
		nodeID := fmt.Sprintf("node-%d", i%7)
		seq := int64(i/7 + 1)
		ts := time.Unix(int64(1700000000+i), int64(rng.Intn(1000000))).UTC()
		counter := uint32(rng.Intn(1000))
		kind := kinds[rng.Intn(len(kinds))]
		payload := fmt.Sprintf(`{"i":%d,"title":"task %d","entropy":"%x"}`, i, i, rng.Int63())

		op := Op{
			NodeID:  nodeID,
			Seq:     seq,
			TS:      ts,
			Counter: counter,
			Kind:    kind,
			Payload: json.RawMessage(payload),
		}

		prev := fmt.Sprintf("prev-hash-%d", i/10)
		if i == 0 {
			prev = GenesisHash
		}

		h := OpHash(op, prev)
		if firstIndex, exists := seen[h]; exists {
			t.Fatalf("hash collision detected between op %d and op %d: hash=%s", firstIndex, i, h)
		}
		seen[h] = i
	}
}

func TestStmtCacheRapidTransactionChurn(t *testing.T) {
	ctx := context.Background()
	db := open(t, newDB(t), Options{NodeID: "churn-node"}).db

	// Perform 250 sequential transactions using stmtCache
	for i := 0; i < 250; i++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("iteration %d: BeginTx failed: %v", i, err)
		}
		c := cached(tx)

		// Create table on first iteration
		if i == 0 {
			if _, err := c.ExecContext(ctx, "CREATE TABLE t_churn (id INT PRIMARY KEY, val TEXT)"); err != nil {
				t.Fatalf("create table failed: %v", err)
			}
		}

		// Perform cached insert and select
		if _, err := c.ExecContext(ctx, "INSERT INTO t_churn (id, val) VALUES (?, ?)", i, fmt.Sprintf("v-%d", i)); err != nil {
			t.Fatalf("iteration %d: insert failed: %v", i, err)
		}

		var gotVal string
		if err := c.QueryRowContext(ctx, "SELECT val FROM t_churn WHERE id = ?", i).Scan(&gotVal); err != nil {
			t.Fatalf("iteration %d: query failed: %v", i, err)
		}
		if gotVal != fmt.Sprintf("v-%d", i) {
			t.Fatalf("iteration %d: got %q, want %q", i, gotVal, fmt.Sprintf("v-%d", i))
		}

		if err := tx.Commit(); err != nil {
			t.Fatalf("iteration %d: commit failed: %v", i, err)
		}
	}

	// Verify total count
	var total int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t_churn").Scan(&total); err != nil {
		t.Fatalf("final count query failed: %v", err)
	}
	if total != 250 {
		t.Fatalf("expected 250 committed rows, got %d", total)
	}
}

func TestStmtCacheOversizedQueryText(t *testing.T) {
	ctx := context.Background()
	db := open(t, newDB(t), Options{NodeID: "oversized-node"}).db

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	c := cached(tx)

	// Create 16KB query with extensive SQL comments
	padding := strings.Repeat("/* padding commentary */\n", 400)
	longQuery := fmt.Sprintf("SELECT 42 %s", padding)

	var result int
	if err := c.QueryRowContext(ctx, longQuery).Scan(&result); err != nil {
		t.Fatalf("failed to execute oversized query: %v", err)
	}
	if result != 42 {
		t.Fatalf("got %d, want 42", result)
	}

	// Verify it was cached and can be called again
	var result2 int
	if err := c.QueryRowContext(ctx, longQuery).Scan(&result2); err != nil {
		t.Fatalf("failed to execute cached oversized query: %v", err)
	}
	if result2 != 42 {
		t.Fatalf("got %d, want 42", result2)
	}
}

func BenchmarkOpHashPayloadSizes(b *testing.B) {
	sizes := []int{32, 256, 1024, 8192, 65536}

	for _, sz := range sizes {
		b.Run(fmt.Sprintf("%d-bytes", sz), func(b *testing.B) {
			payload := json.RawMessage(`{"data":"` + strings.Repeat("a", sz) + `"}`)
			op := Op{
				NodeID:  "bench-node",
				Seq:     1,
				TS:      time.Unix(1700000000, 0).UTC(),
				Counter: 1,
				Kind:    KindTaskCreated,
				Payload: payload,
			}
			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				OpHash(op, GenesisHash)
			}
		})
	}
}
