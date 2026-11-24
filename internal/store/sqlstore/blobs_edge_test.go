package sqlstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCollectBlobsSharedBlobAcrossMultipleTasks(t *testing.T) {
	ctx := context.Background()
	s, _ := freshStore(t)
	tk1 := mk(t, s, "task-1")
	tk2 := mk(t, s, "task-2")

	// Shared photo
	photo := jpeg(4000, 42)
	att1 := attach(t, s, tk1.ID, photo, "image/jpeg", "shared.jpg")
	att2 := attach(t, s, tk2.ID, photo, "image/jpeg", "shared_copy.jpg")

	if att1.Hash != att2.Hash {
		t.Fatalf("hashes should match: %s vs %s", att1.Hash, att2.Hash)
	}

	age(t, s, att1.Hash, 30*24*time.Hour)

	// Delete task 1
	if err := s.DeleteTask(ctx, tk1.ID); err != nil {
		t.Fatal(err)
	}

	// Dry run: should remove 0
	dry, err := s.CollectBlobs(ctx, 7*24*time.Hour, true)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Removed != 0 {
		t.Fatalf("dry run reported %d removals for a shared blob still referenced by task 2", dry.Removed)
	}

	// Live run: should keep the blob
	res, err := s.CollectBlobs(ctx, 7*24*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 0 {
		t.Fatalf("live run removed shared blob: %+v", res)
	}

	bs, _ := s.Blobs()
	if !bs.Has(att1.Hash) {
		t.Fatal("shared blob was deleted while still referenced by task 2")
	}
}

func TestCollectBlobsContextCancellation(t *testing.T) {
	s, _ := freshStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := s.CollectBlobs(ctx, 7*24*time.Hour, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestCollectBlobsForeignFilesInBlobDir(t *testing.T) {
	ctx := context.Background()
	s, _ := freshStore(t)

	// Add foreign files in blob directory
	foreign1 := filepath.Join(s.blobDir, "invalid-dir")
	_ = os.MkdirAll(foreign1, 0o700)
	_ = os.WriteFile(filepath.Join(foreign1, "foreign.txt"), []byte("foreign"), 0o600)

	foreign2 := filepath.Join(s.blobDir, "xx") // valid shard dir name length, but invalid hash filename inside
	_ = os.MkdirAll(foreign2, 0o700)
	_ = os.WriteFile(filepath.Join(foreign2, "not-a-hex-hash.dat"), []byte("bad"), 0o600)

	// Normal referenced blob
	tk := mk(t, s, "task-real")
	att := attach(t, s, tk.ID, jpeg(2000, 1), "image/jpeg", "real.jpg")

	// CollectBlobs should ignore foreign files and not fail or delete real blob
	res, err := s.CollectBlobs(ctx, 7*24*time.Hour, false)
	if err != nil {
		t.Fatalf("CollectBlobs failed with foreign files: %v", err)
	}
	if res.Removed != 0 {
		t.Fatalf("unexpected removals: %+v", res)
	}

	bs, _ := s.Blobs()
	if !bs.Has(att.Hash) {
		t.Fatal("real blob was erroneously deleted")
	}
}

func TestMissingBlobsLimitAndOrdering(t *testing.T) {
	ctx := context.Background()
	a, _ := replicaAt(t, "node-a", epoch)
	b, _ := replicaAt(t, "node-b", epoch)

	tk := mk(t, a, "pump")
	for i := 0; i < 5; i++ {
		attach(t, a, tk.ID, jpeg(1000+i, byte(i)), "image/jpeg", fmt.Sprintf("p%d.jpg", i))
	}

	// Pull ops to b so b knows about the attachments but lacks the bytes
	pull(t, b, a, 100)

	// MissingBlobs with limit <= 0 returns all missing blobs
	m0, err := b.MissingBlobs(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(m0) != 5 {
		t.Fatalf("expected all 5 for limit 0, got %d", len(m0))
	}

	// MissingBlobs with limit 2
	m2, err := b.MissingBlobs(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(m2) != 2 {
		t.Fatalf("expected 2 for limit 2, got %d", len(m2))
	}

	// MissingBlobs with limit 10 (all 5)
	mAll, err := b.MissingBlobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(mAll) != 5 {
		t.Fatalf("expected 5 for limit 10, got %d", len(mAll))
	}
}

func TestAttachmentsByTaskConcurrency(t *testing.T) {
	ctx := context.Background()
	s, _ := freshStore(t)

	// Seed with 10 tasks and attachments
	for i := 0; i < 10; i++ {
		tk := mk(t, s, fmt.Sprintf("concurrent-task-%d", i))
		attach(t, s, tk.ID, jpeg(1000+i, byte(i)), "image/jpeg", "f.jpg")
	}

	const numReaders = 16
	var wg sync.WaitGroup
	wg.Add(numReaders)

	for g := 0; g < numReaders; g++ {
		go func() {
			defer wg.Done()
			for iter := 0; iter < 15; iter++ {
				m, err := s.AttachmentsByTask(ctx)
				if err != nil {
					t.Errorf("AttachmentsByTask error: %v", err)
					return
				}
				if len(m) < 10 {
					t.Errorf("expected at least 10 tasks with attachments, got %d", len(m))
					return
				}
			}
		}()
	}

	wg.Wait()
}

func TestAttachmentTaskLimitEnforced(t *testing.T) {
	ctx := context.Background()
	s, _ := freshStore(t)
	tk := mk(t, s, "task-with-many-attachments")

	// MaxAttachmentsPerTask = 20
	for i := 0; i < 20; i++ {
		p := jpeg(100+i, byte(i))
		attach(t, s, tk.ID, p, "image/jpeg", fmt.Sprintf("p%d.jpg", i))
	}

	// 21st attachment must be refused
	pExtra := jpeg(500, 99)
	_, err := s.AddAttachment(ctx, tk.ID, bytes.NewReader(pExtra), int64(len(pExtra)), "image/jpeg", "extra.jpg")
	if err == nil || !strings.Contains(err.Error(), "at most 20 attachments") {
		t.Fatalf("expected error containing 'at most 20 attachments', got %v", err)
	}
}

func TestAttachmentNameEdgeCases(t *testing.T) {
	ctx := context.Background()
	s, _ := freshStore(t)
	tk := mk(t, s, "name-edge-cases")
	p := jpeg(100, 1)

	// Valid 120-rune unicode name
	longName := strings.Repeat("日", 120)
	if _, err := s.AddAttachment(ctx, tk.ID, bytes.NewReader(p), int64(len(p)), "image/jpeg", longName); err != nil {
		t.Fatalf("120-rune name failed: %v", err)
	}

	// Invalid 121-rune name
	tooLong := strings.Repeat("a", 121)
	if _, err := s.AddAttachment(ctx, tk.ID, bytes.NewReader(p), int64(len(p)), "image/jpeg", tooLong); err == nil {
		t.Fatal("121-char name should fail")
	}

	// Path separators forbidden
	if _, err := s.AddAttachment(ctx, tk.ID, bytes.NewReader(p), int64(len(p)), "image/jpeg", "sub/dir.jpg"); err == nil {
		t.Fatal("slash should fail")
	}
	if _, err := s.AddAttachment(ctx, tk.ID, bytes.NewReader(p), int64(len(p)), "image/jpeg", "sub\\dir.jpg"); err == nil {
		t.Fatal("backslash should fail")
	}

	// Control characters forbidden
	if _, err := s.AddAttachment(ctx, tk.ID, bytes.NewReader(p), int64(len(p)), "image/jpeg", "bad\nname.jpg"); err == nil {
		t.Fatal("newline should fail")
	}
}

func TestCollectBlobsConcurrentWithAttachmentWrites(t *testing.T) {
	ctx := context.Background()
	s, _ := freshStore(t)
	tk := mk(t, s, "concurrent-writes-and-gc")

	var wg sync.WaitGroup
	const writers = 6
	stop := make(chan struct{})

	// Background writers adding attachments
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(wid int) {
			defer wg.Done()
			i := 0
			for {
				select {
				case <-stop:
					return
				default:
					p := jpeg(200+i%50, byte(wid*10+i%256))
					_, _ = s.AddAttachment(ctx, tk.ID, bytes.NewReader(p), int64(len(p)), "image/jpeg", fmt.Sprintf("w%d_%d.jpg", wid, i))
					i++
					time.Sleep(5 * time.Millisecond)
				}
			}
		}(w)
	}

	// Collector repeatedly runs GC
	for g := 0; g < 4; g++ {
		_, err := s.CollectBlobs(ctx, 7*24*time.Hour, false)
		if err != nil {
			t.Errorf("CollectBlobs error under concurrent writes: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	close(stop)
	wg.Wait()

	// Verify database is completely consistent
	mustVerify(t, s)
}
