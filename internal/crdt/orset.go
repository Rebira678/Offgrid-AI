package crdt

// An observed-remove set (OR-Set) with tombstones, add-wins.
//
// The problem: Lidia deletes a note while Beka, who has not seen the deletion,
// adds the same note again (or edits the item, which is a fresh add). Last
// writer wins would need a clock to say who was last. An OR-Set needs no
// clock for this: it asks a different question, "what had the remover SEEN?".
//
// Every add is given a unique tag (the node that made it and its hybrid
// logical timestamp, so no two adds ever share one). Removing an element does
// not say "this element is gone". It says "these tags, the ones I saw, are
// gone": it writes a tombstone for each tag it observed. An element is in the
// set while at least one of its tags has no tombstone.
//
// So an add that happened after (or without the knowledge of) the remove has a
// tag the remove never mentioned, and it survives. Add wins. That is the rule
// the post is about: deleting something does not destroy an edit you had not
// seen.
//
// Merge is the union of the adds and the union of the tombstones. Union is
// commutative, associative and idempotent, so replicas that exchange
// everything agree, in any order, with any repetition. A tombstone that arrives
// before the add it refers to still works: membership is computed from both
// sets, never from the order they arrived.
//
// Cost: tombstones are never forgotten here, so the set only grows. Removing
// them safely needs to know that every replica has seen them, which is a
// property the sync layer will have to establish. Not built yet.

import (
	"fmt"
	"sort"

	"offgrid/internal/hlc"
)

// Tag identifies one add. It is unique because a node's HLC never repeats.
type Tag struct {
	Node  string
	Stamp hlc.Timestamp
}

// String is a stable, readable form: "node@2026-10-20T10:00:00Z#3".
func (t Tag) String() string { return fmt.Sprintf("%s@%s", t.Node, t.Stamp) }

func (t Tag) compare(u Tag) int {
	if t.Node != u.Node {
		if t.Node < u.Node {
			return -1
		}
		return 1
	}
	return t.Stamp.Compare(u.Stamp)
}

// Set is an add-wins observed-remove set of string elements. Like Map, it is a
// value: every method returns a new Set and the argument is never changed. The
// zero Set is the empty set.
type Set struct {
	adds    map[Tag]string   // every add ever seen: tag -> element
	removed map[Tag]struct{} // tombstones: tags that were observed and removed
}

func (s Set) clone() Set {
	out := Set{adds: make(map[Tag]string, len(s.adds)), removed: make(map[Tag]struct{}, len(s.removed))}
	for t, e := range s.adds {
		out.adds[t] = e
	}
	for t := range s.removed {
		out.removed[t] = struct{}{}
	}
	return out
}

// Add records that elem was added with the unique tag. Adding the same tag
// again is a no-op (the same add delivered twice).
func (s Set) Add(elem string, tag Tag) Set {
	out := s.clone()
	out.putAdd(tag, elem)
	return out
}

// putAdd keeps the smaller element if one tag is ever seen with two elements.
// That cannot happen when tags are unique; the rule only exists so that even a
// corrupt or forged input merges the same way everywhere.
func (s *Set) putAdd(tag Tag, elem string) {
	if cur, ok := s.adds[tag]; !ok || elem < cur {
		s.adds[tag] = elem
	}
}

// Observed returns the live tags of elem in this set: what a remove made here
// would be able to see. Sorted, so the result is reproducible.
func (s Set) Observed(elem string) []Tag {
	var tags []Tag
	for t, e := range s.adds {
		if e != elem {
			continue
		}
		if _, dead := s.removed[t]; !dead {
			tags = append(tags, t)
		}
	}
	sort.Slice(tags, func(i, j int) bool { return tags[i].compare(tags[j]) < 0 })
	return tags
}

// Remove tombstones every tag of elem that this set currently observes. An
// add of elem made elsewhere and not seen here is untouched. It returns the
// tags it removed, which is what a remove operation must carry in the log so
// that replaying it gives the same result everywhere.
func (s Set) Remove(elem string) (Set, []Tag) {
	seen := s.Observed(elem)
	return s.RemoveTags(seen...), seen
}

// RemoveTags tombstones exactly the given tags, whether or not their adds have
// arrived yet. This is how a remote remove is applied.
func (s Set) RemoveTags(tags ...Tag) Set {
	out := s.clone()
	for _, t := range tags {
		out.removed[t] = struct{}{}
	}
	return out
}

// NewSet preallocates a Set with the given capacities.
func NewSet(addsCap, removedCap int) Set {
	return Set{
		adds:    make(map[Tag]string, addsCap),
		removed: make(map[Tag]struct{}, removedCap),
	}
}

// AddInPlace adds elem with tag directly to the set.
// It is intended for batch construction where the receiver is not shared.
func (s *Set) AddInPlace(elem string, tag Tag) {
	if s.adds == nil {
		s.adds = make(map[Tag]string)
	}
	s.putAdd(tag, elem)
}

// RemoveTagsInPlace tombstones tags directly in the receiver.
// It is intended for batch construction where the receiver is not shared.
func (s *Set) RemoveTagsInPlace(tags ...Tag) {
	if s.removed == nil {
		s.removed = make(map[Tag]struct{}, len(tags))
	}
	for _, t := range tags {
		s.removed[t] = struct{}{}
	}
}

// Has reports whether elem is in the set. It runs in O(N) over adds with
// zero allocations and short-circuits on the first live tag found.
func (s Set) Has(elem string) bool {
	for t, e := range s.adds {
		if e == elem {
			if _, dead := s.removed[t]; !dead {
				return true
			}
		}
	}
	return false
}

// Elements returns the members, sorted, each once.
func (s Set) Elements() []string {
	seen := make(map[string]struct{}, len(s.adds))
	for t, e := range s.adds {
		if _, dead := s.removed[t]; !dead {
			seen[e] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for e := range seen {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}

// Tombstones is how many removed tags are being kept.
func (s Set) Tombstones() int { return len(s.removed) }

// MergeSet is the whole algorithm: the union of the adds and the union of the
// tombstones. Pure: no clock, no I/O, neither argument is changed, the result
// shares nothing with them.
func MergeSet(a, b Set) Set {
	out := Set{
		adds:    make(map[Tag]string, len(a.adds)+len(b.adds)),
		removed: make(map[Tag]struct{}, len(a.removed)+len(b.removed)),
	}
	for t, e := range a.adds {
		out.adds[t] = e
	}
	for t := range a.removed {
		out.removed[t] = struct{}{}
	}
	for t, e := range b.adds {
		out.putAdd(t, e)
	}
	for t := range b.removed {
		out.removed[t] = struct{}{}
	}
	return out
}

// MergeAllSets merges any number of sets into one. If no sets are given,
// the zero Set is returned. Pre-allocates map capacity for all sets in a single pass.
func MergeAllSets(sets ...Set) Set {
	if len(sets) == 0 {
		return Set{}
	}
	if len(sets) == 1 {
		return sets[0].clone()
	}
	var totalAdds, totalRemoved int
	for _, s := range sets {
		totalAdds += len(s.adds)
		totalRemoved += len(s.removed)
	}
	out := Set{
		adds:    make(map[Tag]string, totalAdds),
		removed: make(map[Tag]struct{}, totalRemoved),
	}
	for _, s := range sets {
		for t, e := range s.adds {
			out.putAdd(t, e)
		}
		for t := range s.removed {
			out.removed[t] = struct{}{}
		}
	}
	return out
}

// EqualSet reports whether a and b hold the same adds and the same tombstones.
// (Two sets can have the same Elements and still differ in their tombstones;
// merging them is what makes the difference disappear.)
func EqualSet(a, b Set) bool {
	if len(a.adds) != len(b.adds) || len(a.removed) != len(b.removed) {
		return false
	}
	for t, e := range a.adds {
		if o, ok := b.adds[t]; !ok || o != e {
			return false
		}
	}
	for t := range a.removed {
		if _, ok := b.removed[t]; !ok {
			return false
		}
	}
	return true
}
