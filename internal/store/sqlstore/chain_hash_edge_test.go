package sqlstore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOpHashVarintBoundaries(t *testing.T) {
	// Boundary lengths for varint encoding:
	// 1 byte: 0..127
	// 2 bytes: 128..16383
	// 3 bytes: 16384..2097151
	// 4 bytes: 2097152..
	lengths := []int{
		0, 1, 126, 127, 128, 129,
		16382, 16383, 16384, 16385,
		65535, 65536,
	}

	for _, l := range lengths {
		payload := bytes.Repeat([]byte("a"), l)
		op := Op{
			NodeID:  "boundary-node",
			Seq:     int64(l),
			TS:      time.Unix(1700000000+int64(l), 0).UTC(),
			Counter: uint32(l),
			Kind:    KindTaskCreated,
			Payload: json.RawMessage(payload),
		}
		got := OpHash(op, GenesisHash)
		want := opHashReference(op, GenesisHash)
		if got != want {
			t.Fatalf("length %d: got %s, want %s", l, got, want)
		}
	}
}

func TestOpHashScratchBufferGrowthAndReclaim(t *testing.T) {
	// Warm up pool
	smallOp := Op{
		NodeID:  "node-small",
		Seq:     1,
		TS:      time.Unix(100, 0).UTC(),
		Counter: 1,
		Kind:    KindNoteAdded,
		Payload: json.RawMessage(`{"note":"initial"}`),
	}
	smallHashBefore := OpHash(smallOp, GenesisHash)

	// Allocate a large 1MB payload to force buffer growth in hashScratch
	largePayload := `{"data":"` + strings.Repeat("X", 1024*1024) + `"}`
	largeOp := Op{
		NodeID:  "node-large",
		Seq:     2,
		TS:      time.Unix(200, 0).UTC(),
		Counter: 2,
		Kind:    KindTaskCreated,
		Payload: json.RawMessage(largePayload),
	}
	largeGot := OpHash(largeOp, GenesisHash)
	largeWant := opHashReference(largeOp, GenesisHash)
	if largeGot != largeWant {
		t.Fatalf("large op hash mismatch: got %s, want %s", largeGot, largeWant)
	}

	// Verify subsequent small ops reuse the pool without buffer corruption
	for i := 0; i < 50; i++ {
		got := OpHash(smallOp, GenesisHash)
		if got != smallHashBefore {
			t.Fatalf("iteration %d: small op hash changed after large buffer reuse: got %s, want %s", i, got, smallHashBefore)
		}
	}
}

func TestOpHashExtremeFieldValues(t *testing.T) {
	testCases := []struct {
		name string
		op   Op
		prev string
	}{
		{
			name: "zero-everything",
			op: Op{
				NodeID:  "",
				Seq:     0,
				TS:      time.Time{},
				Counter: 0,
				Kind:    "",
				Payload: nil,
			},
			prev: "",
		},
		{
			name: "max-int64-seq-and-max-uint32-counter",
			op: Op{
				NodeID:  strings.Repeat("N", 256),
				Seq:     math.MaxInt64,
				TS:      time.Unix(1<<62-1, 999999999).UTC(),
				Counter: math.MaxUint32,
				Kind:    "custom.extreme.kind",
				Payload: json.RawMessage(`{"ok":true}`),
			},
			prev: GenesisHash,
		},
		{
			name: "negative-seq-and-unix-zero",
			op: Op{
				NodeID:  "neg-node",
				Seq:     -42,
				TS:      time.Unix(0, 0).UTC(),
				Counter: 0,
				Kind:    KindTaskRemoved,
				Payload: json.RawMessage(`{}`),
			},
			prev: "0000000000000000000000000000000000000000000000000000000000000000",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := OpHash(tc.op, tc.prev)
			want := opHashReference(tc.op, tc.prev)
			if got != want {
				t.Fatalf("mismatch: got %s, want %s", got, want)
			}
		})
	}
}

func TestOpHashMalformedAndNonObjectJSON(t *testing.T) {
	cases := []string{
		`[1, 2, 3, "four", true]`,
		`"bare-string-json"`,
		`12345.6789`,
		`null`,
		`true`,
		`false`,
		`{"unclosed": "string`,
		`{"trailing": "comma",}`,
		`[1, 2, 3,]`,
		`plain text with \x00 null bytes and \n newlines`,
		`{"arabic": "مرحبا بالعالم", "chinese": "你好世界", "emoji": "🚀🛡️⚡"}`,
	}

	for i, payloadStr := range cases {
		t.Run(fmt.Sprintf("case-%d", i), func(t *testing.T) {
			op := Op{
				NodeID:  "node-json-test",
				Seq:     int64(i + 1),
				TS:      time.Unix(int64(1700000000+i), 500).UTC(),
				Counter: uint32(i),
				Kind:    KindNoteAdded,
				Payload: json.RawMessage(payloadStr),
			}
			got := OpHash(op, GenesisHash)
			want := opHashReference(op, GenesisHash)
			if got != want {
				t.Fatalf("payload %q: got %s, want %s", payloadStr, got, want)
			}
		})
	}
}

func TestOpHashConcurrentStress32Workers(t *testing.T) {
	const workers = 32
	const iters = 200

	var wg sync.WaitGroup
	errCh := make(chan error, workers)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(workerID * 1000)))

			for i := 0; i < iters; i++ {
				op := Op{
					NodeID:  fmt.Sprintf("worker-%d-node", workerID),
					Seq:     int64(i + 1),
					TS:      time.Unix(int64(1700000000+i), int64(rng.Intn(1000000))).UTC(),
					Counter: uint32(rng.Intn(500)),
					Kind:    KindTaskCreated,
					Payload: json.RawMessage(fmt.Sprintf(`{"worker":%d,"iter":%d,"rnd":%d}`, workerID, i, rng.Int63())),
				}
				prev := fmt.Sprintf("prev-%d-%d", workerID, i)
				got := OpHash(op, prev)
				want := opHashReference(op, prev)
				if got != want {
					errCh <- fmt.Errorf("worker %d iter %d mismatch: got %s, want %s", workerID, i, got, want)
					return
				}
			}
		}(w)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatal(err)
	}
}
