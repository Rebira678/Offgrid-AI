package sqlstore

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Day 34: the op decoder is the first thing a stranger's bytes reach once a
// request is authenticated. Whatever arrives through ApplyRemote must (1) not
// panic, (2) fail only with a typed error a caller can map to "your data is
// unacceptable", never an untyped internal failure, (3) be all-or-nothing, and
// (4) leave a store whose hash chain and view still verify.
var fuzzKinds = []string{
	KindTaskCreated, KindTaskUpdated, KindNoteAdded, KindTaskRemoved, KindNoteRemoved,
	KindAnswerRecorded, KindSuggestionMade, KindSuggestionResolved, "task.unknown", "",
}

var fuzzSeedPayloads = []string{
	`{"id":"T1","title":"a","site":"","priority":"medium"}`,
	`{"id":"T2","title":"b","site":"4","priority":"high"}`,
	`{"id":"T1","status":"done"}`,
	`{"id":"T1","priority":"low"}`,
	`{"id":"T1","text":"a note"}`,
	`{"id":"T1","seen":{"peer":1}}`,
	`{"id":"T1","node":"peer","seq":2}`,
	`{"key":"k","question":"q","text":"t","model":"m","source":"local"}`,
	`{"task":"T1","status":"done","base_status":"open","base_priority":"medium","by":"model"}`,
	`{"node":"peer","seq":2,"resolution":"accepted"}`,
	`{}`, `null`, `[]`, `"x"`, `{"id":1}`, `{"id":"T1","extra":true}`, `{"id":"T1","title":"a"} {}`,
	`{"id":"` + string(make([]byte, 10)) + `"}`,
}

func FuzzApplyRemoteOp(f *testing.F) {
	for k := range fuzzKinds {
		for _, p := range fuzzSeedPayloads {
			f.Add(uint8(k), uint8(0), uint8(2), int32(1), uint32(0), []byte(p))
		}
	}
	f.Add(uint8(0), uint8(0), uint8(1), int32(0), uint32(0), []byte(fuzzSeedPayloads[0])) // a duplicate of the setup op
	f.Add(uint8(0), uint8(0), uint8(1), int32(0), uint32(0), []byte(fuzzSeedPayloads[1])) // a fork of it
	f.Add(uint8(0), uint8(1), uint8(1), int32(0), uint32(7), []byte(fuzzSeedPayloads[0])) // another node, same task ID
	f.Add(uint8(0), uint8(0), uint8(9), int32(0), uint32(0), []byte(fuzzSeedPayloads[0])) // a gap
	f.Add(uint8(1), uint8(0), uint8(2), int32(-1<<30), uint32(0), []byte(fuzzSeedPayloads[2]))
	f.Add(uint8(1), uint8(0), uint8(2), int32(1<<30), uint32(0xffffffff), []byte(fuzzSeedPayloads[2]))

	base := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, kind, node, seq uint8, tsOff int32, counter uint32, payload []byte) {
		ctx := context.Background()
		s, err := Open(ctx, ":memory:", Options{NodeID: "local", Now: func() time.Time { return base }})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		setup := Op{NodeID: "peer", Seq: 1, TS: base, Kind: KindTaskCreated, Payload: []byte(fuzzSeedPayloads[0])}
		if _, err := s.ApplyRemote(ctx, []Op{setup}); err != nil {
			t.Fatalf("setup op refused: %v", err)
		}
		before, err := s.Summary(ctx)
		if err != nil {
			t.Fatal(err)
		}
		nodes := []string{"peer", "other", "bad node!", ""}
		op := Op{
			NodeID: nodes[int(node)%len(nodes)], Seq: int64(seq), TS: base.Add(time.Duration(tsOff) * time.Second),
			Counter: counter, Kind: fuzzKinds[int(kind)%len(fuzzKinds)], Payload: payload,
		}
		res, err := s.ApplyRemote(ctx, []Op{op})
		if err != nil {
			typed := errors.Is(err, ErrBadOp) || errors.Is(err, ErrGap) || errors.Is(err, ErrFork) ||
				errors.Is(err, ErrChain) || errors.Is(err, ErrIDCollision) || errors.Is(err, ErrClockDrift) ||
				errors.Is(err, ErrBadSignature) || errors.Is(err, ErrKeyChange) || errors.Is(err, ErrUnsigned) ||
				errors.Is(err, ErrNotAuthorized)
			if !typed {
				t.Fatalf("an op from a peer failed with an error that is not one of the typed refusals (the server would call it an internal error): %v\nop: %+v", err, op)
			}
			after, _ := s.Summary(ctx)
			if !sameSummary(before, after) {
				t.Fatalf("a refused op changed the store: %v -> %v (err %v)", before, after, err)
			}
		} else if res.Applied+res.Duplicates != 1 {
			t.Fatalf("one op in, but applied=%d duplicates=%d", res.Applied, res.Duplicates)
		}
		if _, err := s.Verify(ctx); err != nil {
			t.Fatalf("the store no longer verifies after op %+v: %v", op, err)
		}
	})
}

func sameSummary(a, b Summary) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
