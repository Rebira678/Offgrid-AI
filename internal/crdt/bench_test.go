package crdt

import (
	"fmt"
	"testing"

	"offgrid/internal/hlc"
)

func BenchmarkMergeSmall(b *testing.B) {
	st1 := hlc.Timestamp{Wall: 100, Counter: 1}
	st2 := hlc.Timestamp{Wall: 101, Counter: 0}

	m1 := Map{
		TaskKey("T1", FieldTitle):    Entry{Value: "Pump A", Stamp: st1, Node: "node-a"},
		TaskKey("T1", FieldPriority): Entry{Value: "low", Stamp: st1, Node: "node-a"},
	}
	m2 := Map{
		TaskKey("T1", FieldPriority): Entry{Value: "high", Stamp: st2, Node: "node-b"},
		TaskKey("T1", FieldStatus):   Entry{Value: "in_progress", Stamp: st2, Node: "node-b"},
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Merge(m1, m2)
	}
}

func BenchmarkMergeLarge(b *testing.B) {
	const n = 1000
	m1 := make(Map, n)
	m2 := make(Map, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("T%d", i)
		st1 := hlc.Timestamp{Wall: int64(100 + i), Counter: 0}
		st2 := hlc.Timestamp{Wall: int64(100 + i), Counter: 1}
		m1[TaskKey(id, FieldTitle)] = Entry{Value: "Title A", Stamp: st1, Node: "node-a"}
		m2[TaskKey(id, FieldTitle)] = Entry{Value: "Title B", Stamp: st2, Node: "node-b"}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Merge(m1, m2)
	}
}

func BenchmarkPut(b *testing.B) {
	m := Map{}
	key := TaskKey("T1", FieldTitle)
	entry := Entry{Value: "New Title", Stamp: hlc.Timestamp{Wall: 500, Counter: 1}, Node: "node-1"}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.Put(key, entry)
	}
}

func BenchmarkTaskFields(b *testing.B) {
	const n = 100
	m := make(Map, n*4)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("T%d", i)
		st := hlc.Timestamp{Wall: int64(i + 1), Counter: 0}
		m[TaskKey(id, FieldTitle)] = Entry{Value: "Title", Stamp: st, Node: "node-1"}
		m[TaskKey(id, FieldSite)] = Entry{Value: "Site 1", Stamp: st, Node: "node-1"}
		m[TaskKey(id, FieldPriority)] = Entry{Value: "low", Stamp: st, Node: "node-1"}
		m[TaskKey(id, FieldStatus)] = Entry{Value: "open", Stamp: st, Node: "node-1"}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = TaskFields(m)
	}
}

func BenchmarkSplitTaskKey(b *testing.B) {
	key := TaskKey("T12345", FieldStatus)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = SplitTaskKey(key)
	}
}

func BenchmarkMergeAll(b *testing.B) {
	const numReplicas = 5
	const keysPerReplica = 100
	reps := make([]Map, numReplicas)
	for r := 0; r < numReplicas; r++ {
		reps[r] = make(Map, keysPerReplica)
		node := fmt.Sprintf("node-%d", r)
		for k := 0; k < keysPerReplica; k++ {
			id := fmt.Sprintf("T%d", k)
			reps[r][TaskKey(id, FieldPriority)] = Entry{
				Value: "high",
				Stamp: hlc.Timestamp{Wall: int64(100 + r*10 + k), Counter: uint32(r)},
				Node:  node,
			}
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = MergeAll(reps...)
	}
}
