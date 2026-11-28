package webhook

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func TestConcurrentHookPersistence(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hooks.json")

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		idx := i
		go func() {
			defer wg.Done()
			h, err := NewHook(fmt.Sprintf("https://example.com/hook/%d", idx), []string{"task.created"}, false, int64(idx), t0)
			if err != nil {
				t.Errorf("NewHook failed: %v", err)
				return
			}
			f := File{
				Hooks:  []Hook{h},
				Cursor: int64(idx),
			}
			if err := Save(p, f); err != nil {
				t.Errorf("Save failed: %v", err)
			}
			loaded, err := Load(p)
			if err != nil {
				t.Errorf("Load failed: %v", err)
				return
			}
			if len(loaded.Hooks) == 0 {
				t.Errorf("loaded empty hooks file")
			}
		}()
	}
	wg.Wait()

	if runtime.GOOS != "windows" {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("expected file permission 0600, got %o", fi.Mode().Perm())
		}
	}
}

func TestSaveCreatesMissingParentDirectories(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "deep", "nested", "dir")
	p := filepath.Join(dir, "hooks.json")

	h, err := NewHook("https://example.com/h", nil, false, 0, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(p, File{Hooks: []Hook{h}}); err != nil {
		t.Fatalf("Save failed to create parent dirs: %v", err)
	}
	f, err := Load(p)
	if err != nil || len(f.Hooks) != 1 {
		t.Fatalf("Load failed after auto-mkdir: %+v, err: %v", f, err)
	}
}
