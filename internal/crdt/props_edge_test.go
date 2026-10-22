package crdt

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"offgrid/internal/hlc"
)

// genExtremeStamp generates HLC timestamps at the edges of representation:
// zero, min/max wall time, boundary counters, and standard values.
func genExtremeStamp() *rapid.Generator[hlc.Timestamp] {
	return rapid.Custom(func(t *rapid.T) hlc.Timestamp {
		wallChoices := []int64{
			0,
			1,
			1700000000000000000, // typical 2023-2026 epoch ns
			math.MaxInt64 - 1,
			math.MaxInt64,
		}
		counterChoices := []uint32{
			0,
			1,
			math.MaxUint32 - 1,
			math.MaxUint32,
		}
		wall := rapid.OneOf(
			rapid.SampledFrom(wallChoices),
			rapid.Int64Range(0, 100),
		).Draw(t, "wall")

		counter := rapid.OneOf(
			rapid.SampledFrom(counterChoices),
			rapid.Uint32Range(0, 50),
		).Draw(t, "counter")

		return hlc.Timestamp{Wall: wall, Counter: counter}
	})
}

// genExtremeEntry generates CRDT entries with extreme stamps, Unicode / binary strings,
// empty values, and adversarial node names.
func genExtremeEntry() *rapid.Generator[Entry] {
	return rapid.Custom(func(t *rapid.T) Entry {
		valChoices := []string{
			"",
			"simple",
			"የፓምፕ ጥገና \n\t (Pump Repair) 🔧",
			"ポンプ修理 ⚙️",
			"null\x00byte\x00inside",
			strings.Repeat("A", 256),
		}
		nodeChoices := []string{
			"",
			"node-a",
			"node-b",
			"node-🌍-東京",
			strings.Repeat("N", 64),
		}

		return Entry{
			Value:   rapid.SampledFrom(valChoices).Draw(t, "value"),
			Deleted: rapid.Bool().Draw(t, "deleted"),
			Stamp:   genExtremeStamp().Draw(t, "stamp"),
			Node:    rapid.SampledFrom(nodeChoices).Draw(t, "node"),
		}
	})
}

// genAdversarialMap generates maps with diverse keys: valid task keys,
// malformed keys, unicode keys, and deep slashes.
func genAdversarialMap() *rapid.Generator[Map] {
	return rapid.Custom(func(t *rapid.T) Map {
		keyChoices := []string{
			TaskKey("T1", FieldTitle),
			TaskKey("T1", FieldStatus),
			TaskKey("T2", FieldSite),
			TaskKey("T2", FieldPriority),
			TaskKey("T-⚡-99", FieldTitle),
			TaskKey("a/b/c", FieldStatus),
			"not-a-task-key",
			"task//title",
			"task/T1/",
			"",
		}
		n := rapid.IntRange(0, 8).Draw(t, "numEntries")
		m := Map{}
		for i := 0; i < n; i++ {
			k := rapid.SampledFrom(keyChoices).Draw(t, "key")
			m[k] = genExtremeEntry().Draw(t, "entry")
		}
		return m
	})
}

// TestPropertyMergeLawsHoldUnderExtremeValues proves the semilattice properties
// (commutative, associative, idempotent) remain unbroken under extreme timestamp walls,
// counter boundaries, unicode, and control characters.
func TestPropertyMergeLawsHoldUnderExtremeValues(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		a := genAdversarialMap().Draw(rt, "mapA")
		b := genAdversarialMap().Draw(rt, "mapB")
		c := genAdversarialMap().Draw(rt, "mapC")

		// Commutative: a ∨ b == b ∨ a
		ab := Merge(a, b)
		ba := Merge(b, a)
		if !Equal(ab, ba) {
			rt.Fatalf("extreme commutative failed:\n ab: %v\n ba: %v", ab, ba)
		}

		// Associative: (a ∨ b) ∨ c == a ∨ (b ∨ c)
		ab_c := Merge(ab, c)
		a_bc := Merge(a, Merge(b, c))
		if !Equal(ab_c, a_bc) {
			rt.Fatalf("extreme associative failed:\n (a*b)*c: %v\n a*(b*c): %v", ab_c, a_bc)
		}

		// Idempotent: a ∨ a == a
		aa := Merge(a, a)
		if !Equal(aa, a) {
			rt.Fatalf("extreme idempotent failed:\n aa: %v\n a: %v", aa, a)
		}
	})
}

// TestPropertyTombstoneResurrectionAndOverwrites verifies the exact contract of
// tombstone overwrites and resurrecting edits across all generated permutations.
func TestPropertyTombstoneResurrectionAndOverwrites(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		key := TaskKey("T1", FieldStatus)
		st1 := genExtremeStamp().Draw(rt, "stamp1")
		st2 := genExtremeStamp().Draw(rt, "stamp2")
		node1 := rapid.SampledFrom([]string{"node-1", "node-2"}).Draw(rt, "node1")
		node2 := rapid.SampledFrom([]string{"node-1", "node-2"}).Draw(rt, "node2")

		e1 := Entry{Value: "open", Deleted: false, Stamp: st1, Node: node1}
		e2 := Entry{Value: "", Deleted: true, Stamp: st2, Node: node2}

		// Put both into a map in both orders and via merge
		m1 := Map{}.Put(key, e1).Put(key, e2)
		m2 := Map{}.Put(key, e2).Put(key, e1)
		mMerged := Merge(Map{key: e1}, Map{key: e2})

		if !Equal(m1, m2) || !Equal(m1, mMerged) {
			rt.Fatalf("order of write arrival diverged:\n m1: %v\n m2: %v\n mMerged: %v", m1, m2, mMerged)
		}

		// Determine expected winner according to total order
		winner := e1
		if e2.Compare(e1) > 0 {
			winner = e2
		}

		got := m1[key]
		if got != winner {
			rt.Fatalf("expected winner %v, got %v", winner, got)
		}

		val, ok := m1.Get(key)
		if winner.Deleted {
			if ok || val != "" {
				rt.Fatalf("deleted winner must report ok=false and val=\"\", got ok=%v val=%q", ok, val)
			}
		} else {
			if !ok || val != winner.Value {
				rt.Fatalf("live winner must report ok=true and val=%q, got ok=%v val=%q", winner.Value, ok, val)
			}
		}
	})
}

// TestPropertyTaskFieldsConsistentWithMapGet proves that TaskFields(m) extracts
// exactly the live fields reachable via m.Get and omits all tombstones, invalid keys,
// and all-deleted tasks.
func TestPropertyTaskFieldsConsistentWithMapGet(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		m := genAdversarialMap().Draw(rt, "map")
		fields := TaskFields(m)

		// 1. Every field in fields[id][field] must match m.Get(TaskKey(id, field))
		for id, taskMap := range fields {
			if len(taskMap) == 0 {
				rt.Fatalf("task %q has an empty field map; should have been omitted", id)
			}
			for fName, fVal := range taskMap {
				k := TaskKey(id, fName)
				gotVal, ok := m.Get(k)
				if !ok {
					rt.Fatalf("TaskFields has field %q for task %q, but m.Get(%q) reports ok=false", fName, id, k)
				}
				if gotVal != fVal {
					rt.Fatalf("value mismatch for %q: TaskFields=%q, m.Get=%q", k, fVal, gotVal)
				}
			}
		}

		// 2. For every key in m, verify consistency with TaskFields
		for k := range m {
			id, fName, ok := SplitTaskKey(k)
			liveVal, isLive := m.Get(k)
			if !ok || !isLive {
				// Must NOT be in TaskFields
				if taskMap, exists := fields[id]; exists {
					if _, fieldExists := taskMap[fName]; fieldExists {
						rt.Fatalf("key %q (ok=%v, isLive=%v) should not be present in TaskFields, but got %q",
							k, ok, isLive, taskMap[fName])
					}
				}
			} else {
				// Must be in TaskFields
				taskMap, exists := fields[id]
				if !exists {
					rt.Fatalf("live key %q not found in TaskFields (task %q missing)", k, id)
				}
				if taskMap[fName] != liveVal {
					rt.Fatalf("live key %q in TaskFields has value %q, expected %q", k, taskMap[fName], liveVal)
				}
			}
		}
	})
}

// TestPropertyCompareTotalOrderStrictTransitivity verifies the mathematical
// strict weak/total order properties of Entry.Compare across generated extreme entries.
func TestPropertyCompareTotalOrderStrictTransitivity(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		e1 := genExtremeEntry().Draw(rt, "e1")
		e2 := genExtremeEntry().Draw(rt, "e2")
		e3 := genExtremeEntry().Draw(rt, "e3")

		// Reflexivity
		if e1.Compare(e1) != 0 {
			rt.Fatalf("reflexivity violated: %v.Compare itself != 0", e1)
		}

		// Antisymmetry
		c12 := e1.Compare(e2)
		c21 := e2.Compare(e1)
		if c12 != -c21 {
			rt.Fatalf("antisymmetry violated: %v vs %v (c12=%d, c21=%d)", e1, e2, c12, c21)
		}

		// Transitivity: if e1 <= e2 and e2 <= e3, then e1 <= e3
		c23 := e2.Compare(e3)
		c13 := e1.Compare(e3)
		if c12 <= 0 && c23 <= 0 && c13 > 0 {
			rt.Fatalf("transitivity violated: e1 <= e2 (%d) and e2 <= e3 (%d) but e1 > e3 (%d)", c12, c23, c13)
		}
	})
}

// TestPropertyMergeAllEquivalenceToPairwiseFolding proves that MergeAll over N generated maps
// matches sequential pairwise Merge folding under all permutations.
func TestPropertyMergeAllEquivalenceToPairwiseFolding(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		k := rapid.IntRange(0, 6).Draw(rt, "numMaps")
		maps := make([]Map, k)
		for i := 0; i < k; i++ {
			maps[i] = genAdversarialMap().Draw(rt, fmt.Sprintf("map-%d", i))
		}

		mergedAll := MergeAll(maps...)

		// Fold sequentially
		var folded Map
		for _, m := range maps {
			folded = Merge(folded, m)
		}

		if !Equal(mergedAll, folded) {
			rt.Fatalf("MergeAll diverged from sequential pairwise folding:\n MergeAll: %v\n Folded:   %v", mergedAll, folded)
		}
	})
}
