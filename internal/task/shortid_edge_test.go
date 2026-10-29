package task

import (
	"reflect"
	"testing"
)

// TestShortIDExhaustiveEdgeCases tests ShortID on all valid, boundary, and malformed inputs.
func TestShortIDExhaustiveEdgeCases(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// Standard Day 24 qualified IDs
		{"T1.laptop", "T1"},
		{"T42.phone", "T42"},
		{"T999999.node-a", "T999999"},
		{"T01.desktop", "T01"},
		{"T100.node_with_underscore", "T100"},
		{"T5.a-b-c-d-e", "T5"},

		// Multi-dot replica names
		{"T12.laptop.worker", "T12"},
		{"T7.node.sub.system", "T7"},

		// Legacy plain IDs from older databases
		{"T1", "T1"},
		{"T42", "T42"},
		{"T9999", "T9999"},

		// Malformed or non-qualified patterns (should return unchanged)
		{"", ""},
		{"T", "T"},
		{"T.", "T."},
		{"T.laptop", "T.laptop"}, // missing number
		{"#T1.laptop", "#T1"},
		{"t1.laptop", "t1"},
		{"1.laptop", "1.laptop"}, // missing T/t
		{"T-1.laptop", "T-1.laptop"}, // hyphen not allowed in digits
		{"T.laptop", "T.laptop"}, // missing digits
		{"Ta.laptop", "Ta.laptop"}, // non-digit
		{"T1x.laptop", "T1x.laptop"}, // mixed alphanumeric
		{"X1.laptop", "X1.laptop"}, // non-T prefix
	}

	for _, tc := range cases {
		if got := ShortID(tc.in); got != tc.want {
			t.Errorf("ShortID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestShortIDsCollisionDisambiguationEdgeCases verifies that ShortIDs preserves
// full IDs when numbers collide across different replicas or legacy databases.
func TestShortIDsCollisionDisambiguationEdgeCases(t *testing.T) {
	// 1. All unique
	tsUnique := []Task{
		{ID: "T1.laptop"},
		{ID: "T2.phone"},
		{ID: "T3.server"},
	}
	wantUnique := []string{"T1", "T2", "T3"}
	if got := ShortIDs(tsUnique); !reflect.DeepEqual(got, wantUnique) {
		t.Errorf("ShortIDs(unique) = %v, want %v", got, wantUnique)
	}

	// 2. Pair collision: T1 on laptop and phone, T2 unique
	tsPairCollision := []Task{
		{ID: "T1.laptop"},
		{ID: "T1.phone"},
		{ID: "T2.laptop"},
	}
	wantPair := []string{"T1.laptop", "T1.phone", "T2"}
	if got := ShortIDs(tsPairCollision); !reflect.DeepEqual(got, wantPair) {
		t.Errorf("ShortIDs(pair collision) = %v, want %v", got, wantPair)
	}

	// 3. Triple collision
	tsTripleCollision := []Task{
		{ID: "T5.node-a"},
		{ID: "T5.node-b"},
		{ID: "T5.node-c"},
	}
	wantTriple := []string{"T5.node-a", "T5.node-b", "T5.node-c"}
	if got := ShortIDs(tsTripleCollision); !reflect.DeepEqual(got, wantTriple) {
		t.Errorf("ShortIDs(triple collision) = %v, want %v", got, wantTriple)
	}

	// 4. Collision between legacy plain ID and Day 24 qualified ID
	tsLegacyCollision := []Task{
		{ID: "T1"},
		{ID: "T1.laptop"},
		{ID: "T2"},
	}
	wantLegacy := []string{"T1", "T1.laptop", "T2"}
	if got := ShortIDs(tsLegacyCollision); !reflect.DeepEqual(got, wantLegacy) {
		t.Errorf("ShortIDs(legacy collision) = %v, want %v", got, wantLegacy)
	}

	// 5. Empty and nil handling
	if got := ShortIDs(nil); len(got) != 0 {
		t.Errorf("ShortIDs(nil) = %v, want empty", got)
	}
	if got := ShortIDs([]Task{}); len(got) != 0 {
		t.Errorf("ShortIDs([]) = %v, want empty", got)
	}
}
