package task

import (
	"errors"
	"strings"
	"testing"
)

// TestResolveAmbiguousBetweenMultipleReplicas tests that typing a short ID like "T1"
// when multiple replicas have a T1 returns an AmbiguousError listing the full IDs.
func TestResolveAmbiguousBetweenMultipleReplicas(t *testing.T) {
	ts := []Task{
		{ID: "T1.laptop", Title: "Fix leaking pump", Status: StatusOpen},
		{ID: "T1.phone", Title: "Inspect solar array", Status: StatusOpen},
		{ID: "T2.laptop", Title: "Clean generator filter", Status: StatusOpen},
	}

	// 1. Short ID "T1" matches both T1.laptop and T1.phone
	_, err := Resolve(ts, "T1")
	if err == nil {
		t.Fatal("expected AmbiguousError when resolving T1 across multiple replicas")
	}
	var amb *AmbiguousError
	if !errors.As(err, &amb) {
		t.Fatalf("expected *AmbiguousError, got %v", err)
	}
	if len(amb.Candidates) != 2 {
		t.Fatalf("expected 2 candidates in AmbiguousError, got %d", len(amb.Candidates))
	}
	errStr := amb.Error()
	if !strings.Contains(errStr, "T1.laptop") || !strings.Contains(errStr, "T1.phone") {
		t.Fatalf("ambiguous error message should list full IDs: %s", errStr)
	}

	// 2. Exact qualified ID resolves cleanly without ambiguity
	gotLaptop, err := Resolve(ts, "T1.laptop")
	if err != nil || gotLaptop.ID != "T1.laptop" {
		t.Fatalf("expected direct resolution of T1.laptop, got %+v, %v", gotLaptop, err)
	}

	gotPhone, err := Resolve(ts, "T1.phone")
	if err != nil || gotPhone.ID != "T1.phone" {
		t.Fatalf("expected direct resolution of T1.phone, got %+v, %v", gotPhone, err)
	}

	// 3. Unambiguous short ID "T2" resolves to T2.laptop
	gotT2, err := Resolve(ts, "T2")
	if err != nil || gotT2.ID != "T2.laptop" {
		t.Fatalf("expected direct resolution of T2 to T2.laptop, got %+v, %v", gotT2, err)
	}
}

// TestResolveLegacyAndQualifiedCoexistence verifies that an exact legacy ID ("T1")
// is preferred when present, and qualified prefix fallback occurs when exact match does not exist.
func TestResolveLegacyAndQualifiedCoexistence(t *testing.T) {
	ts := []Task{
		{ID: "T1", Title: "Legacy task", Status: StatusOpen},
		{ID: "T1.node-a", Title: "New modern task", Status: StatusOpen},
	}

	// Typing "T1" finds the exact match "T1"
	tkExact, err := Resolve(ts, "T1")
	if err != nil || tkExact.ID != "T1" {
		t.Fatalf("expected exact match on T1, got %+v, %v", tkExact, err)
	}

	// Typing exact qualified ID "T1.node-a" resolves to the modern task
	tkQualified, err := Resolve(ts, "T1.node-a")
	if err != nil || tkQualified.ID != "T1.node-a" {
		t.Fatalf("expected T1.node-a resolution, got %+v, %v", tkQualified, err)
	}

	// In a set with only the qualified task, typing "T1" resolves to T1.node-a
	tsOnlyQualified := []Task{
		{ID: "T1.node-a", Title: "New modern task", Status: StatusOpen},
	}
	tkFallback, err := Resolve(tsOnlyQualified, "T1")
	if err != nil || tkFallback.ID != "T1.node-a" {
		t.Fatalf("expected fallback to T1.node-a, got %+v, %v", tkFallback, err)
	}
}
