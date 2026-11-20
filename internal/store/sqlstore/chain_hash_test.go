package sqlstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"strconv"
	"testing"
	"time"
)

// opHashReference is OpHash as it was written on Day 27: a fresh hasher, a byte-slice conversion
// per field. It is kept here so that the faster OpHash can never drift from it: the hash chain is
// stored on disk and exchanged between replicas, so a change in what is hashed would make every
// existing log look tampered with.
func opHashReference(op Op, prev string) string {
	h := sha256.New()
	put := func(b []byte) {
		var n [binary.MaxVarintLen64]byte
		h.Write(n[:binary.PutUvarint(n[:], uint64(len(b)))])
		h.Write(b)
	}
	put([]byte("offgrid-op-v1"))
	put([]byte(op.NodeID))
	put([]byte(strconv.FormatInt(op.Seq, 10)))
	put([]byte(strconv.FormatInt(nanos(op.TS), 10)))
	put([]byte(strconv.FormatUint(uint64(op.Counter), 10)))
	put([]byte(op.Kind))
	put([]byte(prev))
	var c bytes.Buffer
	if err := json.Compact(&c, op.Payload); err != nil {
		c.Reset()
		c.Write(op.Payload)
	}
	put(c.Bytes())
	return hex.EncodeToString(h.Sum(nil))
}

func TestOpHashMatchesTheReference(t *testing.T) {
	rng := rand.New(rand.NewSource(45))
	payloads := []string{
		`{"id":"T1.a","title":"x"}`,
		"{ \"id\" : \"T1.a\",\n  \"title\" : \"spaced\" }",
		`{"title":"héllo wörld ☃","site":""}`,
		`not json at all`,
		``,
		`[1, 2, 3]`,
		string(bytes.Repeat([]byte(`{"a":1}`), 200)), // longer than the scratch buffer starts with
	}
	for i := 0; i < 2000; i++ {
		op := Op{
			NodeID:  fmt.Sprintf("node-%d-%s", rng.Intn(5), string(rune('a'+rng.Intn(26)))),
			Seq:     rng.Int63n(1 << 40),
			TS:      time.Unix(0, rng.Int63n(1<<62)).UTC(),
			Counter: uint32(rng.Int63n(1 << 32)),
			Kind:    []string{KindTaskCreated, KindNoteAdded, "weird.kind", ""}[rng.Intn(4)],
			Payload: json.RawMessage(payloads[rng.Intn(len(payloads))]),
		}
		prev := hex.EncodeToString(sha256.New().Sum([]byte{byte(i)}))
		if i%7 == 0 {
			prev = GenesisHash
		}
		if got, want := OpHash(op, prev), opHashReference(op, prev); got != want {
			t.Fatalf("op %d: OpHash %s, reference %s", i, got, want)
		}
	}
}

// Pinned values, computed with the Day 27 code before the rewrite: a change here means every
// stored chain in the field stops verifying.
const goldenOpHash = "6b7c18bf654b0acc3bc0c28c5359d664e7e601eb37da35cda23dee0fda0e8840"

func TestOpHashGoldenValues(t *testing.T) {
	op := Op{NodeID: "laptop", Seq: 7, TS: time.Unix(1700000000, 123456789).UTC(), Counter: 3, Kind: KindTaskCreated,
		Payload: json.RawMessage(`{ "id": "T1.laptop", "title": "pin me" }`)}
	if got, want := OpHash(op, GenesisHash), goldenOpHash; got != want {
		t.Fatalf("OpHash = %s, want %s", got, want)
	}
}

func TestOpHashAllocatesLittle(t *testing.T) {
	op := Op{NodeID: "laptop", Seq: 7, TS: time.Unix(1700000000, 0).UTC(), Counter: 3, Kind: KindTaskCreated,
		Payload: json.RawMessage(`{"id":"T1.laptop","title":"pin me","site":"","priority":"low"}`)}
	OpHash(op, GenesisHash) // warm the pool
	if n := testing.AllocsPerRun(200, func() { OpHash(op, GenesisHash) }); n > hashAllocCeiling {
		t.Fatalf("OpHash allocated %.0f times per call, want %d or fewer (is the scratch buffer still reused?)", n, hashAllocCeiling)
	}
}

func TestOpHashIsSafeToCallConcurrently(t *testing.T) {
	op := Op{NodeID: "n", Seq: 1, TS: time.Unix(1, 0).UTC(), Kind: KindNoteAdded, Payload: json.RawMessage(`{"a":1}`)}
	want := opHashReference(op, GenesisHash)
	done := make(chan string, 8)
	for g := 0; g < 8; g++ {
		go func() {
			var last string
			for i := 0; i < 500; i++ {
				last = OpHash(op, GenesisHash)
				if last != want {
					break
				}
			}
			done <- last
		}()
	}
	for g := 0; g < 8; g++ {
		if got := <-done; got != want {
			t.Fatalf("concurrent OpHash gave %s, want %s", got, want)
		}
	}
}

var benchOp = Op{NodeID: "laptop", Seq: 7, TS: time.Unix(1700000000, 123456789).UTC(), Counter: 3, Kind: KindTaskCreated,
	Payload: json.RawMessage(`{"id":"T1.laptop","title":"pin me","site":"","priority":"low"}`)}

// BenchmarkOpHash and BenchmarkOpHashReference put the Day 45 version next to the Day 27 one.
func BenchmarkOpHash(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		OpHash(benchOp, GenesisHash)
	}
}

func BenchmarkOpHashReference(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		opHashReference(benchOp, GenesisHash)
	}
}
