package task

import (
	"errors"
	"testing"
)

func TestQualifiedIDsResolveExactlyAndShortFormsOnlyWhenUnambiguous(t *testing.T) {
	tasks := []Task{
		{ID: "T1", Title: "Old pump"},
		{ID: "T5.n-aaa", Title: "Valve"},
		{ID: "T5.n-bbb", Title: "Filter"},
		{ID: "T6.n-aaa", Title: "Fuel line"},
	}
	for ref, want := range map[string]string{
		"T1": "T1", "t5.n-aaa": "T5.n-aaa", "T5.n-bbb": "T5.n-bbb", "#T6.n-aaa": "T6.n-aaa", "6": "T6.n-aaa", "T6": "T6.n-aaa",
	} {
		got, err := Resolve(tasks, ref)
		if err != nil || got.ID != want {
			t.Errorf("%q -> %q %v, want %q", ref, got.ID, err, want)
		}
	}
	// two tasks share the number 5: the short form must not guess
	_, err := Resolve(tasks, "T5")
	var amb *AmbiguousError
	if !errors.As(err, &amb) || len(amb.Candidates) != 2 {
		t.Errorf("T5 should be ambiguous between two replicas: %v", err)
	}
	for _, ref := range []string{"T5.n-ccc", "T9", "T5.", "T.n-aaa", "T5.bad id"} {
		if got, err := Resolve(tasks, ref); err == nil {
			t.Errorf("%q should not resolve, got %q", ref, got.ID)
		}
	}
	// a qualified ID never falls back to a title search
	if _, err := Resolve([]Task{{ID: "T1", Title: "T5.n-aaa"}}, "T5.n-aaa"); !errors.Is(err, ErrNotFound) {
		t.Errorf("qualified IDs must not match titles: %v", err)
	}
}

func TestResolveEdgeCases(t *testing.T) {
	tasks := []Task{
		{ID: "T1", Title: "Inspect Pump #1", Status: StatusDone},
		{ID: "T2", Title: "Inspect pump #1 (urgent)", Status: StatusOpen},
		{ID: "T10.node-lidia-backup_01", Title: "Solar Inverter Check", Status: StatusOpen},
		{ID: "T11", Title: "Generator Ölwechsel", Status: StatusOpen},
	}

	// 1. Surrounding whitespace and leading '#' on scoped ID
	for _, ref := range []string{"  T10.node-lidia-backup_01  ", "#T10.node-lidia-backup_01", "  #t10.node-lidia-backup_01  "} {
		got, err := Resolve(tasks, ref)
		if err != nil || got.ID != "T10.node-lidia-backup_01" {
			t.Errorf("ref %q: got (%q, %v), want T10.node-lidia-backup_01", ref, got.ID, err)
		}
	}

	// 2. Short forms with various separators
	for _, ref := range []string{"10", "T-10", "T_10", "T 10", "#T-10"} {
		got, err := Resolve(tasks, ref)
		if err != nil || got.ID != "T10.node-lidia-backup_01" {
			t.Errorf("separator ref %q: got (%q, %v), want T10.node-lidia-backup_01", ref, got.ID, err)
		}
	}

	// 3. Unfinished beats finished on partial title match
	got, err := Resolve(tasks, "inspect pump")
	if err != nil || got.ID != "T2" {
		t.Errorf("unfinished preference failed: got (%q, %v), want T2", got.ID, err)
	}

	// 4. Unicode case folding
	got, err = Resolve(tasks, "generator ölwechsel")
	if err != nil || got.ID != "T11" {
		t.Errorf("unicode case fold failed: got (%q, %v), want T11", got.ID, err)
	}

	// 5. Empty and whitespace-only references return ErrNotFound
	for _, bad := range []string{"", "   ", "\t\n"} {
		if _, err := Resolve(tasks, bad); !errors.Is(err, ErrNotFound) {
			t.Errorf("bad ref %q expected ErrNotFound, got %v", bad, err)
		}
	}
}
