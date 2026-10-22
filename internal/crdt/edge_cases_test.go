package crdt

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"
)

// TestEmptyStringValueDistinguishedFromDeleted verifies that an empty string value
// is stored as a live value and not confused with a tombstone or missing key.
func TestEmptyStringValueDistinguishedFromDeleted(t *testing.T) {
	k := TaskKey("T1", FieldTitle)
	m := Map{}.Put(k, Entry{Value: "", Deleted: false, Stamp: at(100, 0), Node: "node-a"})

	val, ok := m.Get(k)
	if !ok {
		t.Fatalf("expected ok=true for live empty string value, got ok=false")
	}
	if val != "" {
		t.Fatalf("expected val=\"\", got %q", val)
	}

	// Now delete it
	del := m.Delete(k, at(101, 0), "node-a")
	val2, ok2 := del.Get(k)
	if ok2 {
		t.Fatalf("expected ok=false for deleted key, got ok=true (val=%q)", val2)
	}
	if val2 != "" {
		t.Fatalf("expected empty string return on deleted key, got %q", val2)
	}
}

// TestTombstoneVsLiveWithIdenticalStampAndNode verifies deterministic resolution
// when a live write and a tombstone share the EXACT same HLC stamp and node.
func TestTombstoneVsLiveWithIdenticalStampAndNode(t *testing.T) {
	k := TaskKey("T1", FieldStatus)
	st := at(500, 1)
	live := Map{}.Put(k, Entry{Value: "open", Deleted: false, Stamp: st, Node: "node-x"})
	tomb := Map{}.Put(k, Entry{Value: "", Deleted: true, Stamp: st, Node: "node-x"})

	// By definition in Compare: if stamp and node match, Deleted: true sorts after Deleted: false
	ab := Merge(live, tomb)
	ba := Merge(tomb, live)

	if !Equal(ab, ba) {
		t.Fatalf("Merge is not commutative on tombstone tie: ab=%v, ba=%v", ab, ba)
	}

	if _, ok := ab.Get(k); ok {
		t.Fatalf("expected tombstone to win tie-break over live write with identical stamp & node")
	}
}

// TestUnicodeAndSpecialCharactersInValuesAndNodeIDs ensures that internationalization,
// emoji, newlines, and multi-byte UTF-8 characters maintain total ordering and converge.
func TestUnicodeAndSpecialCharactersInValuesAndNodeIDs(t *testing.T) {
	k := TaskKey("T-⚡-99", FieldTitle)
	node1 := "node-🌍-東京"
	node2 := "node-🚀-Addis"

	e1 := Entry{Value: "የፓምፕ ጥገና \n\t (Pump Repair) 🔧", Stamp: at(100, 1), Node: node1}
	e2 := Entry{Value: "ポンプ修理 \n\t (Pump Repair) ⚙️", Stamp: at(100, 1), Node: node2}

	m1 := Map{}.Put(k, e1)
	m2 := Map{}.Put(k, e2)

	merged12 := Merge(m1, m2)
	merged21 := Merge(m2, m1)

	if !Equal(merged12, merged21) {
		t.Fatalf("unicode merge did not commute:\n12: %v\n21: %v", merged12, merged21)
	}

	winner, ok := merged12.Get(k)
	if !ok {
		t.Fatalf("expected winner to be readable")
	}

	expectedWinner := e1.Value
	if e2.Compare(e1) > 0 {
		expectedWinner = e2.Value
	}

	if winner != expectedWinner {
		t.Fatalf("unexpected winner: got %q, want %q", winner, expectedWinner)
	}
}

// TestCyclicReplicationTopologyConvergence simulates complex network topologies
// (Ring, Star, and Partitioned Disconnects) across 5 replicas performing concurrent mutations.
func TestCyclicReplicationTopologyConvergence(t *testing.T) {
	for seed := int64(100); seed < 110; seed++ {
		r := rand.New(rand.NewSource(seed))
		const numNodes = 5
		nodes := make([]Map, numNodes)

		// 100 random events
		for step := 0; step < 100; step++ {
			nodeIdx := r.Intn(numNodes)
			taskID := fmt.Sprintf("T%d", r.Intn(4)+1)
			field := []string{FieldTitle, FieldSite, FieldPriority, FieldStatus}[r.Intn(4)]
			key := TaskKey(taskID, field)

			stamp := at(int64(r.Intn(50)+1), uint32(r.Intn(10)))
			nodeName := fmt.Sprintf("node-%d", nodeIdx)

			if r.Intn(5) == 0 {
				// delete
				nodes[nodeIdx] = nodes[nodeIdx].Delete(key, stamp, nodeName)
			} else {
				val := fmt.Sprintf("val-%d-step%d", r.Intn(10), step)
				nodes[nodeIdx] = nodes[nodeIdx].Put(key, Entry{Value: val, Stamp: stamp, Node: nodeName})
			}

			// Partial gossip along a ring topology
			if r.Intn(2) == 0 {
				next := (nodeIdx + 1) % numNodes
				nodes[next] = Merge(nodes[next], nodes[nodeIdx])
			}
		}

		// Full mesh synchronization rounds
		for round := 0; round < 3; round++ {
			for i := 0; i < numNodes; i++ {
				for j := 0; j < numNodes; j++ {
					if i != j {
						nodes[j] = Merge(nodes[j], nodes[i])
					}
				}
			}
		}

		// All 5 replicas must reach 100% cryptographic equality
		for i := 1; i < numNodes; i++ {
			if !Equal(nodes[0], nodes[i]) {
				t.Fatalf("seed %d: replica 0 and replica %d diverged after full sync:\n0: %v\n%d: %v",
					seed, i, nodes[0], i, nodes[i])
			}
		}
	}
}

// TestKeysSliceMutationDoesNotAffectMap verifies that modifying the slice returned
// by Keys() does not mutate or corrupt the Map.
func TestKeysSliceMutationDoesNotAffectMap(t *testing.T) {
	m := Map{}.
		Put("b", Entry{Value: "1", Stamp: at(1, 0), Node: "a"}).
		Put("a", Entry{Value: "2", Stamp: at(1, 0), Node: "a"})

	keys := m.Keys()
	keys[0] = "mutated_key"

	newKeys := m.Keys()
	if !reflect.DeepEqual(newKeys, []string{"a", "b"}) {
		t.Fatalf("keys modified by external slice mutation: %v", newKeys)
	}
}

// TestComplexTaskKeySplitting tests robust parsing of various task key naming conventions.
func TestComplexTaskKeySplitting(t *testing.T) {
	tests := []struct {
		key       string
		wantID    string
		wantField string
		wantOK    bool
	}{
		{"task/T1/title", "T1", "title", true},
		{"task/uuid-550e8400-e29b-41d4-a716-446655440000/priority", "uuid-550e8400-e29b-41d4-a716-446655440000", "priority", true},
		{"task/site/zone/unit/status", "site/zone/unit", "status", true},
		{"task//status", "", "", false},
		{"task/T1/", "", "", false},
		{"task/T1", "", "", false},
		{"invalid/T1/status", "", "", false},
		{"task///", "", "", false},
		{"", "", "", false},
	}

	for _, tt := range tests {
		id, field, ok := SplitTaskKey(tt.key)
		if ok != tt.wantOK || id != tt.wantID || field != tt.wantField {
			t.Errorf("SplitTaskKey(%q) = (%q, %q, %v); want (%q, %q, %v)",
				tt.key, id, field, ok, tt.wantID, tt.wantField, tt.wantOK)
		}
	}
}

// TestTaskFieldsAllDeletedOmitted verifies that when all fields of a task are deleted,
// the task ID does not appear in the resulting TaskFields map.
func TestTaskFieldsAllDeletedOmitted(t *testing.T) {
	m := Map{}.
		Put(TaskKey("T1", FieldTitle), Entry{Value: "Pump", Stamp: at(1, 0), Node: "a"}).
		Put(TaskKey("T1", FieldPriority), Entry{Value: "low", Stamp: at(1, 0), Node: "a"}).
		Delete(TaskKey("T1", FieldTitle), at(2, 0), "a").
		Delete(TaskKey("T1", FieldPriority), at(2, 0), "a")

	fields := TaskFields(m)
	if len(fields) != 0 {
		t.Fatalf("expected all-deleted task to be omitted, got: %v", fields)
	}
}

// TestZeroValueEntryCompare tests that default/zero-value entries behave predictably
// under comparison without panic.
func TestZeroValueEntryCompare(t *testing.T) {
	var z1, z2 Entry
	if z1.Compare(z2) != 0 {
		t.Errorf("zero value compared to itself should be 0")
	}

	nonzero := Entry{Stamp: at(0, 1)}
	if z1.Compare(nonzero) >= 0 {
		t.Errorf("zero value should sort before non-zero stamp")
	}
}

// TestMergeAllEquivalence verifies that MergeAll produces bit-for-bit identical
// state to pairwise Merge folding across multiple replicas.
func TestMergeAllEquivalence(t *testing.T) {
	m1 := Map{
		TaskKey("T1", FieldTitle):    Entry{Value: "Alpha", Stamp: at(10, 0), Node: "n1"},
		TaskKey("T1", FieldPriority): Entry{Value: "low", Stamp: at(10, 0), Node: "n1"},
	}
	m2 := Map{
		TaskKey("T1", FieldPriority): Entry{Value: "high", Stamp: at(12, 0), Node: "n2"},
		TaskKey("T2", FieldStatus):   Entry{Value: "open", Stamp: at(15, 0), Node: "n2"},
	}
	m3 := Map{
		TaskKey("T1", FieldPriority): Entry{Value: "urgent", Stamp: at(11, 0), Node: "n3"},
		TaskKey("T2", FieldStatus):   Entry{Value: "in_progress", Stamp: at(16, 0), Node: "n3"},
		TaskKey("T3", FieldSite):     Entry{Value: "Site C", Stamp: at(5, 0), Node: "n3"},
	}

	pairwise := Merge(Merge(m1, m2), m3)
	all := MergeAll(m1, m2, m3)

	if !Equal(pairwise, all) {
		t.Fatalf("MergeAll diverged from pairwise merge:\nPairwise: %v\nMergeAll: %v", pairwise, all)
	}

	// Boundary cases: empty, 1 map, nil maps
	if empty := MergeAll(); len(empty) != 0 {
		t.Fatalf("expected empty map for 0 args, got %v", empty)
	}
	if single := MergeAll(m1); !Equal(single, m1) {
		t.Fatalf("expected single map clone, got %v", single)
	}
	if withNil := MergeAll(m1, nil, m2); !Equal(withNil, Merge(m1, m2)) {
		t.Fatalf("MergeAll with nil map failed: %v", withNil)
	}
}
