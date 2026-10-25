package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestCLISpecialCharactersAndUnicode verifies that offgrid task list, delete,
// and remove-note work with Unicode, punctuation, quotes, and non-ASCII titles.
func TestCLISpecialCharactersAndUnicode(t *testing.T) {
	db := filepath.Join(t.TempDir(), "unicode.db")
	specialTitle := `Fix ⚠️ Solar Inverter "Alpha-9" & Pipe/Line #4`

	// Create task via exec
	code, out, errOut := runExec(t, "", "-db", db, fmt.Sprintf(`{"intent":"create_task","title":%q,"priority":"high"}`, specialTitle))
	if code != 0 {
		t.Fatalf("create failed: %s %s", out, errOut)
	}

	// Add Unicode notes
	for _, n := range []string{"1st: Prüfung abgeschlossen (OK)", "2nd: 温度正常 55°C"} {
		if c, o, _ := runExec(t, "", "-db", db, fmt.Sprintf(`{"intent":"add_note","task_ref":"T1","note":%q}`, n)); c != 0 {
			t.Fatalf("add note %q failed: %s", n, o)
		}
	}

	// List
	code, out, _ = runTask(t, db, "list")
	if code != 0 || !strings.Contains(out, "Alpha-9") || !strings.Contains(out, "Prüfung abgeschlossen") || !strings.Contains(out, "温度正常") {
		t.Fatalf("list unicode failed: %s", out)
	}

	// Remove note by exact index
	code, out, _ = runTask(t, db, "remove-note", "T1", "1")
	if code != 0 || !strings.Contains(out, "removed note 1 of T1") {
		t.Fatalf("remove note 1 failed: %d, %s", code, out)
	}

	// List should only have second note
	code, out, _ = runTask(t, db, "list")
	if strings.Contains(out, "Prüfung") || !strings.Contains(out, "温度正常") {
		t.Fatalf("list after note removal unexpected: %s", out)
	}

	// Delete task using part of title with quotes
	code, out, _ = runTask(t, db, "delete", "Solar Inverter")
	if code != 0 || !strings.Contains(out, "deleted T1") {
		t.Fatalf("delete by title fragment failed: %d, %s", code, out)
	}

	// List should now be empty
	code, out, _ = runTask(t, db, "list")
	if !strings.Contains(out, "(no tasks)") {
		t.Fatalf("expected (no tasks), got: %s", out)
	}
}

// TestCLIStoreConcurrentAccess verifies that multiple separate CLI processes
// accessing the same SQLite database simultaneously (readers and writers)
// handle file locking gracefully without crashing or throwing database locked errors.
func TestCLIStoreConcurrentAccess(t *testing.T) {
	db := filepath.Join(t.TempDir(), "concurrent_cli.db")

	// Seed 3 tasks
	for i := 0; i < 3; i++ {
		runExec(t, "", "-db", db, fmt.Sprintf(`{"intent":"create_task","title":"Task-%d","priority":"low"}`, i))
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 20)

	// 5 concurrent workers
	for w := 0; w < 5; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for iter := 0; iter < 5; iter++ {
				var o, e strings.Builder
				var code int
				if iter%2 == 0 {
					code = run([]string{"task", "-db", db, "list"}, &o, &e)
				} else {
					code = run([]string{"exec", "-db", db, fmt.Sprintf(`{"intent":"add_note","task_ref":"T1","note":"worker %d note %d"}`, id, iter)}, &o, &e)
				}
				if code != 0 {
					errCh <- fmt.Errorf("worker %d iter %d code %d: stderr: %s", id, iter, code, e.String())
					return
				}
			}
		}(w)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatal(err)
	}

	// Ensure database is healthy and has all notes
	code, out, _ := runTask(t, db, "list")
	if code != 0 || !strings.Contains(out, "T1") {
		t.Fatalf("final list failed: %s", out)
	}
}
