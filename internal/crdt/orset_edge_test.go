package crdt

import (
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"sync"
	"testing"

	"offgrid/internal/hlc"
)

// TestOrSetAdversarialSameTagDifferentElements verifies deterministic conflict
// resolution when two replicas somehow produce the same tag with different elements.
// The engine must choose deterministically (lexicographically smaller element)
// and preserve commutativity: MergeSet(a, b) == MergeSet(b, a).
func TestOrSetAdversarialSameTagDifferentElements(t *testing.T) {
	tag := Tag{Node: "rogue-node", Stamp: hlc.Timestamp{Wall: 1000, Counter: 1}}
	s1 := Set{}.Add("apple", tag)
	s2 := Set{}.Add("zebra", tag)

	m1 := MergeSet(s1, s2)
	m2 := MergeSet(s2, s1)

	if !EqualSet(m1, m2) {
		t.Fatalf("MergeSet is not commutative on conflicting tag elements: m1=%+v, m2=%+v", m1, m2)
	}

	// Lexicographically smaller ("apple") must be preserved
	elems := m1.Elements()
	if !reflect.DeepEqual(elems, []string{"apple"}) {
		t.Fatalf("expected ['apple'], got %v", elems)
	}
	if !m1.Has("apple") || m1.Has("zebra") {
		t.Fatalf("expected Has('apple')=true and Has('zebra')=false")
	}
}

// TestOrSetTombstoneBeforeAddOutdatedOrder simulates causal inversion:
// tombstones arrive at a replica before the adds they refer to.
func TestOrSetTombstoneBeforeAddOutdatedOrder(t *testing.T) {
	t1 := Tag{Node: "node-a", Stamp: hlc.Timestamp{Wall: 100, Counter: 1}}
	t2 := Tag{Node: "node-a", Stamp: hlc.Timestamp{Wall: 200, Counter: 1}}

	// Replica C receives tombstone for t1 before knowing what t1 is.
	c := Set{}.RemoveTags(t1)
	if c.Has("widget") {
		t.Fatal("c should not have widget")
	}
	if c.Tombstones() != 1 {
		t.Fatalf("expected 1 tombstone, got %d", c.Tombstones())
	}

	// Later, C receives the delayed add for t1
	c = c.Add("widget", t1)
	if c.Has("widget") {
		t.Fatal("widget should remain removed even though add arrived second")
	}
	if len(c.Elements()) != 0 {
		t.Fatalf("expected 0 elements, got %v", c.Elements())
	}

	// Concurrently, node B adds "widget" with a new tag t2
	b := Set{}.Add("widget", t2)

	// C syncs with B: the new tag t2 has no tombstone, so widget resurrects! (Add-wins)
	merged := MergeSet(c, b)
	if !merged.Has("widget") {
		t.Fatal("widget should be live after merging unseen concurrent add")
	}
	if !reflect.DeepEqual(merged.Elements(), []string{"widget"}) {
		t.Fatalf("expected ['widget'], got %v", merged.Elements())
	}
	// Observed tags should only be t2
	obs := merged.Observed("widget")
	if len(obs) != 1 || obs[0] != t2 {
		t.Fatalf("expected observed tags [t2], got %v", obs)
	}
}

// TestOrSetMultipleAddsAndPartialRemoves tests an item added by 10 replicas,
// partially removed by 5, and then fully removed by the rest.
func TestOrSetMultipleAddsAndPartialRemoves(t *testing.T) {
	const n = 10
	var tags []Tag
	for i := 0; i < n; i++ {
		tags = append(tags, Tag{
			Node:  fmt.Sprintf("node-%d", i),
			Stamp: hlc.Timestamp{Wall: int64(1000 + i), Counter: 0},
		})
	}

	// All 10 replicas add "shared-task"
	allAdds := Set{}
	for _, tag := range tags {
		allAdds = allAdds.Add("shared-task", tag)
	}
	if len(allAdds.Observed("shared-task")) != n {
		t.Fatalf("expected %d observed tags, got %d", n, len(allAdds.Observed("shared-task")))
	}

	// Node X only saw the first 5 tags and removes the task
	nodeX := Set{}
	for i := 0; i < 5; i++ {
		nodeX = nodeX.Add("shared-task", tags[i])
	}
	nodeX, removedByX := nodeX.Remove("shared-task")
	if len(removedByX) != 5 {
		t.Fatalf("nodeX should have removed 5 tags, got %d", len(removedByX))
	}

	// Merge allAdds with nodeX
	merged := MergeSet(allAdds, nodeX)
	// Add-wins: tags 5..9 are still live!
	if !merged.Has("shared-task") {
		t.Fatal("shared-task should still be present because tags 5..9 were not seen by nodeX")
	}
	if len(merged.Observed("shared-task")) != 5 {
		t.Fatalf("expected 5 live tags, got %d", len(merged.Observed("shared-task")))
	}

	// Node Y sees the remaining tags and removes the task
	nodeY, removedByY := merged.Remove("shared-task")
	if len(removedByY) != 5 {
		t.Fatalf("nodeY should have removed remaining 5 tags, got %d", len(removedByY))
	}
	if nodeY.Has("shared-task") {
		t.Fatal("shared-task should now be completely removed")
	}
	if nodeY.Tombstones() != 10 {
		t.Fatalf("expected 10 tombstones, got %d", nodeY.Tombstones())
	}
}

// TestOrSetMultiNodeConvergenceMergeAllSets simulates a 7-node cluster
// executing concurrent operations and verifying convergence across
// random pairwise syncs and single-pass MergeAllSets.
func TestOrSetMultiNodeConvergenceMergeAllSets(t *testing.T) {
	const numNodes = 7
	nodes := make([]Set, numNodes)

	// Seed each node with some private and shared items
	for i := 0; i < numNodes; i++ {
		nodeName := fmt.Sprintf("node-%d", i)
		nodes[i] = nodes[i].Add(
			fmt.Sprintf("private-%d", i),
			Tag{Node: nodeName, Stamp: hlc.Timestamp{Wall: int64(100 + i), Counter: 0}},
		)
		nodes[i] = nodes[i].Add(
			"common-item",
			Tag{Node: nodeName, Stamp: hlc.Timestamp{Wall: int64(200 + i), Counter: 0}},
		)
	}

	// Node 0 removes common-item based on what it currently sees
	nodes[0], _ = nodes[0].Remove("common-item")

	// Node 1 removes private-1
	nodes[1], _ = nodes[1].Remove("private-1")

	// Single pass multi-merge
	target := MergeAllSets(nodes...)

	// Pairwise random sync until convergence
	rnd := rand.New(rand.NewSource(42))
	for round := 0; round < 30; round++ {
		i := rnd.Intn(numNodes)
		j := rnd.Intn(numNodes)
		if i != j {
			m := MergeSet(nodes[i], nodes[j])
			nodes[i] = m
			nodes[j] = m
		}
	}

	// Verify all nodes converged to target
	for i, n := range nodes {
		if !EqualSet(n, target) {
			t.Fatalf("node %d failed to converge to target: diff elements: got %v, want %v",
				i, n.Elements(), target.Elements())
		}
	}

	// common-item should still exist because nodes 1..6 added it concurrently
	if !target.Has("common-item") {
		t.Fatal("common-item should be retained due to concurrent adds on nodes 1..6")
	}
	// private-1 was only on node 1, and node 1 removed it before syncing
	if target.Has("private-1") {
		t.Fatal("private-1 should be dead across all replicas")
	}
}

// TestNoteElementBoundaryAndUnicodeEdgeCases tests NoteElement and SplitNoteElement
// with extreme inputs: multiline text, embedded newlines, markdown fences,
// unicode emojis, null bytes, max int64 timestamps, and malformed inputs.
func TestNoteElementBoundaryAndUnicodeEdgeCases(t *testing.T) {
	testCases := []struct {
		name   string
		taskID string
		tag    Tag
		text   string
	}{
		{
			name:   "empty text",
			taskID: "T1",
			tag:    Tag{Node: "n1", Stamp: hlc.Timestamp{Wall: 1, Counter: 0}},
			text:   "",
		},
		{
			name:   "multiline with blank lines",
			taskID: "T100",
			tag:    Tag{Node: "n2", Stamp: hlc.Timestamp{Wall: 100, Counter: 5}},
			text:   "Line 1\n\nLine 3\n\n\nLine 6",
		},
		{
			name:   "text looks like another note header",
			taskID: "T2",
			tag:    Tag{Node: "n3", Stamp: hlc.Timestamp{Wall: 50, Counter: 2}},
			text:   "note/T999/00000000000000000001.0000000001.node\nNested content",
		},
		{
			name:   "unicode runes, emojis, null byte, and RTL text",
			taskID: "task-🚀-123",
			tag:    Tag{Node: "node-αβγ", Stamp: hlc.Timestamp{Wall: 999999, Counter: 42}},
			text:   "Hello 🌍! \x00 Null byte test. שלום עולם. مرحبا بالعالم. 🤖🛠️",
		},
		{
			name:   "max timestamp and max counter boundary",
			taskID: "MAX-TASK",
			tag:    Tag{Node: "super-node", Stamp: hlc.Timestamp{Wall: math.MaxInt64, Counter: math.MaxUint32}},
			text:   "Edge of universe",
		},
		{
			name:   "zero timestamp and zero counter",
			taskID: "ZERO-TASK",
			tag:    Tag{Node: "zero-node", Stamp: hlc.Timestamp{Wall: 0, Counter: 0}},
			text:   "Beginning of time",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			encoded := NoteElement(tc.taskID, tc.tag, tc.text)
			taskID, tag, text, ok := SplitNoteElement(encoded)
			if !ok {
				t.Fatalf("SplitNoteElement failed for %q", encoded)
			}
			if taskID != tc.taskID {
				t.Errorf("taskID: got %q, want %q", taskID, tc.taskID)
			}
			if tag != tc.tag {
				t.Errorf("tag: got %+v, want %+v", tag, tc.tag)
			}
			if text != tc.text {
				t.Errorf("text: got %q, want %q", text, tc.text)
			}
		})
	}
}

// TestSplitNoteElementMalformedInputs verifies that invalid or corrupt strings
// never panic and cleanly return ok = false.
func TestSplitNoteElementMalformedInputs(t *testing.T) {
	badInputs := []string{
		"",
		"not-a-note",
		"note/",
		"note/T1",
		"note/T1\ntext",
		"note/T1/123\ntext",
		"note/T1/00000000000000000001.0000000001.node",       // missing newline
		"note//00000000000000000001.0000000001.node\ntext",   // empty task ID
		"note/T1/00000000000000000001_0000000001_node\ntext", // bad separator
		"note/T1/notanumber0000000001.0000000001.node\ntext", // non-digit wall
		"note/T1/00000000000000000001.notanumber.node\ntext", // non-digit counter
	}

	for _, bad := range badInputs {
		_, _, _, ok := SplitNoteElement(bad)
		if ok {
			t.Errorf("expected ok=false for bad input: %q", bad)
		}
	}
}

// TestOrSetConcurrentReadsAndWrites verifies that Set is completely data-race
// free when read concurrently by multiple goroutines while new sets are derived.
func TestOrSetConcurrentReadsAndWrites(t *testing.T) {
	base := Set{}
	for i := 0; i < 50; i++ {
		base = base.Add(
			fmt.Sprintf("item-%d", i),
			Tag{Node: "base", Stamp: hlc.Timestamp{Wall: int64(i), Counter: 0}},
		)
	}

	var wg sync.WaitGroup
	const goroutines = 16

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			s := base
			for step := 0; step < 100; step++ {
				// Read operations
				_ = s.Has(fmt.Sprintf("item-%d", step%50))
				_ = s.Elements()
				_ = s.Tombstones()
				_ = s.Observed(fmt.Sprintf("item-%d", step%50))

				// Derive new set
				tag := Tag{Node: fmt.Sprintf("worker-%d", id), Stamp: hlc.Timestamp{Wall: int64(step), Counter: uint32(id)}}
				s = s.Add(fmt.Sprintf("new-%d-%d", id, step), tag)
				if step%5 == 0 {
					s, _ = s.Remove(fmt.Sprintf("item-%d", step%50))
				}
			}
		}(g)
	}

	wg.Wait()
}

// BenchmarkOrSetOperations benchmarks Set operations (Has, Add, Elements, MergeSet, MergeAllSets).
func BenchmarkOrSetOperations(t *testing.B) {
	tagGen := func(i int) Tag {
		return Tag{Node: "bench-node", Stamp: hlc.Timestamp{Wall: int64(i), Counter: 0}}
	}

	t.Run("Has/1000", func(b *testing.B) {
		s := Set{}
		for i := 0; i < 1000; i++ {
			s = s.Add(fmt.Sprintf("elem-%d", i), tagGen(i))
		}
		b.ResetTimer()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if !s.Has("elem-500") {
				b.Fatal("missing elem-500")
			}
		}
	})

	t.Run("Elements/1000", func(b *testing.B) {
		s := Set{}
		for i := 0; i < 1000; i++ {
			s = s.Add(fmt.Sprintf("elem-%d", i), tagGen(i))
		}
		b.ResetTimer()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			elems := s.Elements()
			if len(elems) != 1000 {
				b.Fatalf("expected 1000, got %d", len(elems))
			}
		}
	})

	t.Run("MergeSet/1000", func(b *testing.B) {
		s1 := Set{}
		s2 := Set{}
		for i := 0; i < 1000; i++ {
			s1 = s1.Add(fmt.Sprintf("elem-%d", i), Tag{Node: "node-1", Stamp: hlc.Timestamp{Wall: int64(i)}})
			s2 = s2.Add(fmt.Sprintf("elem-%d", i+500), Tag{Node: "node-2", Stamp: hlc.Timestamp{Wall: int64(i)}})
		}
		b.ResetTimer()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			m := MergeSet(s1, s2)
			if len(m.adds) == 0 {
				b.Fatal("empty merge")
			}
		}
	})

	t.Run("MergeAllSets/5x500", func(b *testing.B) {
		sets := make([]Set, 5)
		for s := 0; s < 5; s++ {
			for i := 0; i < 500; i++ {
				sets[s] = sets[s].Add(fmt.Sprintf("elem-%d", i), Tag{Node: fmt.Sprintf("node-%d", s), Stamp: hlc.Timestamp{Wall: int64(i)}})
			}
		}
		b.ResetTimer()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			m := MergeAllSets(sets...)
			if len(m.adds) == 0 {
				b.Fatal("empty merge")
			}
		}
	})
}
