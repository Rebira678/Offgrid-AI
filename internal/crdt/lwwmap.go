// Package crdt holds the conflict-free replicated data types Offgrid merges
// with. Day 16 is the first: a last-writer-wins map.
//
// The problem: two replicas edit the same task while apart. Alice sets the
// priority to low, Bob sets it to high. When they meet, both must end up with
// the same value, without asking anyone and without a server deciding. The
// rule has to give the same answer no matter which replica merges, in which
// order, or how many times.
//
// An LWW-Map keeps, for every key, ONE entry: the value, the hybrid logical
// timestamp it was written at, and the node that wrote it. Merging two maps
// keeps, per key, the entry that sorts last. Different keys never interfere,
// so Alice changing the priority and Bob changing the status both survive.
// That is what "per-field" buys over "the whole task, last writer wins", where
// one of them would be thrown away.
//
// Why the merge is safe: entries are ordered by a TOTAL order (stamp, then
// node, then deleted, then value). "Keep the larger of two" under a total
// order is commutative, associative and idempotent, which are the three laws
// a state-based CRDT needs. Day 17 checks them with generated tests.
//
// What it is not: it does not know which edit was "really" last when two were
// concurrent. It picks one the same way everywhere. A losing edit is not an
// error, it is overwritten, exactly as if it had been made a moment earlier.
package crdt

import (
	"sort"
	"strings"

	"offgrid/internal/hlc"
)

// Entry is the winning write for one key.
type Entry struct {
	Value   string
	Deleted bool // a tombstone: the key was removed at Stamp
	Stamp   hlc.Timestamp
	Node    string // who wrote it; breaks ties between equal stamps
}

// Compare is the total order merge uses: -1, 0 or +1 as e sorts before, equal
// to or after o. Stamp first, then the node ID (so two replicas that stamped
// the same instant agree on a winner), then Deleted and Value so that even
// two entries that should never coexist (the same stamp and node, different
// content: a bug or a forged op) still merge the same way everywhere.
func (e Entry) Compare(o Entry) int {
	if c := e.Stamp.Compare(o.Stamp); c != 0 {
		return c
	}
	if c := strings.Compare(e.Node, o.Node); c != 0 {
		return c
	}
	if e.Deleted != o.Deleted {
		if o.Deleted {
			return -1
		}
		return 1
	}
	return strings.Compare(e.Value, o.Value)
}

// Map is a last-writer-wins map from key to Entry. A Map is a value: every
// function here returns a new Map and leaves its arguments untouched. The nil
// Map is a valid empty map.
type Map map[string]Entry

// Put returns a copy of m in which key holds e if e sorts after what m has
// (or m has nothing). Putting an older entry is a no-op, not an error: that is
// what a late-arriving, already-beaten write looks like.
func (m Map) Put(key string, e Entry) Map {
	out := m.clone(1)
	if cur, ok := out[key]; !ok || e.Compare(cur) > 0 {
		out[key] = e
	}
	return out
}

// Delete returns a copy of m with a tombstone for key at the given stamp.
func (m Map) Delete(key string, stamp hlc.Timestamp, node string) Map {
	return m.Put(key, Entry{Deleted: true, Stamp: stamp, Node: node})
}

// Get returns the live value for key. A missing key and a deleted key both
// report ok == false.
func (m Map) Get(key string) (value string, ok bool) {
	e, found := m[key]
	if !found || e.Deleted {
		return "", false
	}
	return e.Value, true
}

// Keys returns every key that has an entry, tombstones included, sorted.
func (m Map) Keys() []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Equal reports whether a and b hold exactly the same entries.
func Equal(a, b Map) bool {
	if len(a) != len(b) {
		return false
	}
	for k, e := range a {
		if o, ok := b[k]; !ok || o != e {
			return false
		}
	}
	return true
}

// Merge is the whole algorithm. It is pure: no clock, no I/O, no randomness,
// no mutation of a or b, and the result shares nothing with them. For every
// key present in either map it keeps the entry that sorts last.
func Merge(a, b Map) Map {
	out := a.clone(len(b))
	for k, e := range b {
		if cur, ok := out[k]; !ok || e.Compare(cur) > 0 {
			out[k] = e
		}
	}
	return out
}

func (m Map) clone(extra int) Map {
	out := make(Map, len(m)+extra)
	for k, e := range m {
		out[k] = e
	}
	return out
}
