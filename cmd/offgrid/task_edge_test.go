package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestTaskCLIAmbiguousReferenceErrors(t *testing.T) {
	db := filepath.Join(t.TempDir(), "ambig.db")
	for _, js := range []string{
		`{"intent":"create_task","title":"Fix solar panel Alpha","priority":"high"}`,
		`{"intent":"create_task","title":"Fix solar panel Beta","priority":"low"}`,
	} {
		if code, out, _ := runExec(t, "", "-db", db, js); code != 0 {
			t.Fatalf("seeding: %s", out)
		}
	}

	// Delete with ambiguous prefix "Fix solar"
	code, _, errOut := runTask(t, db, "delete", "Fix solar")
	if code == 0 || !strings.Contains(errOut, "matches 2 tasks") {
		t.Fatalf("expected ambiguous error, got code %d, stderr %q", code, errOut)
	}

	// Remove-note with ambiguous prefix
	code, _, errOut = runTask(t, db, "remove-note", "Fix solar", "1")
	if code == 0 || !strings.Contains(errOut, "matches 2 tasks") {
		t.Fatalf("expected ambiguous error on remove-note, got code %d, stderr %q", code, errOut)
	}

	// Disambiguate using full ID works cleanly
	code, out, _ := runTask(t, db, "delete", "T1")
	if code != 0 || !strings.Contains(out, `deleted T1 "Fix solar panel Alpha"`) {
		t.Fatalf("delete T1 failed: %d, %s", code, out)
	}

	// Now prefix "Fix solar" is unambiguous (only Beta left)
	code, out, _ = runTask(t, db, "delete", "Fix solar")
	if code != 0 || !strings.Contains(out, `deleted T2 "Fix solar panel Beta"`) {
		t.Fatalf("delete T2 via prefix failed: %d, %s", code, out)
	}

	// List now says (no tasks)
	code, out, _ = runTask(t, db, "list")
	if code != 0 || !strings.Contains(out, "(no tasks)") {
		t.Fatalf("list after deleting all: %d, %s", code, out)
	}
}

func TestTaskCLINoteBoundsAndEmptyList(t *testing.T) {
	db := filepath.Join(t.TempDir(), "bounds.db")
	if code, out, _ := runExec(t, "", "-db", db, `{"intent":"create_task","title":"Generator inspection","priority":"medium"}`); code != 0 {
		t.Fatalf("seeding: %s", out)
	}

	// Note remove on task with 0 notes
	code, _, errOut := runTask(t, db, "remove-note", "T1", "1")
	if code == 0 || !strings.Contains(errOut, "has no note 1") {
		t.Fatalf("remove non-existent note 1: expected not found error, got code %d, stderr %q", code, errOut)
	}

	// Note remove with negative index
	code, _, errOut = runTask(t, db, "remove-note", "T1", "-1")
	if code == 0 || !strings.Contains(errOut, "notes are numbered from 1") {
		t.Fatalf("remove note -1: expected invalid error, got code %d, stderr %q", code, errOut)
	}

	// Missing db flag
	var o, e strings.Builder
	code = run([]string{"task", "list"}, &o, &e)
	if code != 2 || !strings.Contains(e.String(), "usage:") {
		t.Fatalf("missing -db flag: expected usage error, got code %d, stderr %q", code, e.String())
	}
}

func TestModelRejectionOfAllDestructiveIntents(t *testing.T) {
	db := filepath.Join(t.TempDir(), "safety.db")
	if code, out, _ := runExec(t, "", "-db", db, `{"intent":"create_task","title":"Safety check","priority":"high"}`); code != 0 {
		t.Fatalf("seeding: %s", out)
	}

	forbiddenIntents := []string{
		"delete_task",
		"delete",
		"remove_task",
		"drop_task",
		"purge_task",
		"cancel_task",
		"erase_task",
		"destroy_task",
	}

	for _, intent := range forbiddenIntents {
		payload := `{"intent":"` + intent + `","task_ref":"T1"}`
		code, out, _ := runExec(t, "", "-db", db, payload)
		if code != 0 || !strings.Contains(out, "0 accepted, 1 refused") || !strings.Contains(out, "is not allowed") {
			t.Errorf("intent %q was not strictly refused: code %d, output: %s", intent, code, out)
		}
	}

	// Ensure the task still exists intact
	code, out, _ := runTask(t, db, "list")
	if code != 0 || !strings.Contains(out, "Safety check") {
		t.Fatalf("safety check task was damaged or removed: %s", out)
	}
}
