package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreBlobCmdCustomNameAndAttachmentsListing(t *testing.T) {
	db := dbPath(t)
	// Create two tasks: T1 and T2
	if code, _, e := runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"Task One"}`); code != 0 {
		t.Fatal(e)
	}
	if code, _, e := runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"Task Two"}`); code != 0 {
		t.Fatal(e)
	}

	// Attach with custom name to T1
	f1 := photoFile(t, "original_file.jpg", 1500)
	code, out, e := runCLI(t, "", "store", "-db", db, "attach", "-task", "T1", "-file", f1, "-name", "custom_photo_name.jpg")
	if code != 0 || !strings.Contains(out, "attached custom_photo_name.jpg") {
		t.Fatalf("attach failed: %d\n%s\n%s", code, out, e)
	}

	// List attachments for T1: should show custom_photo_name.jpg
	code, out, _ = runCLI(t, "", "store", "-db", db, "attachments", "-task", "T1")
	if code != 0 || !strings.Contains(out, "custom_photo_name.jpg") {
		t.Fatalf("attachments T1: %d\n%s", code, out)
	}

	// List attachments for T2 (which has none)
	code, out, _ = runCLI(t, "", "store", "-db", db, "attachments", "-task", "T2")
	if code != 0 || !strings.Contains(out, "no attachments") {
		t.Fatalf("attachments T2 empty: %d\n%s", code, out)
	}
}

func TestStoreGCLiveRemovalAndBlobsSummary(t *testing.T) {
	db := dbPath(t)
	runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"Task for GC"}`)

	// Attach 1 legitimate photo
	runCLI(t, "", "store", "-db", db, "attach", "-task", "T1", "-file", photoFile(t, "valid.jpg", 800))

	// Plant an old orphan
	blobs := db + ".blobs"
	orphanHash := strings.Repeat("cd", 32)
	shard := filepath.Join(blobs, orphanHash[:2])
	if err := os.MkdirAll(shard, 0o700); err != nil {
		t.Fatal(err)
	}
	orphanPath := filepath.Join(shard, orphanHash[2:])
	if err := os.WriteFile(orphanPath, []byte("old orphan data payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-40 * 24 * time.Hour)
	_ = os.Chtimes(orphanPath, oldTime, oldTime)

	// Check store blobs summary before GC: should show 1 orphan
	code, out, _ := runCLI(t, "", "store", "-db", db, "blobs")
	if code != 0 || !strings.Contains(out, "named by no op     1") {
		t.Fatalf("blobs before GC: %d\n%s", code, out)
	}

	// Live GC
	code, out, e := runCLI(t, "", "store", "-db", db, "gc")
	if code != 0 || !strings.Contains(out, "removed 1") {
		t.Fatalf("gc live: %d\n%s\n%s", code, out, e)
	}

	// File should now be gone from disk
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Fatalf("orphan file still exists on disk after live GC")
	}

	// Check store blobs summary after GC: orphan count should now be 0
	code, out, _ = runCLI(t, "", "store", "-db", db, "blobs")
	if code != 0 || !strings.Contains(out, "named by no op     0") {
		t.Fatalf("blobs after GC: %d\n%s", code, out)
	}
}
